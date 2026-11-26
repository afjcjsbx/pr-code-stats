// Command pr-code-stats is the entrypoint of the PR Code Stats GitHub Action.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"

	"github.com/afjcjsbx/pr-code-stats/internal/analyzer"
	"github.com/afjcjsbx/pr-code-stats/internal/config"
	"github.com/afjcjsbx/pr-code-stats/internal/github"
	"github.com/afjcjsbx/pr-code-stats/internal/report"
)

// Exit codes.
const (
	exitOK       = 0
	exitFailure  = 1 // analysis failed, or the comment failed and fail-on-comment-error is set
	exitBadInput = 2 // invalid inputs, context or configuration
)

const (
	statusSkipped  = "skipped"
	statusDisabled = "disabled"
)

func main() {
	if analyzer.RunWorkerIfRequested() { // same binary doubles as the sandboxed lexer worker
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Getenv, os.Stdout)
	stop()
	os.Exit(code)
}

type inputs struct {
	token, baseSHA, headSHA, configPath  string
	comment, failOnCommentError, summary bool

	repo, apiURL, projectURL string
	allowHTTP                bool
	pr                       int
	client                   *github.Client
}

// annotate writes a workflow command. Messages may contain untrusted text (file names), so
// the characters that could end the command and start another one are escaped.
func annotate(w io.Writer, level, msg string) {
	msg = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(msg)
	fmt.Fprintf(w, "::%s::%s\n", level, msg)
}

func run(ctx context.Context, getenv func(string) string, out io.Writer) int {
	in, problems := readInputs(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			annotate(out, "error", p)
		}
		return exitBadInput
	}

	repo := analyzer.Repo{Dir: workspace(getenv)}
	if err := repo.VerifyCommit(ctx, "base", in.baseSHA); err != nil {
		annotate(out, "error", err.Error())
		return exitFailure
	}
	// Security: rules come from the base branch, so a PR cannot change how it is measured.
	cfg, err := loadConfig(ctx, repo, in.baseSHA, in.configPath)
	if err != nil {
		annotate(out, "error", err.Error())
		return exitBadInput
	}

	res, err := analyzer.Analyze(ctx, repo, in.baseSHA, in.headSHA, cfg)
	if err != nil {
		annotate(out, "error", err.Error())
		return exitFailure
	}
	for _, w := range res.Warnings {
		annotate(out, "warning", w)
	}
	md := report.Markdown(report.Data{PRNumber: in.pr, Result: res, ProjectURL: in.projectURL})
	fmt.Fprintln(out, md)

	exit := exitOK
	status, commentURL := statusDisabled, ""
	if in.comment {
		var cerr error
		status, commentURL, cerr = publish(ctx, in, md, out)
		if cerr != nil {
			if in.failOnCommentError {
				annotate(out, "error", cerr.Error())
				exit = exitFailure
			} else {
				annotate(out, "warning", cerr.Error())
			}
		}
	}

	tot := res.Stats.Total()
	outputs := [][2]string{
		{"added", strconv.Itoa(tot.Added)},
		{"removed", strconv.Itoa(tot.Removed)},
		{"net", strconv.Itoa(res.Stats.Net())},
		{"files-changed", strconv.Itoa(res.FilesChanged)},
		{"comment-url", commentURL},
		{"comment-status", status},
	}
	if err := writeOutputs(getenv("GITHUB_OUTPUT"), outputs); err != nil {
		annotate(out, "error", "writing outputs: "+err.Error())
		exit = exitFailure
	}
	if in.summary {
		if err := appendFile(getenv("GITHUB_STEP_SUMMARY"), md+"\n"); err != nil {
			annotate(out, "warning", "writing step summary: "+err.Error())
		}
	}
	return exit
}

// publish creates or updates the report comment. A failure never loses the analysis: it is
// reported as status "skipped" and the caller decides whether it is fatal.
func publish(ctx context.Context, in *inputs, md string, out io.Writer) (status, url string, err error) {
	outcome, url, notes, err := in.client.Upsert(ctx, in.repo, in.pr, report.Marker, md)
	for _, n := range notes {
		annotate(out, "warning", n)
	}
	if err == nil {
		return outcome, url, nil
	}
	var ae *github.APIError
	if errors.As(err, &ae) && ae.Status == 403 && !ae.RateLimited {
		return statusSkipped, "", fmt.Errorf("comment skipped: the token may not write comments (%w). "+
			"This is expected for pull requests from forks, where GITHUB_TOKEN is read-only; "+
			"otherwise grant `pull-requests: write` to the job", err)
	}
	return statusSkipped, "", fmt.Errorf("comment skipped: %w", err)
}

func input(getenv func(string) string, name string) string {
	return strings.TrimSpace(getenv("INPUT_" + strings.ToUpper(name)))
}

func boolInput(getenv func(string) string, name string, def bool, problems *[]string) bool {
	v := strings.ToLower(input(getenv, name))
	switch v {
	case "":
		return def
	case "true":
		return true
	case "false":
		return false
	}
	*problems = append(*problems, fmt.Sprintf("input %q must be true or false, got %q", name, v))
	return def
}

// readInputs validates everything up front and reports every problem at once.
func readInputs(getenv func(string) string) (*inputs, []string) {
	var problems []string
	in := &inputs{
		token:      input(getenv, "github-token"),
		baseSHA:    input(getenv, "base-sha"),
		headSHA:    input(getenv, "head-sha"),
		configPath: input(getenv, "config"),

		repo:       getenv("GITHUB_REPOSITORY"),
		apiURL:     getenv("GITHUB_API_URL"),
		projectURL: projectURL(getenv),
		allowHTTP:  getenv("PR_CODE_STATS_ALLOW_HTTP") == "1", // http-only GitHub Enterprise Server, tests
		pr:         prNumber(getenv),
	}
	in.comment = boolInput(getenv, "comment", true, &problems)
	in.failOnCommentError = boolInput(getenv, "fail-on-comment-error", false, &problems)
	in.summary = boolInput(getenv, "write-step-summary", true, &problems)
	if in.configPath == "" {
		in.configPath = ".github/pr-code-stats.yml"
	}
	in.configPath = path.Clean(strings.ReplaceAll(in.configPath, `\`, "/"))
	if path.IsAbs(in.configPath) || in.configPath == ".." || strings.HasPrefix(in.configPath, "../") {
		problems = append(problems, fmt.Sprintf("input %q must be a path inside the repository, got %q", "config", in.configPath))
	}

	for _, f := range []struct{ name, val string }{{"base-sha", in.baseSHA}, {"head-sha", in.headSHA}} {
		if f.val == "" {
			problems = append(problems, fmt.Sprintf("input %q is required", f.name))
		} else if err := analyzer.CheckSHA("input "+f.name, f.val); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if in.comment {
		if in.token == "" {
			problems = append(problems, `input "github-token" is required when comment is true`)
		}
		if in.pr == 0 {
			problems = append(problems, "cannot publish a comment: this run is not for a pull request (no PR number in the event); run on pull_request or set comment: false")
		}
		if !strings.Contains(in.repo, "/") {
			problems = append(problems, "GITHUB_REPOSITORY is not set; this action must run inside GitHub Actions to publish a comment")
		}
		if in.apiURL == "" {
			in.apiURL = "https://api.github.com"
		}
		if c, err := github.New(in.apiURL, in.token, in.allowHTTP); err != nil {
			problems = append(problems, err.Error())
		} else {
			in.client = c
		}
	}
	return in, problems
}

func workspace(getenv func(string) string) string {
	if w := getenv("GITHUB_WORKSPACE"); w != "" {
		return w
	}
	return "."
}

// projectURL points the report footer at the action that is actually running.
func projectURL(getenv func(string) string) string {
	server, repo := getenv("GITHUB_SERVER_URL"), getenv("GITHUB_ACTION_REPOSITORY")
	if server == "" || repo == "" {
		return report.DefaultProjectURL
	}
	return strings.TrimRight(server, "/") + "/" + repo
}

// prNumber reads the PR number from the event payload, falling back to refs/pull/N/merge.
func prNumber(getenv func(string) string) int {
	if p := getenv("GITHUB_EVENT_PATH"); p != "" {
		if f, err := os.Open(p); err == nil { // #nosec G304 -- GITHUB_EVENT_PATH is set by the runner
			defer f.Close()
			var ev struct {
				PullRequest struct {
					Number int `json:"number"`
				} `json:"pull_request"`
			}
			if json.NewDecoder(f).Decode(&ev) == nil && ev.PullRequest.Number > 0 {
				return ev.PullRequest.Number
			}
		}
	}
	if rest, ok := strings.CutPrefix(getenv("GITHUB_REF"), "refs/pull/"); ok {
		if n, _, ok := strings.Cut(rest, "/"); ok {
			if v, err := strconv.Atoi(n); err == nil && v > 0 {
				return v
			}
		}
	}
	return 0
}

func loadConfig(ctx context.Context, repo analyzer.Repo, baseSHA, cfgPath string) (*config.Config, error) {
	data, found, err := repo.ReadFile(ctx, baseSHA, cfgPath, config.MaxBytes)
	if err != nil {
		return nil, fmt.Errorf("reading %s at base commit: %w", cfgPath, err)
	}
	if !found {
		return config.Default(), nil
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s (from base commit %.12s): %w", cfgPath, baseSHA, err)
	}
	return cfg, nil
}

func writeOutputs(file string, kv [][2]string) error {
	if file == "" {
		return nil
	}
	var b strings.Builder
	for _, p := range kv {
		if strings.ContainsAny(p[1], "\r\n") {
			return fmt.Errorf("output %s contains a newline", p[0])
		}
		b.WriteString(p[0] + "=" + p[1] + "\n")
	}
	return appendFile(file, b.String())
}

func appendFile(name, content string) error {
	if name == "" {
		return nil
	}
	// #nosec G302 G304 -- GITHUB_OUTPUT / GITHUB_STEP_SUMMARY are set by the runner, which reads them as another user
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

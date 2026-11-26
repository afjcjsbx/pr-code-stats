package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/afjcjsbx/pr-code-stats/internal/report"
)

// --- fixtures ---------------------------------------------------------------------------

type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	r := &gitRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.name", "t")
	r.git("config", "user.email", "t@example.com")
	return r
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	c := exec.Command("git", args...)
	c.Dir = r.dir
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func (r *gitRepo) commit(files map[string]string) string {
	r.t.Helper()
	for p, content := range files {
		full := filepath.Join(r.dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "c")
	return r.git("rev-parse", "HEAD")
}

type fakeAPI struct {
	mu       sync.Mutex
	comments []map[string]any
	postCode int // when non-zero, POST answers with this status
	requests int
}

const apiToken = "ghs_TOPSECRET"

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Header.Get("Authorization") != "Bearer "+apiToken {
		http.Error(w, `{"message":"Bad credentials"}`, 401)
		return
	}
	const list = "/repos/acme/widgets/issues/9/comments"
	switch {
	case r.Method == "GET" && r.URL.Path == list:
		json.NewEncoder(w).Encode(f.comments)
	case r.Method == "POST" && r.URL.Path == list:
		if f.postCode != 0 {
			w.WriteHeader(f.postCode)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
		var in struct{ Body string }
		json.NewDecoder(r.Body).Decode(&in)
		id := len(f.comments) + 1
		c := map[string]any{"id": id, "body": in.Body, "html_url": fmt.Sprintf("https://github.test/c/%d", id),
			"user": map[string]any{"login": "github-actions[bot]", "type": "Bot"}}
		f.comments = append(f.comments, c)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(c)
	case r.Method == "PATCH":
		var in struct{ Body string }
		json.NewDecoder(r.Body).Decode(&in)
		f.comments[0]["body"] = in.Body
		json.NewEncoder(w).Encode(f.comments[0])
	default:
		http.NotFound(w, r)
	}
}

type env map[string]string

func (e env) get(k string) string { return e[k] }

type runResult struct {
	code           int
	log            string
	outputs        map[string]string
	summary        string
	outputsContent string
}

func execute(t *testing.T, repo *gitRepo, e env) runResult {
	t.Helper()
	tmp := t.TempDir()
	outFile, sumFile := filepath.Join(tmp, "output"), filepath.Join(tmp, "summary")
	e["GITHUB_OUTPUT"], e["GITHUB_STEP_SUMMARY"], e["GITHUB_WORKSPACE"] = outFile, sumFile, repo.dir
	var log bytes.Buffer
	code := run(context.Background(), e.get, &log)

	res := runResult{code: code, log: log.String(), outputs: map[string]string{}}
	if b, err := os.ReadFile(outFile); err == nil {
		res.outputsContent = string(b)
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				res.outputs[k] = v
			}
		}
	}
	if b, err := os.ReadFile(sumFile); err == nil {
		res.summary = string(b)
	}
	if strings.Contains(res.log, apiToken) {
		t.Error("token leaked into the log")
	}
	return res
}

func prEnv(t *testing.T, base, head, apiURL string) env {
	t.Helper()
	event := filepath.Join(t.TempDir(), "event.json")
	os.WriteFile(event, []byte(`{"action":"opened","pull_request":{"number":9}}`), 0o644)
	return env{
		"INPUT_GITHUB-TOKEN": apiToken, "INPUT_BASE-SHA": base, "INPUT_HEAD-SHA": head,
		"INPUT_COMMENT": "true", "INPUT_CONFIG": ".github/pr-code-stats.yml",
		"INPUT_FAIL-ON-COMMENT-ERROR": "false", "INPUT_WRITE-STEP-SUMMARY": "true",
		"GITHUB_REPOSITORY": "acme/widgets", "GITHUB_API_URL": apiURL, "GITHUB_EVENT_PATH": event,
		"GITHUB_SERVER_URL": "https://github.com", "GITHUB_ACTION_REPOSITORY": "acme/pr-code-stats",
	}
}

func twoCommits(t *testing.T) (r *gitRepo, base, head string) {
	r = newGitRepo(t)
	base = r.commit(map[string]string{"a.go": "package a\n"})
	head = r.commit(map[string]string{"a.go": "package a\n\n// doc\nfunc F() {}\n", "a_test.go": "package a\n"})
	return
}

// --- tests ------------------------------------------------------------------------------

func TestRunCreatesThenUpdatesOneComment(t *testing.T) {
	repo, base, head := twoCommits(t)
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()

	first := execute(t, repo, prEnv(t, base, head, srv.URL))
	if first.code != 0 {
		t.Fatalf("exit %d\n%s", first.code, first.log)
	}
	want := map[string]string{
		"added": "4", "removed": "0", "net": "4", "files-changed": "2",
		"comment-url": "https://github.test/c/1", "comment-status": "created",
	}
	for k, v := range want {
		if first.outputs[k] != v {
			t.Errorf("output %s = %q, want %q", k, first.outputs[k], v)
		}
	}
	body := api.comments[0]["body"].(string)
	if !strings.HasPrefix(body, report.Marker) || !strings.Contains(body, "Pull Request #9") ||
		!strings.Contains(body, "https://github.com/acme/pr-code-stats") {
		t.Errorf("unexpected comment body:\n%s", body)
	}
	if !strings.Contains(first.summary, "| Lines added | +4 |") {
		t.Errorf("step summary missing report:\n%s", first.summary)
	}

	second := execute(t, repo, prEnv(t, base, head, srv.URL))
	if second.code != 0 || second.outputs["comment-status"] != "updated" || len(api.comments) != 1 {
		t.Errorf("second run: exit=%d status=%q comments=%d", second.code, second.outputs["comment-status"], len(api.comments))
	}
}

func TestRunCommentDisabledNeedsNoToken(t *testing.T) {
	repo, base, head := twoCommits(t)
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()

	e := prEnv(t, base, head, srv.URL)
	e["INPUT_COMMENT"], e["INPUT_GITHUB-TOKEN"], e["INPUT_WRITE-STEP-SUMMARY"] = "false", "", "false"
	delete(e, "GITHUB_EVENT_PATH") // not even a PR context
	res := execute(t, repo, e)
	if res.code != 0 || res.outputs["comment-status"] != "disabled" || res.outputs["comment-url"] != "" || res.outputs["added"] != "4" {
		t.Errorf("exit=%d outputs=%v\n%s", res.code, res.outputs, res.log)
	}
	if api.requests != 0 {
		t.Error("API called although comment is false")
	}
	if res.summary != "" {
		t.Error("summary written although write-step-summary is false")
	}
}

func TestRunForkTokenIsReadOnly(t *testing.T) {
	repo, base, head := twoCommits(t)
	api := &fakeAPI{postCode: 403}
	srv := httptest.NewServer(api)
	defer srv.Close()

	res := execute(t, repo, prEnv(t, base, head, srv.URL))
	if res.code != 0 {
		t.Fatalf("a read-only token must not fail the run by default (exit %d)\n%s", res.code, res.log)
	}
	if res.outputs["comment-status"] != "skipped" || res.outputs["added"] != "4" {
		t.Errorf("outputs = %v", res.outputs)
	}
	if !strings.Contains(res.log, "::warning::") || !strings.Contains(res.log, "forks") {
		t.Errorf("expected an explanatory warning:\n%s", res.log)
	}

	e := prEnv(t, base, head, srv.URL)
	e["INPUT_FAIL-ON-COMMENT-ERROR"] = "true"
	res = execute(t, repo, e)
	if res.code != exitFailure || res.outputs["comment-status"] != "skipped" || res.outputs["added"] != "4" {
		t.Errorf("fail-on-comment-error: exit=%d outputs=%v", res.code, res.outputs)
	}
	if !strings.Contains(res.log, "::error::") {
		t.Error("expected an error annotation")
	}
}

func TestRunBadCredentialsSkipsComment(t *testing.T) {
	repo, base, head := twoCommits(t)
	srv := httptest.NewServer(&fakeAPI{})
	defer srv.Close()
	e := prEnv(t, base, head, srv.URL)
	e["INPUT_GITHUB-TOKEN"] = "wrong"
	res := execute(t, repo, e)
	if res.code != 0 || res.outputs["comment-status"] != "skipped" || !strings.Contains(res.log, "Bad credentials") {
		t.Errorf("exit=%d outputs=%v\n%s", res.code, res.outputs, res.log)
	}
}

func TestRunValidatesInputsBeforeWork(t *testing.T) {
	repo, base, head := twoCommits(t)
	cases := []struct {
		name   string
		mutate func(env)
		want   []string
	}{
		{"missing shas", func(e env) { e["INPUT_BASE-SHA"], e["INPUT_HEAD-SHA"] = "", "" }, []string{`"base-sha" is required`, `"head-sha" is required`}},
		{"abbreviated sha", func(e env) { e["INPUT_BASE-SHA"] = base[:8] }, []string{"full lowercase commit SHA"}},
		{"ref instead of sha", func(e env) { e["INPUT_HEAD-SHA"] = "main" }, []string{"full lowercase commit SHA"}},
		{"token required when commenting", func(e env) { e["INPUT_GITHUB-TOKEN"] = "" }, []string{`"github-token" is required`}},
		{"not a pull request", func(e env) { delete(e, "GITHUB_EVENT_PATH") }, []string{"not for a pull request"}},
		{"bad boolean", func(e env) { e["INPUT_COMMENT"] = "yes" }, []string{`"comment" must be true or false`}},
		{"config outside repo", func(e env) { e["INPUT_CONFIG"] = "../../etc/passwd" }, []string{`"config" must be a path inside the repository`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := prEnv(t, base, head, "http://127.0.0.1:1")
			tc.mutate(e)
			res := execute(t, repo, e)
			if res.code != exitBadInput {
				t.Errorf("exit = %d, want %d\n%s", res.code, exitBadInput, res.log)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.log, w) {
					t.Errorf("log lacks %q:\n%s", w, res.log)
				}
			}
			if res.outputsContent != "" {
				t.Error("outputs written despite invalid input")
			}
		})
	}
}

func TestRunMissingCommitExplainsFetch(t *testing.T) {
	repo, _, head := twoCommits(t)
	e := prEnv(t, strings.Repeat("ab", 20), head, "http://127.0.0.1:1")
	res := execute(t, repo, e)
	if res.code != exitFailure || !strings.Contains(res.log, "fetch-depth: 0") {
		t.Errorf("exit=%d\n%s", res.code, res.log)
	}
}

// Security: the config that applies to a PR is the one on the base commit.
func TestRunReadsConfigFromBaseNotHead(t *testing.T) {
	r := newGitRepo(t)
	base := r.commit(map[string]string{
		".github/pr-code-stats.yml": "version: 1\nexclude: ['gen/']\n",
		"a.go":                      "package a\n",
	})
	head := r.commit(map[string]string{
		// the PR tries to hide its own changes by excluding everything
		".github/pr-code-stats.yml": "version: 1\nexclude: ['**']\n",
		"src/b.go":                  "package b\nvar x = 1\n",
		"gen/c.go":                  "package c\n",
	})
	e := prEnv(t, base, head, "")
	e["INPUT_COMMENT"] = "false"
	res := execute(t, r, e)
	// counted: src/b.go (+2) and the config file itself (+1/-1, Other); gen/ is excluded by the base config
	if res.code != 0 || res.outputs["added"] != "3" || res.outputs["removed"] != "1" || res.outputs["files-changed"] != "2" {
		t.Errorf("exit=%d outputs=%v\n%s", res.code, res.outputs, res.log)
	}
}

func TestRunMalformedConfigIsAnError(t *testing.T) {
	r := newGitRepo(t)
	base := r.commit(map[string]string{".github/pr-code-stats.yml": "version: 1\nexcludes: ['x']\n", "a.go": "package a\n"})
	head := r.commit(map[string]string{"a.go": "package a\nvar x = 1\n"})
	e := prEnv(t, base, head, "")
	e["INPUT_COMMENT"] = "false"
	res := execute(t, r, e)
	if res.code != exitBadInput || !strings.Contains(res.log, "excludes") || !strings.Contains(res.log, ".github/pr-code-stats.yml") {
		t.Errorf("exit=%d\n%s", res.code, res.log)
	}
}

func TestAnnotateEscapesWorkflowCommands(t *testing.T) {
	var b bytes.Buffer
	annotate(&b, "warning", "file\n::set-output name=x::y 100%\r")
	if got, want := b.String(), "::warning::file%0A::set-output name=x::y 100%25%0D\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if strings.Count(b.String(), "\n") != 1 {
		t.Error("annotation spans several lines")
	}
}

func TestPRNumberFallsBackToRef(t *testing.T) {
	for ref, want := range map[string]int{"refs/pull/12/merge": 12, "refs/heads/main": 0, "refs/pull/x/merge": 0, "": 0} {
		if got := prNumber(env{"GITHUB_REF": ref}.get); got != want {
			t.Errorf("ref %q -> %d, want %d", ref, got, want)
		}
	}
}

// action.yml is the public API: it must declare exactly the inputs the code reads and the
// outputs it writes.
func TestActionYMLMatchesCode(t *testing.T) {
	data, err := os.ReadFile("../../action.yml")
	if err != nil {
		t.Fatal(err)
	}
	var a struct {
		Name, Description string
		Inputs            map[string]struct {
			Description string
			Required    bool
			Default     *string
		}
		Outputs map[string]struct{ Description string }
		Runs    struct {
			Using, Image string
		}
	}
	if err := yaml.Unmarshal(data, &a); err != nil {
		t.Fatal(err)
	}
	if a.Name == "" || a.Description == "" || a.Runs.Using != "docker" || a.Runs.Image != "Dockerfile" {
		t.Errorf("name/description/runs wrong: %+v", a)
	}

	// inputs the code asks for
	read := map[string]bool{}
	repo, base, head := twoCommits(t)
	e := prEnv(t, base, head, "")
	e["INPUT_COMMENT"] = "false"
	e["GITHUB_WORKSPACE"] = repo.dir
	run(context.Background(), func(k string) string {
		if strings.HasPrefix(k, "INPUT_") {
			read[strings.ToLower(strings.TrimPrefix(k, "INPUT_"))] = true
		}
		return e[k]
	}, &bytes.Buffer{})
	declared := map[string]bool{}
	for name, in := range a.Inputs {
		declared[name] = true
		if in.Description == "" {
			t.Errorf("input %s has no description", name)
		}
	}
	if !equalSets(read, declared) {
		t.Errorf("inputs read by code %v != declared in action.yml %v", keys(read), keys(declared))
	}
	for name, def := range map[string]string{"comment": "true", "config": ".github/pr-code-stats.yml", "fail-on-comment-error": "false", "write-step-summary": "true"} {
		if got := a.Inputs[name].Default; got == nil || *got != def {
			t.Errorf("input %s default = %v, want %q", name, got, def)
		}
	}
	for _, name := range []string{"base-sha", "head-sha"} {
		if !a.Inputs[name].Required {
			t.Errorf("input %s must be required", name)
		}
	}

	// outputs the code writes
	res := execute(t, repo, e)
	written := map[string]bool{}
	for k := range res.outputs {
		written[k] = true
	}
	outs := map[string]bool{}
	for name := range a.Outputs {
		outs[name] = true
	}
	if !equalSets(written, outs) {
		t.Errorf("outputs written %v != declared %v", keys(written), keys(outs))
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalSets(a, b map[string]bool) bool {
	return strings.Join(keys(a), ",") == strings.Join(keys(b), ",")
}

// V-09: the token is only sent over https (loopback is fine; anything else needs an explicit opt-in).
func TestRunRefusesPlainHTTPAPI(t *testing.T) {
	repo, base, head := twoCommits(t)
	e := prEnv(t, base, head, "http://api.example.invalid")
	res := execute(t, repo, e)
	if res.code != exitBadInput || !strings.Contains(res.log, "must use https") {
		t.Errorf("exit=%d\n%s", res.code, res.log)
	}
	if res.outputsContent != "" {
		t.Error("outputs written although the inputs were rejected")
	}

	e = prEnv(t, base, head, "http://api.example.invalid")
	e["PR_CODE_STATS_ALLOW_HTTP"] = "1" // explicit opt-in: now it gets as far as trying (and failing) to connect
	res = execute(t, repo, e)
	if res.code != 0 || res.outputs["comment-status"] != "skipped" || strings.Contains(res.log, "must use https") {
		t.Errorf("opt-in: exit=%d outputs=%v\n%s", res.code, res.outputs, res.log)
	}
}

func TestRunRefusesOversizedConfig(t *testing.T) {
	r := newGitRepo(t)
	base := r.commit(map[string]string{".github/pr-code-stats.yml": "version: 1\n# " + strings.Repeat("x", 70_000) + "\n", "a.go": "package a\n"})
	head := r.commit(map[string]string{"a.go": "package a\nvar x = 1\n"})
	e := prEnv(t, base, head, "")
	e["INPUT_COMMENT"] = "false"
	res := execute(t, r, e)
	if res.code != exitBadInput || !strings.Contains(res.log, "larger than") {
		t.Errorf("exit=%d\n%s", res.code, res.log)
	}
}

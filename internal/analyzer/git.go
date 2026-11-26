package analyzer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var shaRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// maxGitOutput caps what is buffered from one git command (a var so tests can lower it).
var maxGitOutput = 256 << 20

var errTooLarge = errors.New("output too large")

// limitBuffer collects output but refuses to grow past max. It deliberately does not embed
// bytes.Buffer: that would promote ReadFrom, which io.Copy prefers over Write, bypassing the limit.
type limitBuffer struct {
	buf      bytes.Buffer
	max      int
	exceeded bool
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		b.exceeded = true
		return 0, errTooLarge
	}
	return b.buf.Write(p)
}

func (b *limitBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitBuffer) String() string { return b.buf.String() }

// passEnv is everything git (and the lexer worker) may inherit. The step environment holds
// INPUT_GITHUB-TOKEN and friends: nothing outside this list is ever handed to a child process.
var passEnv = []string{"PATH", "HOME", "TMPDIR", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"}

func cleanEnv(extra ...string) []string {
	var env []string
	for _, k := range passEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, extra...)
}

// Repo runs git against the repository in Dir. Git is always invoked directly (no shell)
// and only with fixed arguments plus full hex SHAs, so file contents and paths never
// reach a command line.
type Repo struct{ Dir string }

// hardening is prepended to every git call: no fsmonitor hook, no global attributes file.
var hardening = []string{"-c", "core.fsmonitor=false", "-c", "core.attributesFile=" + os.DevNull}

func (r Repo) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "git", append(append([]string{}, hardening...), args...)...) // #nosec G204 -- fixed arguments plus validated hex SHAs, no shell
	c.Dir = r.Dir
	c.Env = cleanEnv("GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	return c
}

// run returns stdout; on failure the error carries git's stderr.
func (r Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	stdout := &limitBuffer{max: maxGitOutput}
	stderr := &limitBuffer{max: 1 << 20}
	c := r.cmd(ctx, args...)
	c.Stdout, c.Stderr = stdout, stderr
	if err := c.Run(); err != nil {
		if stdout.exceeded {
			return nil, fmt.Errorf("git %s: output exceeds %d bytes", subcommand(args), maxGitOutput)
		}
		return nil, gitError(subcommand(args), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// subcommand returns the git subcommand, skipping a leading --attr-source=... style option.
func subcommand(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return "git"
}

func gitError(sub string, err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if len(stderr) > 500 {
		stderr = stderr[:500] + "..."
	}
	if stderr == "" {
		return fmt.Errorf("git %s: %w", sub, err)
	}
	return fmt.Errorf("git %s: %w: %s", sub, err, stderr)
}

// CheckSHA accepts only full lowercase hex SHAs: refs and abbreviations could be read as git options or move.
func CheckSHA(label, sha string) error {
	if !shaRe.MatchString(sha) {
		return fmt.Errorf("%s %q is not a full lowercase commit SHA", label, sha)
	}
	return nil
}

// VerifyCommit fails with fetch instructions if sha is not a commit available locally.
func (r Repo) VerifyCommit(ctx context.Context, label, sha string) error {
	if err := CheckSHA(label, sha); err != nil {
		return err
	}
	if _, err := r.run(ctx, "rev-parse", "--verify", "--quiet", sha+"^{commit}"); err != nil {
		return fmt.Errorf("%s commit %s is not available in the local clone; "+
			"use actions/checkout with fetch-depth: 0 (or fetch that commit) before this action: %w", label, sha, err)
	}
	return nil
}

// MergeBase returns the best common ancestor, i.e. the point the PR branched from.
func (r Repo) MergeBase(ctx context.Context, base, head string) (string, error) {
	out, err := r.run(ctx, "merge-base", base, head)
	if err != nil {
		return "", fmt.Errorf("cannot find a merge base of %s and %s (shallow clone? use fetch-depth: 0): %w", base, head, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ReadFile returns the content of path at commit sha. found is false if the path does not exist there.
// Files bigger than max bytes are refused before being read. The commit must have been verified first.
func (r Repo) ReadFile(ctx context.Context, sha, path string, max int64) (data []byte, found bool, err error) {
	if err := CheckSHA("commit", sha); err != nil {
		return nil, false, err
	}
	if strings.ContainsAny(path, "\x00\n") || strings.HasPrefix(path, "-") {
		return nil, false, fmt.Errorf("invalid path %q", path)
	}
	spec := sha + ":" + path
	var stderr bytes.Buffer
	c := r.cmd(ctx, "rev-parse", "--verify", "--quiet", spec)
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return nil, false, nil
		}
		return nil, false, gitError("rev-parse", err, stderr.String())
	}
	sizeOut, err := r.run(ctx, "cat-file", "-s", spec)
	if err != nil {
		return nil, false, err
	}
	if size, perr := strconv.ParseInt(strings.TrimSpace(string(sizeOut)), 10, 64); perr != nil || size > max {
		return nil, false, fmt.Errorf("%s is larger than the allowed %d bytes", path, max)
	}
	data, err = r.run(ctx, "cat-file", "blob", spec)
	return data, err == nil, err
}

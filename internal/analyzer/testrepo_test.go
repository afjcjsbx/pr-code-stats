package analyzer

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var numstatRow = regexp.MustCompile(`^(\d+)\t(\d+)\t`)

// testRepo is a throwaway git repository.
type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	// Keep the developer's own git config (signing, hooks, diff drivers...) out of the tests.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	r.git("config", "user.name", "t")
	r.git("config", "user.email", "t@example.com")
	return r
}

func (r *testRepo) git(args ...string) string {
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

func (r *testRepo) write(path, content string) {
	r.t.Helper()
	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) remove(path string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, path)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) move(from, to string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(r.dir, to)), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(r.dir, from), filepath.Join(r.dir, to)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) chmod(path string, mode os.FileMode) {
	r.t.Helper()
	if err := os.Chmod(filepath.Join(r.dir, path), mode); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) commit(msg string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	return r.git("rev-parse", "HEAD")
}

// numstat sums `git diff --numstat` between two commits: an oracle independent of our parser.
// -U0 because git's line counts (not just the display) can differ by context size on ambiguous diffs.
func (r *testRepo) numstat(base, head string) (added, removed int) {
	r.t.Helper()
	// With -U0 git also prints the patch after the numstat lines: keep only the numstat rows
	// ("12\t3\tpath"; binary files print "-\t-\t", which do not match and add nothing).
	for _, line := range strings.Split(r.git("diff", "--numstat", "-U0", "-M", base, head), "\n") {
		m := numstatRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		a, _ := strconv.Atoi(m[1])
		d, _ := strconv.Atoi(m[2])
		added, removed = added+a, removed+d
	}
	return
}

package analyzer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/afjcjsbx/pr-code-stats/internal/config"
)

func c(added, removed int) Counter { return Counter{added, removed} }

func analyze(t *testing.T, r *testRepo, base, head string, cfg *config.Config) *Result {
	t.Helper()
	if cfg == nil {
		cfg = config.Default()
	}
	res, err := Analyze(context.Background(), Repo{Dir: r.dir}, base, head, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// pyBase is 10 lines so that renames with one edited line stay above git's 50% similarity.
const pyBase = "import os\n\ndef a():\n    return 1\n\ndef b():\n    return 2\n\ndef c():\n    return 3\n"

func TestAnalyzeScenarios(t *testing.T) {
	type scenario struct {
		name  string
		setup func(r *testRepo) // state of the base commit
		edit  func(r *testRepo) // changes made by the PR
		cfg   string            // optional YAML config
		want  Stats
		files int
		// oracle: compare totals with git --numstat (off when exclusions or degradation change the totals)
		oracle   bool
		degraded int
	}
	scenarios := []scenario{
		{
			name:  "production, comment and test in one PR",
			setup: func(r *testRepo) { r.write("a.go", "package a\n") },
			edit: func(r *testRepo) {
				r.write("a.go", "package a\n\n// doc\nfunc F() {}\n")
				r.write("a_test.go", "package a\n\nfunc TestF() {}\n")
			},
			want:  Stats{Code: c(2, 0), Tests: c(3, 0), Comments: c(1, 0)},
			files: 2, oracle: true,
		},
		{
			name:  "replaced line is one removal plus one addition",
			setup: func(r *testRepo) { r.write("a.go", "package a\n\nvar x = 1\n") },
			edit:  func(r *testRepo) { r.write("a.go", "package a\n\nvar x = 2\n") },
			want:  Stats{Code: c(1, 1)},
			files: 1, oracle: true,
		},
		{
			name:  "removed lines are classified on the base version",
			setup: func(r *testRepo) { r.write("a.go", "package a\n\n// old comment\nvar x = 1\n") },
			edit:  func(r *testRepo) { r.write("a.go", "package a\n\n// new comment\nvar y = 1\n") },
			want:  Stats{Code: c(1, 1), Comments: c(1, 1)},
			files: 1, oracle: true,
		},
		{
			name:  "removed line starting with --- (lua comment) is not a file header",
			setup: func(r *testRepo) { r.write("a.lua", "print(1)\n--- old\nprint(2)\n") },
			edit:  func(r *testRepo) { r.write("a.lua", "print(1)\nprint(2)\n") },
			want:  Stats{Comments: c(0, 1)},
			files: 1, oracle: true,
		},
		{
			name:  "added line starting with +++",
			setup: func(r *testRepo) { r.write("a.py", "x = 1\n") },
			edit:  func(r *testRepo) { r.write("a.py", "x = 1\n+++ y\n") },
			want:  Stats{Code: c(1, 0)},
			files: 1, oracle: true,
		},
		{
			name:  "rename without changes",
			setup: func(r *testRepo) { r.write("a.py", pyBase) },
			edit:  func(r *testRepo) { r.move("a.py", "pkg/b.py") },
			want:  Stats{},
			files: 1, oracle: true,
		},
		{
			name:  "rename with changes: old side by old path, new side by new path",
			setup: func(r *testRepo) { r.write("a.py", pyBase) },
			edit: func(r *testRepo) {
				r.move("a.py", "tests/test_a.py")
				r.write("tests/test_a.py", strings.Replace(pyBase, "return 2", "return 22", 1))
			},
			want:  Stats{Code: c(0, 1), Tests: c(1, 0)},
			files: 1, oracle: true,
		},
		{
			name:  "file added and file deleted",
			setup: func(r *testRepo) { r.write("old.go", "package a\n\nfunc Old() {}\n") },
			edit: func(r *testRepo) {
				r.remove("old.go")
				r.write("new.go", "package a\n// c\n")
			},
			want:  Stats{Code: c(1, 3), Comments: c(1, 0)},
			files: 2, oracle: true,
		},
		{
			name:  "binary file counts as changed but adds no lines",
			setup: func(r *testRepo) { r.write("img.png", "\x89PNG\x00\x01\x02") },
			edit:  func(r *testRepo) { r.write("img.png", "\x89PNG\x00\x03\x04\x05") },
			want:  Stats{},
			files: 1, oracle: true,
		},
		{
			name:  "empty file added",
			setup: func(r *testRepo) { r.write("keep.go", "package a\n") },
			edit:  func(r *testRepo) { r.write("empty.go", "") },
			want:  Stats{},
			files: 1, oracle: true,
		},
		{
			name:  "no newline at end of file on both sides",
			setup: func(r *testRepo) { r.write("a.py", "x = 1") },
			edit:  func(r *testRepo) { r.write("a.py", "x = 2") },
			want:  Stats{Code: c(1, 1)},
			files: 1, oracle: true,
		},
		{
			name:  "newline added at end of file",
			setup: func(r *testRepo) { r.write("a.py", "x = 1") },
			edit:  func(r *testRepo) { r.write("a.py", "x = 1\n") },
			want:  Stats{Code: c(1, 1)},
			files: 1, oracle: true,
		},
		{
			name:  "CRLF files",
			setup: func(r *testRepo) { r.write("a.py", "# c\r\nx = 1\r\n") },
			edit:  func(r *testRepo) { r.write("a.py", "# c\r\nx = 1\r\n# d\r\ny = 2\r\n") },
			want:  Stats{Code: c(1, 0), Comments: c(1, 0)},
			files: 1, oracle: true,
		},
		{
			name:  "spaces, unicode and quotes in names",
			setup: func(r *testRepo) { r.write("keep.go", "package a\n") },
			edit: func(r *testRepo) {
				r.write("dir with space/ünï cödé.go", "package a\n")
				r.write(`we"ird name.go`, "package a\n// c\n")
				r.write("tab\tname_test.go", "package a\n")
			},
			want:  Stats{Code: c(2, 0), Tests: c(1, 0), Comments: c(1, 0)},
			files: 3, oracle: true,
		},
		{
			name: "several hunks in one file",
			setup: func(r *testRepo) {
				r.write("a.go", "package a\n// c1\n"+strings.Repeat("var x = 1\n", 25)+"// c2\nvar z = 1\n")
			},
			edit: func(r *testRepo) {
				r.write("a.go", "package a\n// c1 changed\n"+strings.Repeat("var x = 1\n", 25)+"// c2\nvar z = 2\n")
			},
			want:  Stats{Code: c(1, 1), Comments: c(1, 1)},
			files: 1, oracle: true,
		},
		{
			name:  "mode-only change",
			setup: func(r *testRepo) { r.write("run.sh", "echo hi\n") },
			edit:  func(r *testRepo) { r.chmod("run.sh", 0o755) },
			want:  Stats{},
			files: 1, oracle: true,
		},
		{
			name:  "docs, data and unknown files are Other",
			setup: func(r *testRepo) { r.write("keep.go", "package a\n") },
			edit: func(r *testRepo) {
				r.write("README.md", "# Title\n\ntext\n")
				r.write("conf.yaml", "# comment\na: 1\n")
				r.write("data.zzz", "x\ny\n")
				r.write("go.sum", "h1:abc\n")
				r.write("tests/fixtures/data.json", "{}\n") // under tests/, still data
			},
			want:  Stats{Other: c(3+2+2+1+1, 0)},
			files: 5, oracle: true,
		},
		{
			name:  "excluded paths are ignored everywhere",
			setup: func(r *testRepo) { r.write("a.go", "package a\n") },
			edit: func(r *testRepo) {
				r.write("a.go", "package a\nvar x = 1\n")
				r.write("vendor/lib/lib.go", "package lib\nvar y = 1\n")
				r.write("web/app.min.js", "var a=1;\n")
				r.write("gen/out.go", "package gen\n")
			},
			cfg:   "version: 1\nexclude: ['gen/']\n",
			want:  Stats{Code: c(1, 0)},
			files: 1,
		},
		{
			name:  "file over max_file_size is Other, not partially counted",
			setup: func(r *testRepo) { r.write("big.go", "package a\n") },
			edit: func(r *testRepo) {
				r.write("big.go", "package a\n// c\n"+strings.Repeat("var x = 1\n", 20))
			},
			cfg:      "version: 1\nmax_file_size: 50\n",
			want:     Stats{Other: c(21, 0)},
			files:    1,
			oracle:   true, // totals stay exact; only the category changes
			degraded: 1,
		},
		{
			name:  "type change file -> symlink",
			setup: func(r *testRepo) { r.write("a.go", "package a\n\nfunc F() {}\n") },
			edit: func(r *testRepo) {
				r.remove("a.go")
				if err := os.Symlink("target.go", filepath.Join(r.dir, "a.go")); err != nil {
					t.Skip("symlinks unavailable:", err)
				}
			},
			want:  Stats{Code: c(1, 3)},
			files: 1, oracle: true,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			r := newTestRepo(t)
			s.setup(r)
			base := r.commit("base")
			s.edit(r)
			head := r.commit("head")

			cfg := config.Default()
			if s.cfg != "" {
				var err error
				if cfg, err = config.Parse([]byte(s.cfg)); err != nil {
					t.Fatal(err)
				}
			}
			res := analyze(t, r, base, head, cfg)

			if res.Stats != s.want {
				t.Errorf("stats = %+v\nwant    %+v", res.Stats, s.want)
			}
			if res.FilesChanged != s.files {
				t.Errorf("files changed = %d, want %d", res.FilesChanged, s.files)
			}
			if res.Degraded != s.degraded {
				t.Errorf("degraded = %d, want %d (warnings: %v)", res.Degraded, s.degraded, res.Warnings)
			}
			if s.oracle {
				wa, wr := r.numstat(base, head)
				if tot := res.Stats.Total(); tot.Added != wa || tot.Removed != wr {
					t.Errorf("totals +%d/-%d disagree with git numstat +%d/-%d", tot.Added, tot.Removed, wa, wr)
				}
			}
			tot := res.Stats.Total()
			if res.Stats.Net() != tot.Added-tot.Removed {
				t.Errorf("net %d != added-removed", res.Stats.Net())
			}
		})
	}
}

func TestAnalyzeNoChanges(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	sha := r.commit("only")
	res := analyze(t, r, sha, sha, nil)
	if res.Stats != (Stats{}) || res.FilesChanged != 0 {
		t.Errorf("expected empty result, got %+v", res)
	}
}

// The diff starts at the merge base, so commits that reached the base branch after the PR
// branched are not attributed to the PR (base.sha in the event payload is the base tip).
func TestAnalyzeUsesMergeBase(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	r.commit("fork point")

	r.git("checkout", "-q", "-b", "pr")
	r.write("pr.go", "package a\nvar fromPR = 1\n")
	head := r.commit("pr work")

	r.git("checkout", "-q", "main")
	r.write("main_only.go", strings.Repeat("var onMain = 1\n", 7))
	baseTip := r.commit("landed on main after the PR branched")

	res := analyze(t, r, baseTip, head, nil)
	if want := (Stats{Code: c(2, 0)}); res.Stats != want || res.FilesChanged != 1 {
		t.Errorf("got %+v files=%d, want %+v files=1", res.Stats, res.FilesChanged, want)
	}
}

func TestAnalyzeErrors(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	sha := r.commit("c")
	missing := strings.Repeat("ab", 20)
	ctx := context.Background()
	cfg := config.Default()

	if _, err := Analyze(ctx, Repo{r.dir}, sha, missing, cfg); err == nil || !strings.Contains(err.Error(), "fetch-depth") {
		t.Errorf("missing commit: want error mentioning fetch-depth, got %v", err)
	}
	for _, bad := range []string{"", "HEAD", "--output=x", strings.ToUpper(sha), sha[:10]} {
		if _, err := Analyze(ctx, Repo{r.dir}, bad, sha, cfg); err == nil {
			t.Errorf("base %q accepted", bad)
		}
	}
}

func TestReadFile(t *testing.T) {
	r := newTestRepo(t)
	r.write(".github/pr-code-stats.yml", "version: 1\n")
	sha := r.commit("c")
	repo := Repo{r.dir}

	data, found, err := repo.ReadFile(context.Background(), sha, ".github/pr-code-stats.yml", 1<<20)
	if err != nil || !found || string(data) != "version: 1\n" {
		t.Errorf("got %q found=%v err=%v", data, found, err)
	}
	_, found, err = repo.ReadFile(context.Background(), sha, "nope.yml", 1<<20)
	if err != nil || found {
		t.Errorf("absent file: found=%v err=%v", found, err)
	}
	if _, _, err = repo.ReadFile(context.Background(), sha, "--flag", 1<<20); err == nil {
		t.Error("path starting with - accepted")
	}
}

func TestAnalyzeLargeFiles(t *testing.T) {
	setLexLimits(t, 2*time.Minute, 4*time.Minute) // this test is about size, not speed: -race makes lexing ~10x slower
	line := "func f() { /* c */ x := \"// not a comment\" }\n"
	within := "package a\n" + strings.Repeat(line, 20_000) // ~0.9 MB, lexed
	over := "package a\n" + strings.Repeat(line, 80_000)   // ~3.6 MB, skipped as Other

	r := newTestRepo(t)
	r.write("keep.go", "package a\n")
	base := r.commit("base")
	r.write("within.go", within)
	r.write("over.go", over)
	head := r.commit("head")

	res := analyze(t, r, base, head, nil)
	want := Stats{Code: c(20_001, 0), Other: c(80_001, 0)}
	if res.Stats != want || res.Degraded != 1 || res.FilesChanged != 2 {
		t.Errorf("stats=%+v degraded=%d files=%d, want %+v degraded=1 files=2 (warnings %v)", res.Stats, res.Degraded, res.FilesChanged, want, res.Warnings)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "over.go") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestReadFileRefusesOversizedFiles(t *testing.T) {
	r := newTestRepo(t)
	r.write("big.yml", strings.Repeat("a: 1\n", 1000))
	sha := r.commit("c")
	if _, _, err := (Repo{r.dir}).ReadFile(context.Background(), sha, "big.yml", 100); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v", err)
	}
}

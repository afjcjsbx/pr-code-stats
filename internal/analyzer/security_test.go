package analyzer

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"

	"github.com/afjcjsbx/pr-code-stats/internal/config"
)

// Regression tests for the findings of the security audit (V-01 ... V-09).

func setLexLimits(t *testing.T, perFile, budget time.Duration) {
	t.Helper()
	oldT, oldB := lexTimeout, lexBudget
	lexTimeout, lexBudget = perFile, budget
	t.Cleanup(func() { lexTimeout, lexBudget = oldT, oldB })
}

// V-01: a Java file of 20,000 newlines takes ~50 s inside Chroma. It must be cut off, counted
// as Other with exact line numbers, and must not stop later files from being classified.
func TestHostileLexerInputIsBoundedByTimeout(t *testing.T) {
	setLexLimits(t, 1500*time.Millisecond, time.Minute)
	r := newTestRepo(t)
	r.write("keep.go", "package a\n")
	base := r.commit("base")
	r.write("Main.java", strings.Repeat("\n", 20_000))
	r.write("Other.java", strings.Repeat("\n", 20_000))
	r.write("ok.go", "package a\n// fine\nvar x = 1\n")
	head := r.commit("head")

	began := time.Now()
	res := analyze(t, r, base, head, nil)
	if took := time.Since(began); took > 20*time.Second {
		t.Fatalf("analysis took %s; the hostile files were not cut off", took)
	}
	want := Stats{Other: c(40_000, 0), Code: c(2, 0), Comments: c(1, 0)}
	if res.Stats != want {
		t.Errorf("stats = %+v, want %+v", res.Stats, want)
	}
	if res.Degraded != 2 || len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], "time limit") {
		t.Errorf("degraded=%d warnings=%v", res.Degraded, res.Warnings)
	}
	if tot := res.Stats.Total(); tot.Added != 40_003 {
		t.Errorf("total added = %d: lines must stay exact", tot.Added)
	}
}

func TestLexBudgetIsSharedByAllFiles(t *testing.T) {
	setLexLimits(t, 1500*time.Millisecond, 2*time.Second)
	r := newTestRepo(t)
	r.write("keep.go", "package a\n")
	base := r.commit("base")
	for i := 0; i < 3; i++ {
		r.write(fmt.Sprintf("H%d.java", i), strings.Repeat("\n", 20_000))
	}
	r.write("late.go", "package a\nvar x = 1\n") // innocent, but the budget is gone by now
	head := r.commit("head")

	began := time.Now()
	res := analyze(t, r, base, head, nil)
	if took := time.Since(began); took > 15*time.Second {
		t.Fatalf("took %s with a 2 s budget", took)
	}
	if res.Degraded != 4 || res.Stats.Other.Added != 60_002 {
		t.Errorf("degraded=%d stats=%+v warnings=%v", res.Degraded, res.Stats, res.Warnings)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "budget") {
		t.Errorf("warnings do not mention the budget: %v", res.Warnings)
	}
}

func TestPoolRecoversAfterTimeoutAndCrash(t *testing.T) {
	setLexLimits(t, 1200*time.Millisecond, time.Minute)
	p := newLexPool()
	defer p.close()
	want, _ := classifyLines(lexers.Get("Go"), []byte("// c\nx := 1\n"))

	if _, err := p.classify("Main.java", bytes.Repeat([]byte("\n"), 20_000)); err != errLexTimeout {
		t.Fatalf("hostile file: err = %v, want the per-file timeout", err)
	}
	got, err := p.classify("a.go", []byte("// c\nx := 1\n"))
	if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("after a timeout the pool must restart: got %v, %v", got, err)
	}
	_ = p.cmd.Process.Kill() // simulate a worker that dies on its own (OOM, crash...)
	_, _ = p.cmd.Process.Wait()
	if _, err := p.classify("a.go", []byte("x := 1\n")); err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("a dead worker must be reported, got %v", err)
	}
	if got, err := p.classify("a.go", []byte("// c\nx := 1\n")); err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("pool did not recover after a crash: %v, %v", got, err)
	}
}

func TestWorkerRefusesOversizedRequests(t *testing.T) {
	var req bytes.Buffer
	binary.Write(&req, binary.BigEndian, uint32(10))
	binary.Write(&req, binary.BigEndian, uint32(maxWorkerContent+1))
	if err := serveWorker(&req, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v", err)
	}
	if err := serveWorker(bytes.NewReader(nil), &bytes.Buffer{}); err != nil {
		t.Errorf("clean EOF must end the worker without error, got %v", err)
	}
}

// V-02: .gitattributes in the pull request must not change what counts as a text file.
func TestGitattributesInThePRCannotHideLines(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	base := r.commit("base")
	r.write("a.go", "package a\n"+strings.Repeat("var x = 1\n", 100))
	r.write(".gitattributes", "*.go -diff\n*.go binary\n")
	head := r.commit("head")

	res := analyze(t, r, base, head, nil)
	if res.Stats.Code.Added != 100 || res.Stats.Total().Added != 102 {
		t.Errorf("stats = %+v: the PR's .gitattributes hid its own lines", res.Stats)
	}
	// A .gitattributes already on the base branch is trusted and still honoured.
	r.write(".gitattributes", "*.go -diff\n")
	base2 := r.commit("base branch marks go files binary")
	r.write("a.go", "package a\n"+strings.Repeat("var x = 1\n", 150))
	head2 := r.commit("more")
	if res := analyze(t, r, base2, head2, nil); res.Stats.Total().Added != 0 {
		t.Errorf("base attributes ignored: %+v", res.Stats)
	}
}

// V-05: the step environment (INPUT_GITHUB-TOKEN...) must never reach a git child process.
func TestGitChildrenDoNotInheritTheEnvironment(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	r := newTestRepo(t)
	r.write("a.go", "package a\n")
	base := r.commit("b")
	r.write("a.go", "package a\nvar x = 1\n")
	head := r.commit("h")

	shim := t.TempDir()
	log := filepath.Join(shim, "env.log")
	script := fmt.Sprintf("#!/bin/sh\nenv >> %q\nexec %q \"$@\"\n", log, realGit)
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("INPUT_GITHUB-TOKEN", "ghs_SUPERSECRET")
	t.Setenv("GITHUB_TOKEN", "ghs_SUPERSECRET2")
	t.Setenv("GIT_EXTERNAL_DIFF", "/bin/false") // a hostile-looking GIT_* variable must not pass either

	analyze(t, r, base, head, nil)
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("shim never ran: %v", err)
	}
	if env := string(data); strings.Contains(env, "SUPERSECRET") || strings.Contains(env, "INPUT_") ||
		strings.Contains(env, "GITHUB_TOKEN") || strings.Contains(env, "GIT_EXTERNAL_DIFF") {
		t.Errorf("git inherited the step environment:\n%s", env)
	} else if !strings.Contains(env, "PATH=") {
		t.Errorf("git lost PATH:\n%s", env)
	}
}

// V-04: hard limits fail loudly instead of reporting something partial.
func TestLimitsRefuseInsteadOfReportingPartially(t *testing.T) {
	r := newTestRepo(t)
	r.write("keep.go", "package a\n")
	base := r.commit("b")
	for i := 0; i < 5; i++ {
		r.write(fmt.Sprintf("f%d.go", i), "package a\nvar x = 1\n")
	}
	head := r.commit("h")
	cfg := config.Default()
	run := func() error {
		_, err := Analyze(context.Background(), Repo{r.dir}, base, head, cfg)
		return err
	}

	old := maxChanges
	maxChanges = 3
	if err := run(); err == nil || !strings.Contains(err.Error(), "touches 5 files") {
		t.Errorf("maxChanges: err = %v", err)
	}
	maxChanges = old

	oldP := maxPatchBytes
	maxPatchBytes = 50
	if err := run(); err == nil || !strings.Contains(err.Error(), "patch is larger") {
		t.Errorf("maxPatchBytes: err = %v", err)
	}
	maxPatchBytes = oldP

	oldO := maxGitOutput
	maxGitOutput = 10
	if err := run(); err == nil || !strings.Contains(err.Error(), "output exceeds") {
		t.Errorf("maxGitOutput: err = %v", err)
	}
	maxGitOutput = oldO

	oldA := analysisTimeout
	analysisTimeout = time.Nanosecond
	if err := run(); err == nil || !strings.Contains(err.Error(), "time limit") {
		t.Errorf("analysisTimeout: err = %v", err)
	}
	analysisTimeout = oldA

	if err := run(); err != nil {
		t.Errorf("limits restored but the run fails: %v", err)
	}
}

func TestFileCacheRetentionIsBounded(t *testing.T) {
	r := newTestRepo(t)
	for i := 0; i < 30; i++ {
		r.write(fmt.Sprintf("f%d.go", i), strings.Repeat("var x = 1\n", 20))
	}
	sha := r.commit("c")
	blobs, err := Repo{r.dir}.openBlobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.close()
	pool := newLexPool()
	defer pool.close()
	fc := newFileCache(blobs, pool, 1<<20)

	old := retainLimit
	retainLimit = 100 // bytes of kinds: five of the 20-line files
	defer func() { retainLimit = old }()
	for i := 0; i < 30; i++ {
		p := fmt.Sprintf("f%d.go", i)
		blob := r.git("rev-parse", sha+":"+p)
		if kinds, reason, err := fc.kinds(blob, p); err != nil || reason != "" || len(kinds) != 20 {
			t.Fatalf("%s: %v %q %d", p, err, reason, len(kinds))
		}
		if fc.retained > retainLimit {
			t.Fatalf("retained %d bytes, limit %d", fc.retained, retainLimit)
		}
	}
	if len(fc.entries) > 6 {
		t.Errorf("cache grew to %d entries", len(fc.entries))
	}
}

// V-04: the faster lexer matcher must pick exactly what chroma picks.
func TestMatchLexerEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var names []string
	concretize := func(g string) string {
		var b strings.Builder
		for i := 0; i < len(g); i++ {
			switch g[i] {
			case '*':
				b.WriteString("foo")
			case '?':
				b.WriteString("a")
			case '[':
				j := strings.IndexByte(g[i:], ']')
				if j < 0 {
					b.WriteByte('[')
					continue
				}
				class := strings.TrimPrefix(g[i+1:i+j], "!")
				if class == "" || strings.HasPrefix(g[i+1:], "!") {
					b.WriteString("Z")
				} else {
					b.WriteByte(class[0])
				}
				i += j
			case '\\':
				if i+1 < len(g) {
					i++
					b.WriteByte(g[i])
				}
			default:
				b.WriteByte(g[i])
			}
		}
		return b.String()
	}
	for _, l := range lexers.GlobalLexerRegistry.Lexers {
		cfg := l.Config()
		for _, g := range append(append([]string{}, cfg.Filenames...), cfg.AliasFilenames...) {
			names = append(names, concretize(g))
		}
	}
	generic := []string{"foo.go", "FOO.GO", ".go", "go", "a.b.c", "README", "x.min.js", "dir.d", "a.tar.gz", "Dockerfile.dev", "x.go.in", "x.go~", "x.py.bak", ".gitignore", "CMakeLists.txt", "Makefile.am", "a b.c", "ü.rs", strings.Repeat("a", 300) + ".java"}
	var all []string
	for _, n := range names {
		all = append(all, n)
		all = append(all, n+ignoredSuffixes[rng.Intn(len(ignoredSuffixes))])
	}
	all = append(all, generic...)
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	if testing.Short() && len(all) > 300 {
		all = all[:300]
	} else if len(all) > 1200 {
		all = all[:1200]
	}
	for _, n := range all {
		want, got := lexers.Match(n), matchLexer(n)
		if (want == nil) != (got == nil) || (want != nil && want.Config().Name != got.Config().Name) {
			t.Fatalf("%q: chroma picks %v, matchLexer picks %v", n, lexName(want), lexName(got))
		}
	}
}

func TestMatchLexerIsFastOnManyDistinctNames(t *testing.T) {
	matchLexer("warm.go")
	began := time.Now()
	for i := 0; i < 20_000; i++ {
		matchLexer(fmt.Sprintf("file%06d.zz%d", i, i%7))
	}
	if took := time.Since(began); took > 5*time.Second { // chroma needs ~4 ms each: 80 s
		t.Errorf("20,000 distinct names took %s", took)
	}
}

func lexName(l interface{ Config() *chroma.Config }) string {
	if l == nil {
		return "<none>"
	}
	return l.Config().Name
}

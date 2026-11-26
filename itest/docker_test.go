//go:build integration

// Package itest runs the real Docker image the way the Actions runner does: root user, workspace
// mounted at /github/workspace, inputs as INPUT_* variables. Run with:
//
//	go test -tags integration -count=1 ./itest/
//
// PR_CODE_STATS_IMAGE selects a prebuilt image; otherwise the Dockerfile in the repo root is built.
package itest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var image = "pr-code-stats:itest"

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Println("docker not installed; skipping")
		os.Exit(0)
	}
	if v := os.Getenv("PR_CODE_STATS_IMAGE"); v != "" {
		image = v
	} else if out, err := exec.Command("docker", "build", "-t", image, "..").CombinedOutput(); err != nil {
		fmt.Printf("docker build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// --- helpers ----------------------------------------------------------------------------

func docker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c := exec.Command("docker", args...)
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return out.String(), err
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo returns a repo dir with base and head commits.
func newRepo(t *testing.T) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.name", "t")
	git(t, dir, "config", "user.email", "t@example.com")
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package a\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	base = git(t, dir, "rev-parse", "HEAD")
	write("a.go", "package a\n\n// doc\nfunc F() {}\n")
	write("a_test.go", "package a\n")
	write("README.md", "# hi\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "head")
	head = git(t, dir, "rev-parse", "HEAD")
	return
}

const token = "ghs_ITEST_SECRET"

// fakeAPI serves PR 9 of acme/widgets on all interfaces so the container can reach it.
type fakeAPI struct {
	mu       sync.Mutex
	comments []map[string]any
	postCode int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, `{"message":"Bad credentials"}`, 401)
		return
	}
	const list = "/repos/acme/widgets/issues/9/comments"
	var in struct{ Body string }
	switch {
	case r.Method == "GET" && r.URL.Path == list:
		json.NewEncoder(w).Encode(append([]map[string]any{}, f.comments...))
	case r.Method == "POST" && r.URL.Path == list:
		if f.postCode != 0 {
			w.WriteHeader(f.postCode)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
		json.NewDecoder(r.Body).Decode(&in)
		id := len(f.comments) + 1
		c := map[string]any{"id": id, "body": in.Body, "html_url": fmt.Sprintf("https://github.test/c/%d", id),
			"user": map[string]any{"login": "github-actions[bot]", "type": "Bot"}}
		f.comments = append(f.comments, c)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(c)
	case r.Method == "PATCH":
		json.NewDecoder(r.Body).Decode(&in)
		f.comments[0]["body"] = in.Body
		json.NewEncoder(w).Encode(f.comments[0])
	default:
		http.NotFound(w, r)
	}
}

func startAPI(t *testing.T, f *fakeAPI) (url string) {
	t.Helper()
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(f)
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return fmt.Sprintf("http://host.docker.internal:%d", l.Addr().(*net.TCPAddr).Port)
}

type result struct {
	exit    int
	log     string
	outputs map[string]string
	summary string
}

// runAction starts the image like the runner would. workspace is either a host directory or a
// docker volume name (anything without a path separator).
func runAction(t *testing.T, workspace string, env map[string]string, extra ...string) result {
	t.Helper()
	fc := t.TempDir()
	os.Chmod(fc, 0o777)
	event := `{"action":"opened","pull_request":{"number":9}}`
	if err := os.WriteFile(filepath.Join(fc, "event.json"), []byte(event), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "--rm",
		"--add-host", "host.docker.internal:host-gateway",
		"--workdir", "/github/workspace",
		"-v", workspace + ":/github/workspace",
		"-v", fc + ":/github/file_commands",
		"-e", "GITHUB_WORKSPACE=/github/workspace",
		"-e", "GITHUB_OUTPUT=/github/file_commands/set_output",
		"-e", "GITHUB_STEP_SUMMARY=/github/file_commands/summary.md",
		"-e", "GITHUB_EVENT_PATH=/github/file_commands/event.json",
		"-e", "GITHUB_REPOSITORY=acme/widgets",
		"-e", "GITHUB_SERVER_URL=https://github.com",
		"-e", "GITHUB_ACTION_REPOSITORY=acme/pr-code-stats",
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, extra...)
	args = append(args, image)
	out, err := docker(t, args...)

	res := result{log: out, outputs: map[string]string{}}
	if ee, ok := err.(*exec.ExitError); ok {
		res.exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(fc, "set_output")); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				res.outputs[k] = v
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(fc, "summary.md")); err == nil {
		res.summary = string(b)
	}
	if strings.Contains(res.log, token) {
		t.Error("token leaked into the container log")
	}
	return res
}

func inputs(base, head, apiURL string) map[string]string {
	return map[string]string{
		"INPUT_GITHUB-TOKEN": token, "INPUT_BASE-SHA": base, "INPUT_HEAD-SHA": head,
		"INPUT_COMMENT": "true", "INPUT_CONFIG": ".github/pr-code-stats.yml",
		"INPUT_FAIL-ON-COMMENT-ERROR": "false", "INPUT_WRITE-STEP-SUMMARY": "true",
		"GITHUB_API_URL":           apiURL,
		"PR_CODE_STATS_ALLOW_HTTP": "1", // the fake API is plain http on host.docker.internal
	}
}

// --- tests ------------------------------------------------------------------------------

func TestImageCreatesThenUpdatesComment(t *testing.T) {
	dir, base, head := newRepo(t)
	api := &fakeAPI{}
	url := startAPI(t, api)

	first := runAction(t, dir, inputs(base, head, url))
	if first.exit != 0 {
		t.Fatalf("exit %d\n%s", first.exit, first.log)
	}
	want := map[string]string{"added": "5", "removed": "0", "net": "5", "files-changed": "3",
		"comment-url": "https://github.test/c/1", "comment-status": "created"}
	for k, v := range want {
		if first.outputs[k] != v {
			t.Errorf("output %s = %q, want %q", k, first.outputs[k], v)
		}
	}
	body, _ := api.comments[0]["body"].(string)
	for _, s := range []string{"<!-- pr-code-stats:report:v1 -->", "Pull Request #9", "| 💬 Comments | 1 | 0 |", "| 📄 Other | 1 | 0 |", "https://github.com/acme/pr-code-stats"} {
		if !strings.Contains(body, s) {
			t.Errorf("comment lacks %q:\n%s", s, body)
		}
	}
	if !strings.Contains(first.summary, "| Lines added | +5 |") {
		t.Errorf("step summary:\n%s", first.summary)
	}

	second := runAction(t, dir, inputs(base, head, url))
	if second.exit != 0 || second.outputs["comment-status"] != "updated" || len(api.comments) != 1 {
		t.Errorf("second run: exit=%d status=%q comments=%d\n%s", second.exit, second.outputs["comment-status"], len(api.comments), second.log)
	}
}

func TestImageForkTokenReadOnly(t *testing.T) {
	dir, base, head := newRepo(t)
	url := startAPI(t, &fakeAPI{postCode: 403})

	res := runAction(t, dir, inputs(base, head, url))
	if res.exit != 0 || res.outputs["comment-status"] != "skipped" || res.outputs["added"] != "5" {
		t.Errorf("exit=%d outputs=%v\n%s", res.exit, res.outputs, res.log)
	}
	if !strings.Contains(res.log, "::warning::") || !strings.Contains(res.log, "forks") {
		t.Errorf("missing warning:\n%s", res.log)
	}

	env := inputs(base, head, url)
	env["INPUT_FAIL-ON-COMMENT-ERROR"] = "true"
	res = runAction(t, dir, env)
	if res.exit != 1 || res.outputs["comment-status"] != "skipped" {
		t.Errorf("fail-on-comment-error: exit=%d outputs=%v", res.exit, res.outputs)
	}
}

func TestImageCommentDisabled(t *testing.T) {
	dir, base, head := newRepo(t)
	env := inputs(base, head, "http://127.0.0.1:1") // unreachable on purpose: must not be contacted
	env["INPUT_COMMENT"], env["INPUT_GITHUB-TOKEN"] = "false", ""
	res := runAction(t, dir, env)
	if res.exit != 0 || res.outputs["comment-status"] != "disabled" || res.outputs["files-changed"] != "3" {
		t.Errorf("exit=%d outputs=%v\n%s", res.exit, res.outputs, res.log)
	}
}

func TestImageMissingCommit(t *testing.T) {
	dir, _, head := newRepo(t)
	res := runAction(t, dir, inputs(strings.Repeat("ab", 20), head, "http://127.0.0.1:1"))
	if res.exit != 1 || !strings.Contains(res.log, "fetch-depth: 0") {
		t.Errorf("exit=%d\n%s", res.exit, res.log)
	}
}

func TestImageInvalidInputs(t *testing.T) {
	dir, _, _ := newRepo(t)
	res := runAction(t, dir, map[string]string{"INPUT_COMMENT": "false"})
	if res.exit != 2 || !strings.Contains(res.log, `"base-sha" is required`) {
		t.Errorf("exit=%d\n%s", res.exit, res.log)
	}
}

// The runner mounts a workspace owned by another uid than the container's root; without the
// safe.directory entry baked into the image, git refuses to touch it ("dubious ownership").
func TestImageWorkspaceOwnedByAnotherUser(t *testing.T) {
	vol := fmt.Sprintf("pr-code-stats-itest-%d", os.Getpid())
	if out, err := docker(t, "volume", "create", vol); err != nil {
		t.Fatalf("volume: %v\n%s", err, out)
	}
	t.Cleanup(func() { docker(t, "volume", "rm", "-f", vol) })

	// populate the volume as uid 4242
	script := `set -e
chown 4242:4242 /ws
`
	if out, err := docker(t, "run", "--rm", "-v", vol+":/ws", "--entrypoint", "sh", image, "-c", script); err != nil {
		t.Fatalf("chown: %v\n%s", err, out)
	}
	script = `set -e
export HOME=/tmp GIT_CONFIG_GLOBAL=/dev/null
cd /ws
git init -q -b main
git config user.name t; git config user.email t@e
printf 'package a\n' > a.go; git add -A; git commit -q -m base; git rev-parse HEAD > /tmp/base
printf 'package a\nvar x = 1\n' > a.go; git commit -qam head
git rev-parse HEAD
cat /tmp/base
`
	out, err := docker(t, "run", "--rm", "--user", "4242:4242", "-v", vol+":/ws", "--entrypoint", "sh", image, "-c", script)
	if err != nil {
		t.Fatalf("populate: %v\n%s", err, out)
	}
	shas := strings.Fields(out)
	if len(shas) < 2 {
		t.Fatalf("unexpected populate output %q", out)
	}
	head, base := shas[len(shas)-2], shas[len(shas)-1]

	env := inputs(base, head, "http://127.0.0.1:1")
	env["INPUT_COMMENT"] = "false"

	// negative control: no system gitconfig => the ownership check bites, proving the test means something
	control := runAction(t, vol, mergeEnv(env, map[string]string{"GIT_CONFIG_SYSTEM": "/dev/null"}))
	if control.exit == 0 || !strings.Contains(control.log, "dubious ownership") {
		t.Fatalf("control run should fail with dubious ownership, got exit=%d\n%s", control.exit, control.log)
	}

	res := runAction(t, vol, env)
	if res.exit != 0 || res.outputs["added"] != "1" || res.outputs["files-changed"] != "1" {
		t.Errorf("exit=%d outputs=%v\n%s", res.exit, res.outputs, res.log)
	}
}

func mergeEnv(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// TLS and CA certificates: talk to the real api.github.com with a bogus token and expect a clean 401.
func TestImageTalksTLSToGitHub(t *testing.T) {
	if out, err := exec.Command("curl", "-sS", "-m", "10", "-o", os.DevNull, "https://api.github.com/zen").CombinedOutput(); err != nil {
		t.Skipf("api.github.com unreachable: %v %s", err, out)
	}
	dir, base, head := newRepo(t)
	env := inputs(base, head, "https://api.github.com")
	env["INPUT_GITHUB-TOKEN"] = "ghp_not_a_real_token"
	env["GITHUB_REPOSITORY"] = "actions/checkout"
	res := runAction(t, dir, env)
	if res.exit != 0 || res.outputs["comment-status"] != "skipped" || !strings.Contains(res.log, "HTTP 401") {
		t.Errorf("exit=%d outputs=%v\n%s", res.exit, res.outputs, res.log)
	}
	if strings.Contains(res.log, "x509") || strings.Contains(res.log, "certificate") {
		t.Errorf("TLS verification problem:\n%s", res.log)
	}
}

func TestImageShape(t *testing.T) {
	out, err := docker(t, "run", "--rm", "--entrypoint", "sh", image, "-c",
		"id -u; git --version; ls /etc/ssl/certs/ca-certificates.crt; command -v go || echo no-go; git config --system --get-all safe.directory")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"0\n", "git version", "ca-certificates.crt", "no-go", "/github/workspace\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("image check lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n*\n") || strings.HasSuffix(strings.TrimSpace(out), "*") {
		t.Errorf("safe.directory must not be a wildcard:\n%s", out)
	}
	size, _ := docker(t, "image", "inspect", image, "--format", "{{.Size}}")
	t.Logf("image size: %s bytes", strings.TrimSpace(size))
}

// V-01 end to end: ~20 KB of newlines in a Java file used to keep the action busy for about a
// minute (and 1 MiB of them for hours). The worker is cut off after the per-file time limit,
// the file counts as Other, and the run finishes by itself.
func TestImageSurvivesHostileLexerInput(t *testing.T) {
	dir, base, _ := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "Main.java"), []byte(strings.Repeat("\n", 40_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.go"), []byte("package a\n// fine\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "hostile")
	head := git(t, dir, "rev-parse", "HEAD")

	began := time.Now()
	env := inputs(base, head, "http://127.0.0.1:1")
	env["INPUT_COMMENT"], env["INPUT_GITHUB-TOKEN"] = "false", ""
	res := runAction(t, dir, env)
	took := time.Since(began)
	if res.exit != 0 || took > 90*time.Second {
		t.Fatalf("exit=%d after %s\n%s", res.exit, took, res.log)
	}
	if !strings.Contains(res.log, "time limit") || !strings.Contains(res.log, "| 📄 Other | 40001 | 0 |") {
		t.Errorf("hostile file not degraded to Other:\n%s", res.log)
	}
	if !strings.Contains(res.log, "| 💬 Comments | 2 | 0 |") { // README-independent: ok.go still lexed after the restart
		t.Errorf("later files were not classified after the worker restart:\n%s", res.log)
	}
}

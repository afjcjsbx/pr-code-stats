package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const (
	marker = "<!-- pr-code-stats:report:v1 -->"
	token  = "ghs_SECRET_TOKEN_VALUE"
)

type fakeComment struct {
	ID    int64
	Body  string
	Login string
	Type  string
}

// fakeGitHub implements the three endpoints we use, for PR 7 of acme/widgets.
type fakeGitHub struct {
	mu       sync.Mutex
	comments []fakeComment
	requests []string
	fail     func(r *http.Request) (status int, header http.Header, body string) // optional error injection
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())

	if r.Header.Get("Authorization") != "Bearer "+token ||
		r.Header.Get("Accept") != "application/vnd.github+json" ||
		r.Header.Get("X-GitHub-Api-Version") != APIVersion || r.Header.Get("User-Agent") == "" {
		http.Error(w, `{"message":"bad headers"}`, 400)
		return
	}
	if f.fail != nil {
		if status, h, body := f.fail(r); status != 0 {
			for k, v := range h {
				w.Header()[k] = v
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
	}
	render := func(c fakeComment) map[string]any {
		return map[string]any{
			"id": c.ID, "body": c.Body, "html_url": fmt.Sprintf("https://github.test/acme/widgets/pull/7#issuecomment-%d", c.ID),
			"user": map[string]any{"login": c.Login, "type": c.Type},
		}
	}
	const list = "/repos/acme/widgets/issues/7/comments"
	switch {
	case r.Method == "GET" && r.URL.Path == list:
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if page < 1 || per != 100 {
			http.Error(w, `{"message":"bad paging"}`, 400)
			return
		}
		out := []map[string]any{}
		for i := (page - 1) * per; i < len(f.comments) && i < page*per; i++ {
			out = append(out, render(f.comments[i]))
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "POST" && r.URL.Path == list:
		var in struct{ Body string }
		json.NewDecoder(r.Body).Decode(&in)
		c := fakeComment{ID: int64(1000 + len(f.comments)), Body: in.Body, Login: "github-actions[bot]", Type: "Bot"}
		f.comments = append(f.comments, c)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(render(c))
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/issues/comments/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/issues/comments/"), 10, 64)
		var in struct{ Body string }
		json.NewDecoder(r.Body).Decode(&in)
		for i := range f.comments {
			if f.comments[i].ID == id {
				f.comments[i].Body = in.Body
				json.NewEncoder(w).Encode(render(f.comments[i]))
				return
			}
		}
		http.Error(w, `{"message":"Not Found"}`, 404)
	default:
		http.Error(w, `{"message":"unexpected route"}`, 404)
	}
}

func setup(t *testing.T, comments ...fakeComment) (*Client, *fakeGitHub) {
	t.Helper()
	f := &fakeGitHub{comments: comments}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, token, false) // loopback http is allowed without the escape hatch
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func upsert(c *Client) (string, string, []string, error) {
	return c.Upsert(context.Background(), "acme/widgets", 7, marker, marker+"\nbody")
}

func TestUpsertCreatesWhenMissing(t *testing.T) {
	c, f := setup(t, fakeComment{1, "LGTM", "alice", "User"})
	out, url, _, err := upsert(c)
	if err != nil || out != Created {
		t.Fatalf("outcome=%q err=%v", out, err)
	}
	if !strings.HasSuffix(url, "#issuecomment-1001") {
		t.Errorf("url = %q", url)
	}
	if len(f.comments) != 2 || f.comments[1].Body != marker+"\nbody" {
		t.Errorf("comments: %+v", f.comments)
	}
}

func TestUpsertUpdatesExistingNotDuplicates(t *testing.T) {
	c, f := setup(t,
		fakeComment{1, "hello", "alice", "User"},
		fakeComment{2, marker + "\nold report", "github-actions[bot]", "Bot"},
	)
	for i := 0; i < 3; i++ {
		out, url, _, err := upsert(c)
		if err != nil || out != Updated || !strings.HasSuffix(url, "#issuecomment-2") {
			t.Fatalf("run %d: outcome=%q url=%q err=%v", i, out, url, err)
		}
	}
	if len(f.comments) != 2 || f.comments[1].Body != marker+"\nbody" || f.comments[0].Body != "hello" {
		t.Errorf("comments: %+v", f.comments)
	}
}

func TestUpsertNeverTouchesHumanComments(t *testing.T) {
	quoted := "> " + marker + "\nlook, the bot says..."
	c, f := setup(t, fakeComment{1, quoted, "mallory", "User"})
	out, _, _, err := upsert(c)
	if err != nil || out != Created {
		t.Fatalf("outcome=%q err=%v", out, err)
	}
	if f.comments[0].Body != quoted {
		t.Error("human comment was modified")
	}
	for _, r := range f.requests {
		if strings.HasPrefix(r, "PATCH") {
			t.Errorf("unexpected %s", r)
		}
	}
}

func TestUpsertPaginates(t *testing.T) {
	var cs []fakeComment
	for i := 1; i <= 250; i++ {
		cs = append(cs, fakeComment{int64(i), fmt.Sprintf("c%d", i), "alice", "User"})
	}
	cs[220] = fakeComment{221, marker + "\nold", "github-actions[bot]", "Bot"} // page 3
	c, f := setup(t, cs...)
	out, _, _, err := upsert(c)
	if err != nil || out != Updated {
		t.Fatalf("outcome=%q err=%v", out, err)
	}
	if len(f.comments) != 250 {
		t.Errorf("comment count changed to %d", len(f.comments))
	}
	pages := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, "GET") {
			pages++
		}
	}
	if pages != 3 {
		t.Errorf("fetched %d pages, want 3", pages)
	}
}

func TestListCommentsExactMultipleOfPageSize(t *testing.T) {
	var cs []fakeComment
	for i := 1; i <= 200; i++ {
		cs = append(cs, fakeComment{int64(i), "x", "alice", "User"})
	}
	c, _ := setup(t, cs...)
	n := 0
	err := c.eachComment(context.Background(), "acme/widgets", 7, func(Comment) { n++ })
	if err != nil || n != 200 {
		t.Fatalf("got %d comments, err=%v", n, err)
	}
}

func TestUpsertDuplicateReportsUpdatesNewest(t *testing.T) {
	c, f := setup(t,
		fakeComment{5, marker + "\nA", "github-actions[bot]", "Bot"},
		fakeComment{9, marker + "\nB", "github-actions[bot]", "Bot"},
		fakeComment{7, marker + "\nC", "github-actions[bot]", "Bot"},
	)
	out, _, notes, err := upsert(c)
	if err != nil || out != Updated {
		t.Fatalf("outcome=%q err=%v", out, err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "#9") {
		t.Errorf("notes = %v", notes)
	}
	if f.comments[1].Body != marker+"\nbody" || f.comments[0].Body != marker+"\nA" || f.comments[2].Body != marker+"\nC" {
		t.Errorf("wrong comment updated: %+v", f.comments)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		header   http.Header
		body     string
		wantMsg  string
		wantRate bool
	}{
		{"401", 401, nil, `{"message":"Bad credentials"}`, "Bad credentials", false},
		{"403 permissions", 403, nil, `{"message":"Resource not accessible by integration"}`, "Resource not accessible by integration", false},
		{"403 rate limit header", 403, http.Header{"X-Ratelimit-Remaining": {"0"}}, `{"message":"API rate limit exceeded"}`, "rate limit", true},
		{"403 secondary rate limit", 403, nil, `{"message":"You have exceeded a secondary rate limit"}`, "secondary rate limit", true},
		{"429", 429, http.Header{"Retry-After": {"30"}}, `{"message":"slow down"}`, "slow down", true},
		{"404", 404, nil, `{"message":"Not Found"}`, "Not Found", false},
		{"422", 422, nil, `{"message":"Validation Failed"}`, "Validation Failed", false},
		{"500 non-json", 500, nil, `<html>oops</html>`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f := setup(t)
			f.fail = func(r *http.Request) (int, http.Header, string) { return tc.status, tc.header, tc.body }
			_, _, _, err := upsert(c)
			ae, ok := err.(*APIError)
			if !ok {
				t.Fatalf("err = %T %v, want *APIError", err, err)
			}
			if ae.Status != tc.status || ae.RateLimited != tc.wantRate || !strings.Contains(ae.Error(), tc.wantMsg) {
				t.Errorf("got %+v (%q), want status %d rate=%v msg~%q", ae, ae.Error(), tc.status, tc.wantRate, tc.wantMsg)
			}
			if strings.Contains(err.Error(), token) {
				t.Error("token leaked in error")
			}
		})
	}
}

func TestErrorOnWritePhase(t *testing.T) {
	// listing works (read-only fork token) but POST is refused
	c, f := setup(t)
	f.fail = func(r *http.Request) (int, http.Header, string) {
		if r.Method == "POST" {
			return 403, nil, `{"message":"Resource not accessible by integration"}`
		}
		return 0, nil, ""
	}
	_, _, _, err := upsert(c)
	if ae, ok := err.(*APIError); !ok || ae.Status != 403 || ae.RateLimited {
		t.Fatalf("err = %v", err)
	}
}

func TestNetworkErrorDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	c, err := New(url, token, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = c.Upsert(context.Background(), "acme/widgets", 7, marker, "x")
	if err == nil {
		t.Fatal("expected network error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("token leaked: %v", err)
	}
}

func TestBadRepository(t *testing.T) {
	c, err := New("https://unused.invalid", token, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"", "acme", "acme/", "/widgets", "a/b/c"} {
		if err := c.eachComment(context.Background(), repo, 1, func(Comment) {}); err == nil {
			t.Errorf("repo %q accepted", repo)
		}
	}
}

// V-09: the token must only ever travel over https (or loopback), and never leave the API origin.
func TestNewRequiresHTTPS(t *testing.T) {
	ok := []struct {
		url      string
		insecure bool
	}{
		{"https://api.github.com", false},
		{"https://ghe.example.com/api/v3/", false},
		{"http://127.0.0.1:8080", false},
		{"http://localhost:8080", false},
		{"http://[::1]:8080", false},
		{"http://ghe.internal", true}, // explicit opt-in
	}
	for _, c := range ok {
		if _, err := New(c.url, token, c.insecure); err != nil {
			t.Errorf("%q (insecure=%v) rejected: %v", c.url, c.insecure, err)
		}
	}
	bad := []string{
		"http://api.github.com", "http://ghe.internal", "ftp://api.github.com", "file:///etc/passwd",
		"api.github.com", "", "https://", "https://user:pass@api.github.com", "https://api.github.com?x=1",
		"https://api.github.com#frag", "http://127.0.0.1.evil.com",
	}
	for _, u := range bad {
		if c, err := New(u, token, false); err == nil {
			t.Errorf("%q accepted: %+v", u, c)
		} else if strings.Contains(err.Error(), "pass") {
			t.Errorf("error leaks the credentials in %q: %v", u, err)
		}
	}
}

func TestRedirectsNeverLeaveTheAPIOrigin(t *testing.T) {
	var seen []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Write([]byte("[]"))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect) // same host, other port
	}))
	defer origin.Close()
	c, _ := New(origin.URL, token, false)
	err := c.eachComment(context.Background(), "acme/widgets", 7, func(Comment) {})
	if err == nil || !strings.Contains(err.Error(), "refusing to follow") {
		t.Errorf("err = %v", err)
	}
	if len(seen) != 0 {
		t.Errorf("the redirect target received the request with Authorization %q", seen)
	}
}

func TestSameOriginRedirectIsFollowed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repositories/1/issues/7/comments?"+r.URL.RawQuery, http.StatusMovedPermanently)
	})
	mux.HandleFunc("/repositories/1/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no auth", 401)
			return
		}
		w.Write([]byte("[]"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c, _ := New(srv.URL, token, false)
	if err := c.eachComment(context.Background(), "acme/widgets", 7, func(Comment) {}); err != nil {
		t.Errorf("renamed-repo style redirect failed: %v", err)
	}
}

// V-09: comments are streamed, never accumulated, and a runaway listing stops.
func TestEachCommentStopsAtMaxPages(t *testing.T) {
	old := maxPages
	maxPages = 3
	defer func() { maxPages = old }()
	c, f := setup(t)
	for i := 1; i <= 1000; i++ {
		f.comments = append(f.comments, fakeComment{int64(i), "x", "alice", "User"})
	}
	n := 0
	err := c.eachComment(context.Background(), "acme/widgets", 7, func(Comment) { n++ })
	if err == nil || !strings.Contains(err.Error(), "more than 300 comments") || n != 300 {
		t.Errorf("n=%d err=%v", n, err)
	}
}

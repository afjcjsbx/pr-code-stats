// Package github is a minimal GitHub REST client: just enough to find and write one PR comment.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIVersion is sent as X-GitHub-Api-Version so behaviour does not change under us.
const APIVersion = "2022-11-28"

// Client talks to one GitHub API host. The token only ever goes into the Authorization header;
// it is never part of a URL, an error or a log line.
type Client struct {
	BaseURL string // e.g. https://api.github.com (no trailing slash)
	Token   string
	HTTP    *http.Client
}

// New returns a client with a sane timeout. The API URL must be https (the token travels in
// every request); plain http is accepted only for loopback hosts, or everywhere if allowInsecure
// is set, which exists for tests and for an http-only GitHub Enterprise Server.
// Redirects are followed only within the same origin, so the token never leaves the API host.
func New(baseURL, token string, allowInsecure bool) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("invalid GitHub API URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid GitHub API URL %q: credentials, query and fragment are not allowed", u.Redacted())
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && (allowInsecure || isLoopback(u.Hostname())):
	default:
		return nil, fmt.Errorf("GitHub API URL %q must use https", u.Redacted())
	}
	origin := u.Scheme + "://" + u.Host
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return errors.New("too many redirects")
				}
				if req.URL.Scheme+"://"+req.URL.Host != origin {
					return fmt.Errorf("refusing to follow a redirect to %s://%s", req.URL.Scheme, req.URL.Host)
				}
				return nil
			},
		},
	}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// APIError is a non-2xx answer.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
	RateLimited  bool
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("GitHub API %s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.RateLimited {
		msg += " (rate limited, retry later)"
	}
	return msg
}

// do sends one request and decodes a 2xx JSON answer into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("User-Agent", "pr-code-stats")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// *url.Error holds method and URL only, never headers.
		return fmt.Errorf("GitHub API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return apiError(method, path, resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil {
		return fmt.Errorf("GitHub API %s %s: decoding response: %w", method, path, err)
	}
	return nil
}

func apiError(method, path string, resp *http.Response) *APIError {
	e := &APIError{Method: method, Path: path, Status: resp.StatusCode}
	var payload struct {
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(raw, &payload) == nil {
		e.Message = payload.Message
	}
	e.RateLimited = resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusForbidden &&
			(resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" ||
				strings.Contains(strings.ToLower(e.Message), "rate limit"))
	return e
}

package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Outcome of Upsert.
const (
	Created = "created"
	Updated = "updated"
)

// Comment is the subset of an issue comment we use.
type Comment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

const perPage = 100

func issuesPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("repository %q is not in owner/name form", repo)
	}
	return fmt.Sprintf("/repos/%s/%s/issues", url.PathEscape(owner), url.PathEscape(name)), nil
}

// maxPages stops a runaway listing: 1000 pages are 100,000 comments.
var maxPages = 1000

// eachComment calls fn for every comment of the PR, page by page. Nothing is accumulated, so a
// PR flooded with large comments by a third party costs requests, not memory.
func (c *Client) eachComment(ctx context.Context, repo string, pr int, fn func(Comment)) error {
	base, err := issuesPath(repo)
	if err != nil {
		return err
	}
	for page := 1; page <= maxPages; page++ {
		var batch []Comment
		path := fmt.Sprintf("%s/%d/comments?per_page=%d&page=%d", base, pr, perPage, page)
		if err := c.do(ctx, "GET", path, nil, &batch); err != nil {
			return err
		}
		for _, cm := range batch {
			fn(cm)
		}
		if len(batch) < perPage {
			return nil
		}
	}
	return fmt.Errorf("the PR has more than %d comments", maxPages*perPage)
}

// Upsert publishes body as the PR's report comment: it updates the newest bot comment that
// contains marker, or creates one. Comments written by people are never touched, even if they
// quote the marker. It returns what happened, the comment URL, and notes worth logging.
func (c *Client) Upsert(ctx context.Context, repo string, pr int, marker, body string) (outcome, commentURL string, notes []string, err error) {
	base, err := issuesPath(repo)
	if err != nil {
		return "", "", nil, err
	}
	var found *Comment
	matches := 0
	err = c.eachComment(ctx, repo, pr, func(cm Comment) {
		if cm.User.Type != "Bot" || !strings.Contains(cm.Body, marker) {
			return
		}
		matches++
		if found == nil || cm.ID > found.ID {
			found = &cm
		}
	})
	if err != nil {
		return "", "", nil, err
	}
	if matches > 1 {
		notes = append(notes, fmt.Sprintf("found %d report comments from bots; updating the newest (#%d) and leaving the others", matches, found.ID))
	}

	var saved Comment
	if found != nil {
		path := fmt.Sprintf("%s/comments/%d", base, found.ID)
		if err := c.do(ctx, "PATCH", path, map[string]string{"body": body}, &saved); err != nil {
			return "", "", notes, err
		}
		return Updated, saved.HTMLURL, notes, nil
	}
	path := fmt.Sprintf("%s/%d/comments", base, pr)
	if err := c.do(ctx, "POST", path, map[string]string{"body": body}, &saved); err != nil {
		return "", "", notes, err
	}
	return Created, saved.HTMLURL, notes, nil
}

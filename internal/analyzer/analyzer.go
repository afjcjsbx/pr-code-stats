// Package analyzer measures what a pull request changes: lines added and removed, split into
// production code, tests, comments and other.
package analyzer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/afjcjsbx/pr-code-stats/internal/config"
)

// Counter holds independent added/removed totals.
type Counter struct{ Added, Removed int }

// Stats splits the diff into mutually exclusive categories.
type Stats struct{ Code, Tests, Comments, Other Counter }

// Total is the sum of the categories, so the totals can never disagree with the breakdown.
func (s Stats) Total() Counter {
	return Counter{
		Added:   s.Code.Added + s.Tests.Added + s.Comments.Added + s.Other.Added,
		Removed: s.Code.Removed + s.Tests.Removed + s.Comments.Removed + s.Other.Removed,
	}
}

// Net is added - removed.
func (s Stats) Net() int { t := s.Total(); return t.Added - t.Removed }

// Result is the outcome of Analyze.
type Result struct {
	Stats        Stats
	FilesChanged int      // files with at least one non-excluded side, binaries included
	Degraded     int      // files whose lines were forced into Other (too big, lexer failure)
	Warnings     []string // human-readable details; may contain paths (untrusted)
}

type category int

const (
	catCode category = iota
	catTests
	catComments
	catOther
)

func (s *Stats) counter(c category) *Counter {
	switch c {
	case catCode:
		return &s.Code
	case catTests:
		return &s.Tests
	case catComments:
		return &s.Comments
	}
	return &s.Other
}

// side is one version (old or new) of a changed file.
type side struct {
	path, blob string
	active     bool       // exists and is not excluded
	loaded     bool       // kinds/test below are valid
	kinds      []lineKind // nil: every line is Other
	test       bool
}

func (s *side) category(line int) category {
	if s.kinds == nil {
		return catOther
	}
	switch {
	case s.kinds[line-1] == lineComment:
		return catComments
	case s.test:
		return catTests
	}
	return catCode
}

type analyzer struct {
	cfg   *config.Config
	cls   *classifier
	files *fileCache
	res   *Result
}

// Analyze compares the PR branch with the point it forked from. base is the base branch tip
// as reported by GitHub; the diff starts at merge-base(base, head), exactly like the PR's
// "Files changed" tab, so commits that landed on the base branch afterwards are not counted.
func Analyze(ctx context.Context, repo Repo, baseSHA, headSHA string, cfg *config.Config) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, analysisTimeout)
	defer cancel()
	res, err := runAnalysis(ctx, repo, baseSHA, headSHA, cfg)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("the analysis exceeded its time limit of %s: %w", analysisTimeout, err)
	}
	return res, err
}

// analysisTimeout bounds one whole analysis (git, parsing, lexing). A var so tests can shorten it.
var analysisTimeout = 15 * time.Minute

func runAnalysis(ctx context.Context, repo Repo, baseSHA, headSHA string, cfg *config.Config) (*Result, error) {
	if err := repo.VerifyCommit(ctx, "base", baseSHA); err != nil {
		return nil, err
	}
	if err := repo.VerifyCommit(ctx, "head", headSHA); err != nil {
		return nil, err
	}
	mergeBase, err := repo.MergeBase(ctx, baseSHA, headSHA)
	if err != nil {
		return nil, err
	}
	changes, err := repo.diff(ctx, baseSHA, mergeBase, headSHA)
	if err != nil {
		return nil, err
	}
	blobs, err := repo.openBlobs(ctx)
	if err != nil {
		return nil, err
	}
	pool := newLexPool()
	defer pool.close()
	a := &analyzer{cfg: cfg, cls: newClassifier(cfg), files: newFileCache(blobs, pool, cfg.MaxFileSize), res: &Result{}}

	var rawAdded, rawRemoved int
	for i := range changes {
		add, rem, err := a.addChange(&changes[i])
		if err != nil {
			_ = blobs.close()
			return nil, err
		}
		rawAdded += add
		rawRemoved += rem
	}
	if err := blobs.close(); err != nil {
		return nil, gitError("cat-file", err, blobs.stderr.String())
	}
	// Invariant: categories add up to what the hunks say.
	if t := a.res.Stats.Total(); t.Added != rawAdded || t.Removed != rawRemoved {
		return nil, fmt.Errorf("internal error: categories sum to +%d/-%d but the diff has +%d/-%d",
			t.Added, t.Removed, rawAdded, rawRemoved)
	}
	return a.res, nil
}

// addChange counts one file; it returns the raw added/removed lines it accounted for.
func (a *analyzer) addChange(ch *fileChange) (added, removed int, err error) {
	old := &side{path: ch.OldPath, blob: ch.OldBlob, active: ch.OldPath != "" && !a.cfg.Exclude.Match(ch.OldPath)}
	cur := &side{path: ch.NewPath, blob: ch.NewBlob, active: ch.NewPath != "" && !a.cfg.Exclude.Match(ch.NewPath)}
	if !old.active && !cur.active {
		return 0, 0, nil
	}
	a.res.FilesChanged++
	if ch.Binary || ch.gitlink() {
		return 0, 0, nil
	}

	degraded := false
	for _, h := range ch.Hunks {
		if old.active && h.OldLines > 0 {
			removed += h.OldLines
			if err := a.count(old, h.OldStart, h.OldLines, true, &degraded); err != nil {
				return 0, 0, err
			}
		}
		if cur.active && h.NewLines > 0 {
			added += h.NewLines
			if err := a.count(cur, h.NewStart, h.NewLines, false, &degraded); err != nil {
				return 0, 0, err
			}
		}
	}
	if degraded {
		a.res.Degraded++
	}
	return added, removed, nil
}

// count attributes lines [start, start+n) of one side to categories.
func (a *analyzer) count(s *side, start, n int, removed bool, degraded *bool) error {
	if !s.loaded {
		fc := a.cls.forPath(s.path)
		s.test = fc.test
		if fc.lexer != nil {
			kinds, reason, err := a.files.kinds(s.blob, s.path)
			if err != nil {
				return fmt.Errorf("%s: %w", s.path, err)
			}
			if reason != "" {
				*degraded = true
				a.res.Warnings = append(a.res.Warnings, fmt.Sprintf("%s: %s; its lines are counted as Other", s.path, reason))
			} else {
				s.kinds = kinds
			}
		}
		s.loaded = true
	}
	if s.kinds != nil && start+n-1 > len(s.kinds) {
		return fmt.Errorf("%s: diff touches line %d but the file has %d lines", s.path, start+n-1, len(s.kinds))
	}
	if start < 1 {
		return errors.New(s.path + ": diff reports a line before the start of the file")
	}
	for l := start; l < start+n; l++ {
		c := a.res.Stats.counter(s.category(l))
		if removed {
			c.Removed++
		} else {
			c.Added++
		}
	}
	return nil
}

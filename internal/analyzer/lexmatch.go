package analyzer

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// matchLexer is lexers.Match(base) with the same result, hundreds of times faster: chroma runs
// filepath.Match, plus once per ignored suffix, against every glob of every lexer (3-5 ms per
// file name), which a pull request with 100k distinct file names turns into minutes of CPU.
// Here each glob is classified once and the common shapes ("name", "*.ext") become plain
// string comparisons. TestMatchLexerEquivalence checks it against chroma.
func matchLexer(base string) chroma.Lexer {
	matchIndexOnce.Do(buildMatchIndex)
	// chroma also tries every glob with each ignored suffix appended ("*.go" matches "x.go.in").
	// Matching glob+suffix against base is the same as matching glob against base without that
	// suffix, so the candidates are computed once here instead of once per glob.
	names := []string{base}
	for _, suf := range ignoredSuffixes {
		if strings.HasSuffix(base, suf) {
			names = append(names, strings.TrimSuffix(base, suf))
		}
	}
	for _, phase := range [][]globEntry{matchPrimary, matchAlias} {
		var matched chroma.PrioritisedLexers
		for i := range phase {
			if phase[i].matchesAny(names) {
				matched = append(matched, phase[i].lexer)
			}
		}
		if len(matched) > 0 {
			sort.Sort(matched) // same ordering as chroma.LexerRegistry.Match
			return matched[0]
		}
	}
	return nil
}

// ignoredSuffixes mirrors chroma v2.27.0 registry.go; the equivalence test covers each of them.
var ignoredSuffixes = []string{
	"~", ".bak", ".old", ".orig",
	".dpkg-dist", ".dpkg-old", ".ucf-dist", ".ucf-new", ".ucf-old",
	".rpmnew", ".rpmorig", ".rpmsave",
	".in",
}

type globKind uint8

const (
	globLiteral globKind = iota // "Makefile"
	globSuffix                  // "*.go": any name ending in lit
	globComplex                 // anything else: ask filepath.Match
)

type globEntry struct {
	lexer chroma.Lexer
	kind  globKind
	glob  string // the whole glob (complex) or the literal part
}

var (
	matchIndexOnce sync.Once
	matchPrimary   []globEntry
	matchAlias     []globEntry
)

const globMeta = `*?[\`

func buildMatchIndex() {
	add := func(dst *[]globEntry, l chroma.Lexer, glob string) {
		e := globEntry{lexer: l, kind: globComplex, glob: glob}
		switch {
		case !strings.ContainsAny(glob, globMeta):
			e.kind = globLiteral
		case glob[0] == '*' && !strings.ContainsAny(glob[1:], globMeta):
			e.kind, e.glob = globSuffix, glob[1:]
		}
		*dst = append(*dst, e)
	}
	for _, l := range lexers.GlobalLexerRegistry.Lexers {
		cfg := l.Config()
		for _, g := range cfg.Filenames {
			add(&matchPrimary, l, g)
		}
		for _, g := range cfg.AliasFilenames {
			add(&matchAlias, l, g)
		}
	}
}

func (e *globEntry) matchesAny(names []string) bool {
	for _, n := range names {
		switch e.kind {
		case globLiteral:
			if n == e.glob {
				return true
			}
		case globSuffix:
			if strings.HasSuffix(n, e.glob) {
				return true
			}
		default:
			if ok, _ := filepath.Match(e.glob, n); ok {
				return true
			}
		}
	}
	return false
}

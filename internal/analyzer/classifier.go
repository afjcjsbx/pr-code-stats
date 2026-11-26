package analyzer

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"github.com/alecthomas/chroma/v2"

	"github.com/afjcjsbx/pr-code-stats/internal/config"
)

// lineKind is what the lexer says a physical line contains.
type lineKind uint8

const (
	lineBlank   lineKind = iota // nothing, or whitespace outside any comment
	lineCode                    // at least one non-whitespace code token (inline comments don't change this)
	lineComment                 // only comment tokens, or blank inside a comment
)

// nonSourceLexers are lexers (lowercased names) for documentation, data and configuration
// formats. A file they match is "Other" even though Chroma can tokenise it.
var nonSourceLexers = map[string]bool{
	"markdown": true, "plaintext": true, "restructuredtext": true, "org mode": true, "gemtext": true,
	"markless": true, "scdoc": true, "bibtex": true, "groff": true,
	"json": true, "yaml": true, "yaml+jinja": true, "toml": true, "ini": true, "properties": true,
	"xml": true, "csv": true, "diff": true, "gettext": true, "hexdump": true, "dtd": true,
	"gemfile.lock": true, "desktop file": true, "protocol buffer text format": true,
	"termcap": true, "terminfo": true, "webvtt": true, "dns": true, "reg": true, "http": true,
	"apacheconf": true, "nginx configuration file": true, "lighttpd configuration file": true,
	"squidconf": true, "pacmanconf": true, "pkgconfig": true, "systemd": true, "xorg": true,
	"bash session": true,
}

// fileClass says how the lines of one path are categorised.
type fileClass struct {
	lexer chroma.Lexer // nil: not source, everything is Other
	test  bool
}

type classifier struct{ cfg *config.Config }

func newClassifier(cfg *config.Config) *classifier { return &classifier{cfg: cfg} }

func (c *classifier) forPath(p string) fileClass {
	return fileClass{
		lexer: sourceLexer(path.Base(p)),
		test:  c.cfg.TestDirectories.Match(p) || c.cfg.TestFilePatterns.Match(p),
	}
}

func (c *classifier) lexer(base string) chroma.Lexer { return sourceLexer(base) }

// sourceLexer returns the source-code lexer for a file name, or nil for documentation, data,
// configuration and unknown files. The parent process and the lexer worker both call it.
func sourceLexer(base string) chroma.Lexer {
	switch {
	case base == "go.mod" || base == "go.work" || strings.HasSuffix(base, ".lock"):
		return nil // Chroma maps go.mod to AMPL; lock files are generated data
	}
	if m := matchLexer(base); m != nil && !nonSourceLexers[strings.ToLower(m.Config().Name)] {
		return m
	}
	return nil
}

func countLines(b []byte) int {
	n := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// isComment: comment tokens, except preprocessor directives (#include, #define) which are code.
func isComment(t chroma.TokenType) bool {
	return t.InCategory(chroma.Comment) && t != chroma.CommentPreproc && t != chroma.CommentPreprocFile
}

// classifyLines lexes content and labels every line, numbered like git does (by "\n").
// The decision is the lexer's, never a text search: "//" inside a string is code.
func classifyLines(lexer chroma.Lexer, content []byte) (kinds []lineKind, err error) {
	defer func() {
		if r := recover(); r != nil {
			kinds, err = nil, fmt.Errorf("lexer panic: %v", r)
		}
	}()
	n := countLines(content)
	if n == 0 {
		return nil, nil
	}
	// CRLF -> LF so "\r" never leaks into tokens; line numbering is unchanged.
	text := string(bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n")))
	it, err := lexer.Tokenise(&chroma.TokeniseOptions{State: "root"}, text)
	if err != nil {
		return nil, err
	}

	type info struct{ code, comment, nlComment bool }
	infos := make([]info, n)
	line := 0
	for tok := it(); tok != chroma.EOF; tok = it() {
		comment := isComment(tok.Type)
		v := tok.Value
		for {
			i := strings.IndexByte(v, '\n')
			seg := v
			if i >= 0 {
				seg = v[:i]
			}
			if line < n && strings.TrimSpace(seg) != "" {
				if comment {
					infos[line].comment = true
				} else {
					infos[line].code = true
				}
			}
			if i < 0 {
				break
			}
			if line < n {
				infos[line].nlComment = comment // does this line's newline belong to a comment token?
			}
			line++
			v = v[i+1:]
		}
	}
	// A lexer that drops or invents text would shift every later line: refuse to guess.
	if line != n && line != n-1 {
		return nil, fmt.Errorf("lexer output has %d lines, file has %d", line, n)
	}

	kinds = make([]lineKind, n)
	for i, in := range infos {
		switch {
		case in.code:
			kinds[i] = lineCode
		case in.comment:
			kinds[i] = lineComment
		case i > 0 && infos[i-1].nlComment && in.nlComment:
			kinds[i] = lineComment // blank line swallowed by a multi-line comment token
		}
	}
	return kinds, nil
}

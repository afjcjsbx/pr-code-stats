package analyzer

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/afjcjsbx/pr-code-stats/internal/config"
)

// categories labels every line of content the way Analyze would for a file at path.
func categories(t *testing.T, path, content string) []category {
	t.Helper()
	fc := newClassifier(config.Default()).forPath(path)
	s := &side{test: fc.test}
	if fc.lexer != nil {
		kinds, err := classifyLines(fc.lexer, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		s.kinds = kinds
	}
	n := countLines([]byte(content))
	out := make([]category, n)
	for i := range out {
		out[i] = s.category(i + 1)
	}
	return out
}

func TestClassification(t *testing.T) {
	const (
		P = catCode
		T = catTests
		C = catComments
		O = catOther
	)
	cases := []struct {
		name, path, content string
		want                []category
	}{
		{"go code", "a.go", "package a\nfunc F() {}\n", []category{P, P}},
		{"go code in _test.go", "a_test.go", "package a\nfunc TestF() {}\n", []category{T, T}},
		{"python code", "a.py", "x = 1\ny = 2\n", []category{P, P}},
		{"file under tests/", "tests/helper.py", "x = 1\n", []category{T}},
		{"file under spec/ nested", "pkg/spec/a.rb", "x = 1\n", []category{T}},
		{"pure // comment", "a.go", "// hi\n", []category{C}},
		{"multiline comment with blank line", "a.go", "/* a\n\n b */\n", []category{C, C, C}},
		{"indented blank line inside multiline comment", "a.go", "/* a\n   \n b */\n", []category{C, C, C}},
		{"# comment", "a.py", "# hi\nx = 1\n", []category{C, P}},
		{"inline comment is code", "a.go", "x := 1 // why\n", []category{P}},
		{"inline comment in test file is test", "a_test.go", "x := 1 // why\n", []category{T}},
		{"comment in test file stays a comment", "a_test.go", "// hi\n", []category{C}},
		{"// inside string is code", "a.go", "var u = \"http://x\"\n", []category{P}},
		{"# inside string is code", "a.py", "u = '# not a comment'\n", []category{P}},
		{"code after closing */ on same line", "a.go", "/* a\n b */ x := 1\n", []category{C, P}},
		{"comment opener after code", "a.go", "x := 1 /* a\n b */\n", []category{P, C}},
		{"blank line in prod file", "a.go", "package a\n\nfunc F() {}\n", []category{P, P, P}},
		{"blank line in test file", "a_test.go", "package a\n\nfunc F() {}\n", []category{T, T, T}},
		{"blank line between line comments is not a comment", "a.go", "// a\n\n// b\n", []category{C, P, C}},
		{"markdown is Other", "README.md", "# Title\n\ntext\n", []category{O, O, O}},
		{"yaml is Other, comments too", "ci.yml", "# c\na: 1\n", []category{O, O}},
		{"json is Other", "a.json", "{}\n", []category{O}},
		{"unknown extension is Other", "a.zzz", "whatever\n", []category{O}},
		{"lock file is Other", "Gemfile.lock", "GEM\n", []category{O}},
		{"go.mod is Other", "go.mod", "module x\n", []category{O}},
		{"markdown under tests/ is Other", "tests/README.md", "hi\n", []category{O}},
		{"preprocessor directive is code", "a.c", "#include <stdio.h>\n", []category{P}},
		{"shebang-less shell comment", "run.sh", "# c\necho hi\n", []category{C, P}},
		{"no trailing newline", "a.go", "// c\nx := 1", []category{C, P}},
		{"crlf", "a.go", "// c\r\nx := 1\r\n", []category{C, P}},
		{"empty file", "a.go", "", []category{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := categories(t, tc.path, tc.content); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s %q\n got %v\nwant %v", tc.path, tc.content, got, tc.want)
			}
		})
	}
}

func TestCountLines(t *testing.T) {
	for in, want := range map[string]int{"": 0, "\n": 1, "a": 1, "a\n": 1, "a\nb": 2, "a\n\n": 2, "a\r\nb\r\n": 2} {
		if got := countLines([]byte(in)); got != want {
			t.Errorf("countLines(%q) = %d, want %d", in, got, want)
		}
	}
}

// A lexer must never be able to desynchronise line numbers silently; every language Chroma
// knows by extension should survive CRLF and a missing final newline with the right line count.
func TestClassifyLinesKeepsLineCount(t *testing.T) {
	cl := newClassifier(config.Default())
	for _, name := range []string{"a.go", "a.py", "a.c", "a.cpp", "a.java", "a.js", "a.ts", "a.rb", "a.rs", "a.php", "a.cs", "a.kt", "a.swift", "a.sh", "a.sql", "a.html", "a.css", "Makefile", "Dockerfile", "a.lua", "a.pl"} {
		lexer := cl.lexer(name)
		if lexer == nil {
			t.Errorf("%s: expected a source lexer", name)
			continue
		}
		for _, content := range []string{
			"a\n\nb\nc",
			"a\r\n\r\nb\r\nc\r\n",
			strings.Repeat("x = 1\n\n", 50),
			"/* a\n\n b */\n// x\n# y\n-- z\n",
			"\n\n\n",
		} {
			kinds, err := classifyLines(lexer, []byte(content))
			if err != nil {
				t.Errorf("%s %q: %v", name, content, err)
				continue
			}
			if want := countLines([]byte(content)); len(kinds) != want {
				t.Errorf("%s %q: %d kinds for %d lines", name, content, len(kinds), want)
			}
		}
	}
}

func TestClassifierCachesLexers(t *testing.T) {
	cl := newClassifier(config.Default())
	a, b := cl.lexer("x.go"), cl.lexer("x.go")
	if a == nil || fmt.Sprintf("%p", a) != fmt.Sprintf("%p", b) {
		t.Error("lexer lookup not cached")
	}
}

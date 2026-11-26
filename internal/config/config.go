// Package config loads and validates the optional .github/pr-code-stats.yml file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Version is the only schema version understood by this release.
const Version = 1

// DefaultMaxFileSize is the largest blob (bytes) that is lexed; bigger files are counted as Other.
const DefaultMaxFileSize = 1 << 20

// Limits on the configuration itself. It is read from the base branch, so only maintainers can
// write it, but a typo or a generated file should still not be able to exhaust the runner.
const (
	MaxBytes           = 64 << 10 // size of the YAML file
	MaxPatterns        = 200      // user patterns per list
	MaxPatternLen      = 256      // bytes per pattern
	MaxAllowedFileSize = 16 << 20 // ceiling for max_file_size
)

// Globs is a compiled list of path patterns.
//
// Semantics, matched against slash-separated paths relative to the repository root
// (case-sensitive):
//
//   - any characters except "/"
//     ?      one character except "/"
//     [a-z]  character class ("[!a-z]" negates)
//     **/    zero or more directories
//     **     anything, including "/"
//
// A pattern with no "/" other than a trailing one matches at any depth, like .gitignore
// ("*_test.go" == "**/*_test.go"); a leading "/" anchors it at the repository root.
// A trailing "/" means everything below that directory.
type Globs []*regexp.Regexp

// Match reports whether p matches any pattern.
func (g Globs) Match(p string) bool {
	for _, re := range g {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// Config is the effective configuration: defaults plus the user's additions.
type Config struct {
	TestDirectories  Globs
	TestFilePatterns Globs
	Exclude          Globs
	MaxFileSize      int64
}

type file struct {
	Version int `yaml:"version"`
	Tests   struct {
		Directories  []string `yaml:"directories"`
		FilePatterns []string `yaml:"file_patterns"`
	} `yaml:"tests"`
	Exclude     []string `yaml:"exclude"`
	MaxFileSize *int64   `yaml:"max_file_size"`
}

var (
	defaultTestDirs  = []string{"**/test/**", "**/tests/**", "**/__tests__/**", "**/spec/**", "**/specs/**"}
	defaultTestFiles = []string{
		"*_test.go",
		"*.test.js", "*.test.jsx", "*.test.ts", "*.test.tsx",
		"*.spec.js", "*.spec.jsx", "*.spec.ts", "*.spec.tsx",
		"test_*.py", "*_test.py",
		"*_spec.rb", "*_test.rb",
		"Test*.java", "*Test.java", "*Tests.java", "*TestCase.java",
		"*Test.kt", "*Tests.kt",
		"*Test.cs", "*Tests.cs", "*TestCase.cs",
	}
	defaultExclude = []string{"**/vendor/**", "**/node_modules/**", "**/dist/**", "**/build/**", "**/*.min.js"}
)

// Default returns the configuration used when no file exists.
func Default() *Config {
	c, err := build(&file{})
	if err != nil {
		panic(err) // defaults are static; covered by tests
	}
	return c
}

// Parse validates data and merges it over the defaults. Empty data means defaults.
func Parse(data []byte) (*Config, error) {
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("invalid config: the file is %d bytes, the limit is %d", len(data), MaxBytes)
	}
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return Default(), nil
		}
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if f.Version != Version {
		return nil, fmt.Errorf("invalid config: field %q must be %d (got %d)", "version", Version, f.Version)
	}
	return build(&f)
}

func build(f *file) (*Config, error) {
	c := &Config{MaxFileSize: DefaultMaxFileSize}
	var err error
	if c.TestDirectories, err = compile("tests.directories", defaultTestDirs, f.Tests.Directories); err != nil {
		return nil, err
	}
	if c.TestFilePatterns, err = compile("tests.file_patterns", defaultTestFiles, f.Tests.FilePatterns); err != nil {
		return nil, err
	}
	if c.Exclude, err = compile("exclude", defaultExclude, f.Exclude); err != nil {
		return nil, err
	}
	if f.MaxFileSize != nil {
		if *f.MaxFileSize <= 0 || *f.MaxFileSize > MaxAllowedFileSize {
			return nil, fmt.Errorf("invalid config: field %q must be between 1 and %d (got %d)", "max_file_size", MaxAllowedFileSize, *f.MaxFileSize)
		}
		c.MaxFileSize = *f.MaxFileSize
	}
	return c, nil
}

func compile(field string, defaults, extra []string) (Globs, error) {
	if len(extra) > MaxPatterns {
		return nil, fmt.Errorf("invalid config: field %s has %d patterns, the limit is %d", field, len(extra), MaxPatterns)
	}
	for i, p := range extra {
		if len(p) > MaxPatternLen {
			return nil, fmt.Errorf("invalid config: field %s[%d] is %d bytes long, the limit is %d", field, i, len(p), MaxPatternLen)
		}
	}
	var g Globs
	for i, p := range append(append([]string{}, defaults...), extra...) {
		re, err := compileGlob(p)
		if err != nil {
			// indexes past the defaults refer to the user's list
			if j := i - len(defaults); j >= 0 {
				return nil, fmt.Errorf("invalid config: field %s[%d] (%q): %w", field, j, p, err)
			}
			return nil, fmt.Errorf("invalid built-in pattern %q: %w", p, err)
		}
		g = append(g, re)
	}
	return g, nil
}

func compileGlob(p string) (*regexp.Regexp, error) {
	if strings.TrimSpace(p) == "" {
		return nil, errors.New("empty pattern")
	}
	anchored := strings.HasPrefix(p, "/")
	p = strings.TrimPrefix(p, "/")
	if !anchored && !strings.Contains(strings.TrimSuffix(p, "/"), "/") {
		p = "**/" + p
	}
	if strings.HasSuffix(p, "/") {
		p += "**"
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); {
		switch {
		case strings.HasPrefix(p[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i += 2
		case p[i] == '*':
			b.WriteString("[^/]*")
			i++
		case p[i] == '?':
			b.WriteString("[^/]")
			i++
		case p[i] == '[':
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				return nil, errors.New("unclosed '['")
			}
			class := p[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end + 2
		default:
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
			i++
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("bad pattern: %w", err)
	}
	return re, nil
}

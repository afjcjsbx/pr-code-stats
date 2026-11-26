package config

import (
	"strings"
	"testing"
)

func TestGlobs(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*_test.go", "a_test.go", true},
		{"*_test.go", "pkg/deep/a_test.go", true}, // no "/" => any depth
		{"*_test.go", "a_test.go.bak", false},
		{"**/test/**", "test/a.go", true}, // "**/" matches zero directories
		{"**/test/**", "src/test/a/b.go", true},
		{"**/test/**", "src/latest/a.go", false},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/sub/a.go", false}, // "*" stops at "/"
		{"src/**/*.go", "src/a.go", true},
		{"src/**/*.go", "src/x/y/a.go", true},
		{"vendor/", "vendor/x/y.go", true}, // trailing "/" == directory contents
		{"vendor/", "a/vendor/y.go", true},
		{"/vendor/", "vendor/y.go", true},
		{"/vendor/", "a/vendor/y.go", false}, // leading "/" anchors at root
		{"file?.txt", "file1.txt", true},
		{"file?.txt", "file12.txt", false},
		{"[!a]*.go", "b.go", true},
		{"[!a]*.go", "a.go", false},
		{"dir/ünï*", "dir/ünïcode", true},
		{"a.min.js", "x/a.min.js", true},
		{"a+b.go", "a+b.go", true}, // regex metacharacters are literal
		{"a+b.go", "aab.go", false},
	}
	for _, c := range cases {
		re, err := compileGlob(c.pattern)
		if err != nil {
			t.Fatalf("%q: %v", c.pattern, err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("glob %q on %q = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestGlobErrors(t *testing.T) {
	for _, p := range []string{"", "  ", "[abc", "[z-a]"} {
		if _, err := compileGlob(p); err == nil {
			t.Errorf("pattern %q: expected error", p)
		}
	}
}

func TestParse(t *testing.T) {
	t.Run("empty means defaults", func(t *testing.T) {
		for _, in := range []string{"", "# only a comment\n"} {
			c, err := Parse([]byte(in))
			if err != nil {
				t.Fatal(err)
			}
			if !c.Exclude.Match("a/node_modules/x.js") || c.MaxFileSize != DefaultMaxFileSize {
				t.Errorf("defaults not applied: %+v", c)
			}
		}
	})
	t.Run("user lists extend defaults", func(t *testing.T) {
		c, err := Parse([]byte("version: 1\nexclude: ['**/gen/**']\ntests:\n  file_patterns: ['*.check.sh']\nmax_file_size: 10\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !c.Exclude.Match("x/gen/a.go") || !c.Exclude.Match("vendor/a.go") {
			t.Error("exclude should hold user + default patterns")
		}
		if !c.TestFilePatterns.Match("a.check.sh") || !c.TestFilePatterns.Match("a_test.go") {
			t.Error("file_patterns should hold user + default patterns")
		}
		if c.MaxFileSize != 10 {
			t.Errorf("MaxFileSize = %d", c.MaxFileSize)
		}
	})
	errCases := []struct{ name, in, want string }{
		{"missing version", "exclude: ['x']\n", `field "version"`},
		{"wrong version", "version: 2\n", `field "version"`},
		{"unknown field", "version: 1\nexcludes: ['x']\n", "excludes"},
		{"bad glob", "version: 1\nexclude: ['ok', '[bad']\n", "exclude[1]"},
		{"bad test glob", "version: 1\ntests:\n  directories: ['[bad']\n", "tests.directories[0]"},
		{"bad max size", "version: 1\nmax_file_size: 0\n", "max_file_size"},
		{"malformed yaml", "version: 1\nexclude: [\n", "invalid config"},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.in))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}

func TestDefaultTestPatterns(t *testing.T) {
	c := Default()
	for _, p := range []string{
		"a_test.go", "web/a.test.tsx", "web/a.spec.js", "test_a.py", "a_test.py", "a_spec.rb",
		"src/FooTest.java", "src/TestFoo.java", "src/FooTests.java", "A.kt", // last one must fail below
	} {
		isTest := c.TestFilePatterns.Match(p) || c.TestDirectories.Match(p)
		if p == "A.kt" {
			if isTest {
				t.Errorf("%s must not be a test", p)
			}
			continue
		}
		if !isTest {
			t.Errorf("%s should be a test", p)
		}
	}
	for _, p := range []string{"tests/a.go", "pkg/spec/a.go", "__tests__/a.js", "x/test/y/z.rs"} {
		if !c.TestDirectories.Match(p) {
			t.Errorf("%s should be in a test directory", p)
		}
	}
}

// V-08: the configuration cannot be used to exhaust the runner.
func TestConfigLimits(t *testing.T) {
	many := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString("  - 'a" + strings.Repeat("x", i%5) + "/**'\n")
		}
		return b.String()
	}
	cases := []struct{ name, in, want string }{
		{"file too big", "version: 1\n# " + strings.Repeat("x", MaxBytes) + "\n", "limit is"},
		{"too many patterns", "version: 1\nexclude:\n" + many(MaxPatterns+1), "exclude has"},
		{"pattern too long", "version: 1\nexclude:\n  - '" + strings.Repeat("a", MaxPatternLen+1) + "'\n", "exclude[0]"},
		{"max_file_size too big", "version: 1\nmax_file_size: 17000000\n", "max_file_size"},
		{"max_file_size negative", "version: 1\nmax_file_size: -1\n", "max_file_size"},
		{"max_file_size absurd", "version: 1\nmax_file_size: 9223372036854775807\n", "max_file_size"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.in)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
	// exactly at the limits is fine
	ok := "version: 1\nmax_file_size: 16777216\nexclude:\n" + many(MaxPatterns)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Errorf("config at the limits rejected: %v", err)
	}
}

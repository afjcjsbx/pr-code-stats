package analyzer

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseRaw(t *testing.T) {
	const z = "0000000000000000000000000000000000000000"
	raw := strings.Join([]string{
		":100644 100644 aaa bbb M", "dir with space/ünï.go",
		":000000 100644 " + z + " ccc A", "new\nline.go",
		":100644 000000 ddd " + z + " D", "gone.go",
		":100644 100755 eee eee R100", "old name.sh", "new name.sh",
		":100644 120000 fff ggg T", "link",
		"",
	}, "\x00")
	got, err := parseRaw([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		status           byte
		oldPath, newPath string
	}
	var rows []row
	for _, c := range got {
		rows = append(rows, row{c.Status, c.OldPath, c.NewPath})
	}
	want := []row{
		{'M', "dir with space/ünï.go", "dir with space/ünï.go"},
		{'A', "", "new\nline.go"},
		{'D', "gone.go", ""},
		{'R', "old name.sh", "new name.sh"},
		{'T', "link", "link"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("got %v\nwant %v", rows, want)
	}
	if got[3].OldMode != "100644" || got[3].NewMode != "100755" || got[1].NewBlob != "ccc" {
		t.Errorf("modes/blobs not parsed: %+v", got)
	}
	if _, err := parseRaw([]byte(":100644 100644 a b M\x00")); err == nil {
		t.Error("truncated record accepted")
	}
	if _, err := parseRaw([]byte("garbage\x00x\x00")); err == nil {
		t.Error("garbage accepted")
	}
}

func TestParseHunkHeader(t *testing.T) {
	cases := map[string]hunk{
		"@@ -1 +1 @@":                      {1, 1, 1, 1},
		"@@ -3,2 +3,4 @@ func foo() {":     {3, 2, 3, 4},
		"@@ -0,0 +1,5 @@":                  {0, 0, 1, 5},
		"@@ -7,3 +6,0 @@":                  {7, 3, 6, 0},
		"@@ -12 +12,2 @@ type X struct {}": {12, 1, 12, 2},
	}
	for in, want := range cases {
		got, err := parseHunkHeader(in)
		if err != nil || got != want {
			t.Errorf("%q = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"@@", "@@ -a +1 @@", "@@ -1,2 1,2 @@", "@@ -1 +1", "@@ --1 +1 @@"} {
		if _, err := parseHunkHeader(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParsePatch(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/a.py b/a.py",
		"index 111..222 100644",
		"--- a/a.py",
		"+++ b/a.py",
		"@@ -2 +2,2 @@",
		"--- removed line that looks like a header", // body: starts with "--- "
		"+++ added line that looks like a header",
		"+second added",
		`\ No newline at end of file`,
		"@@ -9,0 +11 @@ def f():",
		"+another",
		"diff --git a/img.png b/img.png",
		"index 333..444 100644",
		"Binary files a/img.png and b/img.png differ",
		"diff --git a/old.sh b/new.sh",
		"similarity index 100%",
		"rename from old.sh",
		"rename to new.sh",
		"diff --git a/link b/link", // type change: two sections for one raw entry
		"deleted file mode 100644",
		"--- a/link",
		"+++ /dev/null",
		"@@ -1,2 +0,0 @@",
		"-x",
		"-y",
		"diff --git a/link b/link",
		"new file mode 120000",
		"--- /dev/null",
		"+++ b/link",
		"@@ -0,0 +1 @@",
		"+target",
		`\ No newline at end of file`,
		"",
	}, "\n")
	changes := []fileChange{{Status: 'M'}, {Status: 'M'}, {Status: 'R'}, {Status: 'T'}}
	if err := parsePatch(strings.NewReader(patch), changes); err != nil {
		t.Fatal(err)
	}
	if want := []hunk{{2, 1, 2, 2}, {9, 0, 11, 1}}; !reflect.DeepEqual(changes[0].Hunks, want) {
		t.Errorf("hunks = %+v, want %+v", changes[0].Hunks, want)
	}
	if !changes[1].Binary || len(changes[1].Hunks) != 0 {
		t.Errorf("binary entry: %+v", changes[1])
	}
	if changes[2].Binary || len(changes[2].Hunks) != 0 {
		t.Errorf("pure rename: %+v", changes[2])
	}
	if want := []hunk{{1, 2, 0, 0}, {0, 0, 1, 1}}; !reflect.DeepEqual(changes[3].Hunks, want) {
		t.Errorf("type change hunks = %+v, want %+v", changes[3].Hunks, want)
	}
}

func TestParsePatchHandlesHugeLines(t *testing.T) {
	big := strings.Repeat("x", 300_000)
	patch := "diff --git a/m.js b/m.js\n@@ -1 +1 @@\n-" + big + "\n+" + big + "y\ndiff --git a/n b/n\n@@ -0,0 +1 @@\n+z\n"
	changes := []fileChange{{Status: 'M'}, {Status: 'M'}}
	if err := parsePatch(strings.NewReader(patch), changes); err != nil {
		t.Fatal(err)
	}
	if len(changes[0].Hunks) != 1 || len(changes[1].Hunks) != 1 {
		t.Errorf("hunks: %+v", changes)
	}
}

func TestParsePatchRejectsInconsistentInput(t *testing.T) {
	cases := map[string]struct {
		patch   string
		changes []fileChange
	}{
		"body shorter than header": {"diff --git a/a b/a\n@@ -1,2 +1 @@\n-x\ndiff --git a/b b/b\n", []fileChange{{}, {}}},
		"truncated inside hunk":    {"diff --git a/a b/a\n@@ -1,3 +1,0 @@\n-x\n", []fileChange{{}}},
		"more files than raw":      {"diff --git a/a b/a\ndiff --git a/b b/b\n", []fileChange{{}}},
		"fewer files than raw":     {"diff --git a/a b/a\n", []fileChange{{}, {}}},
		"text before first file":   {"@@ -1 +1 @@\n-a\n+b\n", []fileChange{{}}},
	}
	for name, c := range cases {
		if err := parsePatch(strings.NewReader(c.patch), c.changes); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

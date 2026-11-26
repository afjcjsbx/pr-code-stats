package report

import (
	"fmt"
	"strings"
	"testing"

	"github.com/afjcjsbx/pr-code-stats/internal/analyzer"
)

func TestMarkdown(t *testing.T) {
	res := &analyzer.Result{
		FilesChanged: 12,
		Stats: analyzer.Stats{
			Code:     analyzer.Counter{Added: 280, Removed: 80},
			Tests:    analyzer.Counter{Added: 110, Removed: 36},
			Comments: analyzer.Counter{Added: 38, Removed: 10},
			Other:    analyzer.Counter{Added: 20, Removed: 3},
		},
	}
	got := Markdown(Data{PRNumber: 42, Result: res, ProjectURL: "https://example.test/x"})

	if !strings.HasPrefix(got, Marker+"\n") {
		t.Error("report must start with the marker")
	}
	for _, want := range []string{
		"Pull Request #42 · Code analysis",
		"| Files changed | 12 |",
		"| Lines added | +448 |",
		"| Lines removed | -129 |",
		"| Net change | +319 |",
		"| 💻 Production code | 280 | 80 |",
		"| 🧪 Tests | 110 | 36 |",
		"| 💬 Comments | 38 | 10 |",
		"| 📄 Other | 20 | 3 |",
		"| **Total** | **448** | **129** |",
		"[PR Code Stats](https://example.test/x)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "could not be classified") {
		t.Error("degradation note shown without degraded files")
	}
}

func TestMarkdownVariants(t *testing.T) {
	res := &analyzer.Result{Degraded: 2, Stats: analyzer.Stats{Code: analyzer.Counter{Added: 1, Removed: 5}}}
	got := Markdown(Data{Result: res})
	if strings.Contains(got, "Pull Request #") {
		t.Error("PR line must be omitted when the number is unknown")
	}
	if !strings.Contains(got, "| Net change | -4 |") {
		t.Errorf("negative net not rendered:\n%s", got)
	}
	if !strings.Contains(got, "2 file(s) could not be classified") {
		t.Error("degradation note missing")
	}
	if !strings.Contains(got, DefaultProjectURL) {
		t.Error("default project URL missing")
	}
	zero := Markdown(Data{Result: &analyzer.Result{}})
	if !strings.Contains(zero, "| Net change | +0 |") {
		t.Errorf("zero net:\n%s", zero)
	}
}

// The numbers in the breakdown rows must always add up to the Total row.
func TestBreakdownSumsToTotal(t *testing.T) {
	for i := 0; i < 20; i++ {
		s := analyzer.Stats{
			Code: analyzer.Counter{Added: i * 3, Removed: i}, Tests: analyzer.Counter{Added: i, Removed: i * 7},
			Comments: analyzer.Counter{Added: 5 * i, Removed: 2}, Other: analyzer.Counter{Added: 1, Removed: i * i},
		}
		tot := s.Total()
		got := Markdown(Data{Result: &analyzer.Result{Stats: s}})
		if want := fmt.Sprintf("| **Total** | **%d** | **%d** |", tot.Added, tot.Removed); !strings.Contains(got, want) {
			t.Fatalf("missing %q", want)
		}
		if tot.Added != s.Code.Added+s.Tests.Added+s.Comments.Added+s.Other.Added {
			t.Fatal("Total().Added is not the sum of categories")
		}
	}
}

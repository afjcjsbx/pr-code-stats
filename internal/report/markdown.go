// Package report renders the analysis as Markdown.
package report

import (
	"fmt"
	"strings"

	"github.com/afjcjsbx/pr-code-stats/internal/analyzer"
)

// Marker starts every comment this action posts; it is how an earlier report is found again.
const Marker = "<!-- pr-code-stats:report:v1 -->"

// DefaultProjectURL is linked from the footer when the runner does not tell us where the action lives.
const DefaultProjectURL = "https://github.com/afjcjsbx/pr-code-stats"

// Data is everything the report shows; all numbers come from here, none are hard-coded.
type Data struct {
	PRNumber   int // 0: unknown, the line is omitted
	Result     *analyzer.Result
	ProjectURL string
}

// Markdown renders the report, starting with Marker.
func Markdown(d Data) string {
	s, tot := d.Result.Stats, d.Result.Stats.Total()
	url := d.ProjectURL
	if url == "" {
		url = DefaultProjectURL
	}

	var b strings.Builder
	b.WriteString(Marker + "\n## 📊 PR Code Statistics\n\n**Analysis completed successfully.**\n\n")
	if d.PRNumber > 0 {
		fmt.Fprintf(&b, "Pull Request #%d · Code analysis\n\n", d.PRNumber)
	}
	fmt.Fprintf(&b, "| Metric | Value |\n|---|---:|\n| Files changed | %d |\n| Lines added | +%d |\n| Lines removed | -%d |\n| Net change | %+d |\n\n",
		d.Result.FilesChanged, tot.Added, tot.Removed, s.Net())

	b.WriteString("### Breakdown\n\n| Category | Added | Removed |\n|---|---:|---:|\n")
	for _, row := range []struct {
		label string
		c     analyzer.Counter
	}{
		{"💻 Production code", s.Code},
		{"🧪 Tests", s.Tests},
		{"💬 Comments", s.Comments},
		{"📄 Other", s.Other},
	} {
		fmt.Fprintf(&b, "| %s | %d | %d |\n", row.label, row.c.Added, row.c.Removed)
	}
	fmt.Fprintf(&b, "| **Total** | **%d** | **%d** |\n", tot.Added, tot.Removed)

	if n := d.Result.Degraded; n > 0 {
		fmt.Fprintf(&b, "\n> ⚠️ %d file(s) could not be classified (too large or unparsable); their lines are counted as Other. Totals are exact.\n", n)
	}
	fmt.Fprintf(&b, "\n---\n\n*Generated automatically by [PR Code Stats](%s)*\n", url)
	return b.String()
}

package analyzer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// fileChange is one entry of `git diff`, with the exact line ranges that changed.
type fileChange struct {
	Status           byte // A, D, M, R, T
	OldMode, NewMode string
	OldBlob, NewBlob string // full blob SHAs; all zeros when that side does not exist
	OldPath, NewPath string // empty when that side does not exist
	Binary           bool
	Hunks            []hunk
}

// hunk is a -U0 hunk: lines [OldStart, OldStart+OldLines) were removed from the old file and
// [NewStart, NewStart+NewLines) were added to the new one. Without context there is nothing else.
type hunk struct{ OldStart, OldLines, NewStart, NewLines int }

func (c *fileChange) gitlink() bool { return c.OldMode == "160000" || c.NewMode == "160000" }

// Hard limits: a diff beyond them is refused with an error rather than reported partially.
var (
	maxChanges          = 100_000 // changed files
	maxPatchBytes int64 = 1 << 30 // bytes of `git diff -p` output read
)

// diffArgs pins everything that could change the output: no external drivers or colours,
// rename detection on, the default Myers algorithm whatever diff.algorithm says, and
// .gitattributes taken from attrSource (the trusted base commit) instead of the working tree,
// which holds the pull request's own copy: a PR could otherwise mark its files "-diff" (binary)
// and make its lines vanish from the statistics.
func diffArgs(attrSource string, extra ...string) []string {
	args := []string{"--attr-source=" + attrSource, "diff", "-M", "--diff-algorithm=myers", "--no-abbrev", "--no-ext-diff", "--no-textconv", "--no-color"}
	return append(args, extra...)
}

// diff lists the changes from base to head with their hunks. It runs git twice (`--raw -z` for
// unambiguous paths and blob IDs, `-p -U0` for hunks) and joins the results by position.
func (r Repo) diff(ctx context.Context, attrSource, base, head string) ([]fileChange, error) {
	raw, err := r.run(ctx, diffArgs(attrSource, "--raw", "-z", base, head, "--")...)
	if err != nil {
		return nil, err
	}
	changes, err := parseRaw(raw)
	if err != nil {
		return nil, err
	}
	if len(changes) > maxChanges {
		return nil, fmt.Errorf("the diff touches %d files, more than the %d this action analyses", len(changes), maxChanges)
	}

	c := r.cmd(ctx, diffArgs(attrSource, "-p", "-U0", base, head, "--")...)
	stderr := &limitBuffer{max: 1 << 20}
	c.Stderr = stderr
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, gitError("diff", err, "")
	}
	perr := parsePatch(&limitedReader{r: stdout, left: maxPatchBytes}, changes)
	if perr != nil {
		_ = c.Process.Kill() // do not wait for the rest of a patch we are going to discard
	}
	werr := c.Wait()
	if perr != nil {
		return nil, perr
	}
	if werr != nil {
		return nil, gitError("diff", werr, stderr.String())
	}
	return changes, nil
}

// limitedReader fails (instead of silently truncating) once more than left bytes were read.
type limitedReader struct {
	r    io.Reader
	left int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return n, fmt.Errorf("the patch is larger than %d bytes, more than this action analyses", maxPatchBytes)
	}
	return n, err
}

// parseRaw parses `git diff --raw -z --no-abbrev`:
// ":oldmode newmode oldsha newsha STATUS\0path\0" (renames carry two paths).
func parseRaw(raw []byte) ([]fileChange, error) {
	var out []fileChange
	fields := strings.Split(string(raw), "\x00")
	for i := 0; i < len(fields); {
		meta := fields[i]
		if meta == "" && i == len(fields)-1 {
			break // trailing NUL
		}
		i++
		m := strings.Fields(meta)
		if len(m) != 5 || !strings.HasPrefix(m[0], ":") {
			return nil, fmt.Errorf("unexpected git diff --raw record %q", meta)
		}
		ch := fileChange{
			Status: m[4][0], OldMode: m[0][1:], NewMode: m[1], OldBlob: m[2], NewBlob: m[3],
		}
		npaths := 1
		if ch.Status == 'R' || ch.Status == 'C' {
			npaths = 2
		}
		if i+npaths > len(fields) || fields[i] == "" || fields[i+npaths-1] == "" {
			return nil, fmt.Errorf("truncated git diff --raw record %q", meta)
		}
		switch {
		case npaths == 2:
			ch.OldPath, ch.NewPath = fields[i], fields[i+1]
		case ch.Status == 'A':
			ch.NewPath = fields[i]
		case ch.Status == 'D':
			ch.OldPath = fields[i]
		default:
			ch.OldPath, ch.NewPath = fields[i], fields[i]
		}
		i += npaths
		out = append(out, ch)
	}
	return out, nil
}

// parsePatch reads a `-p -U0` stream and attaches hunks to changes (same order as the raw list;
// a type change is printed as two sections, delete + create). Parsing is a state machine:
// once a hunk header announces N old and M new lines, exactly that many body lines are consumed
// as data, so a line like "+++ x" or "--- x" inside a hunk is never mistaken for a file header.
func parsePatch(r io.Reader, changes []fileChange) error {
	var sectionOwner []int // section number -> index into changes
	for i, ch := range changes {
		sectionOwner = append(sectionOwner, i)
		if ch.Status == 'T' {
			sectionOwner = append(sectionOwner, i)
		}
	}

	br := bufio.NewReaderSize(r, 64<<10)
	var (
		section    = -1
		cur        *fileChange
		remOld     int
		remNew     int
		cont       bool // previous read was a prefix of a longer line
		lineNumber int
	)
	for {
		frag, isPrefix, err := br.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		wasCont := cont
		cont = isPrefix
		if wasCont {
			continue // tail of a very long line we already classified by its head
		}
		lineNumber++

		if remOld > 0 || remNew > 0 { // inside a hunk body: only the first byte matters
			var first byte
			if len(frag) > 0 {
				first = frag[0]
			}
			switch {
			case first == '-' && remOld > 0:
				remOld--
			case first == '+' && remNew > 0:
				remNew--
			case first == '\\': // "\ No newline at end of file"
			default:
				return fmt.Errorf("git diff line %d: hunk body ended early", lineNumber)
			}
			continue
		}
		line := string(frag)
		switch {
		case strings.HasPrefix(line, `\`):
		case strings.HasPrefix(line, "diff --git "):
			section++
			if section >= len(sectionOwner) {
				return errors.New("git diff patch has more files than the raw listing")
			}
			cur = &changes[sectionOwner[section]]
		case cur == nil:
			return fmt.Errorf("git diff line %d: unexpected text before first file header", lineNumber)
		case strings.HasPrefix(line, "@@ "):
			h, err := parseHunkHeader(line)
			if err != nil {
				return fmt.Errorf("git diff line %d: %w", lineNumber, err)
			}
			cur.Hunks = append(cur.Hunks, h)
			remOld, remNew = h.OldLines, h.NewLines
		case strings.HasPrefix(line, "Binary files "), strings.HasPrefix(line, "GIT binary patch"):
			cur.Binary = true
		}
		// anything else is a header line (index, mode, rename, ---, +++): nothing to learn
	}
	if remOld > 0 || remNew > 0 {
		return errors.New("git diff output ended inside a hunk")
	}
	if section+1 != len(sectionOwner) {
		return fmt.Errorf("git diff patch has %d files, raw listing has %d", section+1, len(sectionOwner))
	}
	return nil
}

// parseHunkHeader parses "@@ -a[,b] +c[,d] @@ ...".
func parseHunkHeader(line string) (hunk, error) {
	f := strings.Fields(line)
	if len(f) < 4 || f[0] != "@@" || f[3] != "@@" {
		return hunk{}, fmt.Errorf("malformed hunk header %q", line)
	}
	var h hunk
	var err error
	if h.OldStart, h.OldLines, err = parseRange(f[1], '-'); err != nil {
		return hunk{}, fmt.Errorf("malformed hunk header %q: %w", line, err)
	}
	if h.NewStart, h.NewLines, err = parseRange(f[2], '+'); err != nil {
		return hunk{}, fmt.Errorf("malformed hunk header %q: %w", line, err)
	}
	return h, nil
}

func parseRange(s string, sign byte) (start, n int, err error) {
	if s == "" || s[0] != sign {
		return 0, 0, fmt.Errorf("bad range %q", s)
	}
	a, b, hasCount := strings.Cut(s[1:], ",")
	if start, err = strconv.Atoi(a); err != nil {
		return 0, 0, err
	}
	n = 1
	if hasCount {
		if n, err = strconv.Atoi(b); err != nil {
			return 0, 0, err
		}
	}
	if start < 0 || n < 0 {
		return 0, 0, fmt.Errorf("negative range %q", s)
	}
	return start, n, nil
}

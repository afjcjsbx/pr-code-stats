package analyzer

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// blobStore reads blobs through a single long-lived `git cat-file --batch` process.
// Blobs are addressed by SHA (taken from the diff), so paths with spaces, newlines or
// non-UTF-8 bytes never touch the git protocol.
type blobStore struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *bytes.Buffer
}

func (r Repo) openBlobs(ctx context.Context) (*blobStore, error) {
	c := r.cmd(ctx, "cat-file", "--batch")
	in, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &bytes.Buffer{}
	c.Stderr = stderr
	if err := c.Start(); err != nil {
		return nil, gitError("cat-file", err, "")
	}
	return &blobStore{cmd: c, in: in, out: bufio.NewReaderSize(out, 64<<10), stderr: stderr}, nil
}

// read returns the blob content, or tooBig=true (content skipped) if it exceeds max bytes.
func (b *blobStore) read(sha string, max int64) (data []byte, tooBig bool, err error) {
	if _, err := io.WriteString(b.in, sha+"\n"); err != nil {
		return nil, false, gitError("cat-file", err, b.stderr.String())
	}
	header, err := b.out.ReadString('\n')
	if err != nil {
		return nil, false, gitError("cat-file", err, b.stderr.String())
	}
	f := strings.Fields(header)
	if len(f) == 2 && f[1] == "missing" {
		return nil, false, fmt.Errorf("blob %s is missing from the local clone (partial or shallow clone?)", sha)
	}
	if len(f) != 3 || f[1] != "blob" {
		return nil, false, fmt.Errorf("unexpected cat-file reply %q", strings.TrimSpace(header))
	}
	size, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return nil, false, fmt.Errorf("unexpected cat-file size in %q", strings.TrimSpace(header))
	}
	if size > max {
		_, err = io.CopyN(io.Discard, b.out, size+1) // content + trailing LF
		return nil, true, err
	}
	data = make([]byte, size+1)
	if _, err := io.ReadFull(b.out, data); err != nil {
		return nil, false, gitError("cat-file", err, b.stderr.String())
	}
	return data[:size], false, nil
}

func (b *blobStore) close() error {
	_ = b.in.Close()
	return b.cmd.Wait()
}

// fileCache memoises line classification per (blob SHA, path): the lexer is chosen by file
// name, so the same blob under two names is classified twice but never read twice. Kinds cost
// one byte per line, so the cache is dropped wholesale past retainLimit instead of growing
// with the number of files in the pull request.
type fileCache struct {
	blobs    *blobStore
	pool     *lexPool
	max      int64
	entries  map[cacheKey]cacheEntry
	retained int
}

var retainLimit = 32 << 20

type cacheKey struct{ blob, path string }

type cacheEntry struct {
	kinds  []lineKind
	reason string // non-empty: the file could not be classified and must count as Other
}

func newFileCache(blobs *blobStore, pool *lexPool, maxSize int64) *fileCache {
	return &fileCache{blobs: blobs, pool: pool, max: maxSize, entries: map[cacheKey]cacheEntry{}}
}

// kinds returns per-line kinds for a blob. reason != "" means "degrade this file to Other"
// (too big, lexer too slow or crashed). Errors are only real failures: git cannot read the blob.
func (fc *fileCache) kinds(blob, p string) (kinds []lineKind, reason string, err error) {
	key := cacheKey{blob, p}
	if e, ok := fc.entries[key]; ok {
		return e.kinds, e.reason, nil
	}
	data, tooBig, err := fc.blobs.read(blob, fc.max)
	if err != nil {
		return nil, "", err
	}
	var e cacheEntry
	if tooBig {
		e.reason = fmt.Sprintf("larger than max_file_size (%d bytes)", fc.max)
	} else if e.kinds, err = fc.pool.classify(path.Base(p), data); err != nil {
		e.kinds, e.reason = nil, err.Error()
	}
	if fc.retained+len(e.kinds) > retainLimit {
		fc.entries, fc.retained = map[cacheKey]cacheEntry{}, 0
	}
	fc.entries[key] = e
	fc.retained += len(e.kinds)
	return e.kinds, e.reason, nil
}

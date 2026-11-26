package analyzer

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

// Chroma's lexers can take time quadratic in the number of tokens on hostile input (a Java file
// of 20,000 newlines needs ~50 s, one of 1 MiB would need hours) and cannot be interrupted.
// So files are lexed in a worker process, a copy of this binary talking over pipes, that is
// killed when a file takes too long; such a file counts as Other and the worker is restarted.
// A time budget bounds the total, and the worker never sees the step's environment (token).

const workerEnv = "PR_CODE_STATS_LEX_WORKER"

// Vars so tests can shorten them.
var (
	lexTimeout = 10 * time.Second // per file
	lexBudget  = 4 * time.Minute  // all files of one analysis
)

const (
	maxWorkerName    = 4096
	maxWorkerContent = 64 << 20
)

var (
	errLexTimeout = errors.New("lexing took longer than the per-file time limit")
	errLexBudget  = errors.New("the lexing time budget of this analysis is exhausted")
)

// RunWorkerIfRequested turns the process into a lexer worker when it was started as one by
// lexPool. main and test binaries call it first thing; it reports whether it served.
func RunWorkerIfRequested() bool {
	if os.Getenv(workerEnv) != "1" {
		return false
	}
	if err := serveWorker(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "lexer worker:", err)
		os.Exit(1)
	}
	return true
}

// --- worker side ---------------------------------------------------------------------------

func serveWorker(r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 1<<20)
	bw := bufio.NewWriterSize(w, 1<<20)
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		nameLen, contentLen := binary.BigEndian.Uint32(hdr[:4]), binary.BigEndian.Uint32(hdr[4:])
		if nameLen > maxWorkerName || contentLen > maxWorkerContent {
			return fmt.Errorf("request too large (%d, %d)", nameLen, contentLen)
		}
		buf := make([]byte, int(nameLen)+int(contentLen))
		if _, err := io.ReadFull(br, buf); err != nil {
			return err
		}
		kinds, err := workerClassify(string(buf[:nameLen]), buf[nameLen:])
		var status byte
		payload := make([]byte, len(kinds))
		for i, k := range kinds {
			payload[i] = byte(k)
		}
		if err != nil {
			status, payload = 1, []byte(err.Error())
		}
		var out [5]byte
		out[0] = status
		binary.BigEndian.PutUint32(out[1:], uint32(len(payload))) // #nosec G115 -- a file of at most maxWorkerContent bytes
		if _, err := bw.Write(out[:]); err != nil {
			return err
		}
		if _, err := bw.Write(payload); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
	}
}

func workerClassify(base string, content []byte) ([]lineKind, error) {
	lexer := sourceLexer(base)
	if lexer == nil {
		return nil, errors.New("no source lexer for this file name")
	}
	return classifyLines(lexer, content)
}

// --- parent side ---------------------------------------------------------------------------

type lexPool struct {
	cmd       *exec.Cmd
	in        io.WriteCloser
	out       *bufio.Reader
	remaining time.Duration
}

func newLexPool() *lexPool { return &lexPool{remaining: lexBudget} }

func (p *lexPool) start() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate the lexer worker: %w", err)
	}
	c := exec.Command(exe) // #nosec G204 -- our own executable, no arguments, no step environment
	c.Env = []string{workerEnv + "=1"}
	in, err := c.StdinPipe()
	if err != nil {
		return err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	c.Stderr = &limitBuffer{max: 1 << 16}
	if err := c.Start(); err != nil {
		return fmt.Errorf("cannot start the lexer worker: %w", err)
	}
	p.cmd, p.in, p.out = c, in, bufio.NewReaderSize(out, 1<<20)
	return nil
}

// reap kills the worker (if any); the next call starts a fresh one.
func (p *lexPool) reap() {
	if p.cmd == nil {
		return
	}
	_ = p.in.Close()
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	p.cmd, p.in, p.out = nil, nil, nil
}

func (p *lexPool) close() { p.reap() }

// classify lexes content with the lexer chosen for the file name base. Any error means "treat
// this file as Other"; it never aborts the analysis.
func (p *lexPool) classify(base string, content []byte) ([]lineKind, error) {
	if p.remaining <= 0 {
		return nil, errLexBudget
	}
	if p.cmd == nil {
		if err := p.start(); err != nil {
			return nil, err
		}
	}
	limit := lexTimeout
	budgetBound := p.remaining < limit
	if budgetBound {
		limit = p.remaining
	}
	var timedOut atomic.Bool
	proc := p.cmd.Process
	timer := time.AfterFunc(limit, func() { timedOut.Store(true); _ = proc.Kill() })
	began := time.Now()
	kinds, lexErr, transportErr := p.roundTrip(base, content)
	timer.Stop()
	p.remaining -= time.Since(began)

	if transportErr != nil {
		p.reap()
		switch {
		case timedOut.Load() && budgetBound:
			return nil, errLexBudget
		case timedOut.Load():
			return nil, errLexTimeout
		}
		return nil, fmt.Errorf("lexer worker failed: %w", transportErr)
	}
	if timedOut.Load() {
		p.reap() // the answer arrived but the worker was killed under us
	}
	if lexErr != nil {
		return nil, lexErr
	}
	if len(kinds) != countLines(content) {
		return nil, fmt.Errorf("lexer worker returned %d lines for a file of %d", len(kinds), countLines(content))
	}
	return kinds, nil
}

func (p *lexPool) roundTrip(base string, content []byte) (kinds []lineKind, lexErr, transportErr error) {
	if len(base) > maxWorkerName || len(content) > maxWorkerContent {
		return nil, fmt.Errorf("file too large for the lexer worker (%d bytes)", len(content)), nil
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(base)))    // #nosec G115 -- bounded just above
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(content))) // #nosec G115 -- bounded just above
	for _, chunk := range [][]byte{hdr[:], []byte(base), content} {
		if _, err := p.in.Write(chunk); err != nil {
			return nil, nil, err
		}
	}
	var resp [5]byte
	if _, err := io.ReadFull(p.out, resp[:]); err != nil {
		return nil, nil, err
	}
	n := binary.BigEndian.Uint32(resp[1:])
	if n > maxWorkerContent {
		return nil, nil, fmt.Errorf("worker answer too large (%d)", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(p.out, payload); err != nil {
		return nil, nil, err
	}
	if resp[0] != 0 {
		return nil, fmt.Errorf("lexer failed: %s", payload), nil
	}
	kinds = make([]lineKind, n)
	for i, b := range payload {
		kinds[i] = lineKind(b)
	}
	return kinds, nil, nil
}

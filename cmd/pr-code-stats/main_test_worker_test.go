package main

import (
	"os"
	"testing"

	"github.com/afjcjsbx/pr-code-stats/internal/analyzer"
)

// The lexer worker is a copy of the running binary; here that binary is the test binary.
func TestMain(m *testing.M) {
	if analyzer.RunWorkerIfRequested() {
		return
	}
	os.Exit(m.Run())
}

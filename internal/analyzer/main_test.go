package analyzer

import (
	"os"
	"testing"
)

// The lexer worker is a copy of the running binary; here that binary is the test binary.
func TestMain(m *testing.M) {
	if RunWorkerIfRequested() {
		return
	}
	os.Exit(m.Run())
}

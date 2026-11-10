package web

import (
	"io"
	"os"
	"testing"
)

// The default logger writes to stderr; keep test output readable. Tests that
// assert on logs pass their own logger with WithLogger.
func TestMain(m *testing.M) {
	logOutput = io.Discard
	os.Exit(m.Run())
}

package app

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
)

// closeQuietly closes c and fails the test on any error other than an
// already-closed resource, so a deferred close can follow an explicit one.
func closeQuietly(t *testing.T, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close: %v", err)
	}
}

// setMode changes a mode the test needs; directories require the execute bit,
// and several tests deliberately widen a mode to exercise the safety checks.
func setMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil { //nolint:gosec // G302: directory modes need the execute bit; widened modes are the fixture
		t.Fatal(err)
	}
}

// readTestFile reads a fixture path created under the test's temporary directory.
func readTestFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: test fixture path under a temporary directory
}

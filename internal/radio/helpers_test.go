package radio

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

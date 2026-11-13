package control

import (
	"arednlink-hf/internal/core"
	"arednlink-hf/internal/status"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type emptyPeers struct{}

func (emptyPeers) Snapshot() []core.Peer { return []core.Peer{} }
func (emptyPeers) PeerTable() core.PeerTableStats {
	return core.PeerTableStats{Policy: core.FullPolicyReplace, MaxPeers: 64}
}

type configStub struct{}

func (configStub) Snapshot(context.Context) (ConfigSnapshot, error) {
	return ConfigSnapshot{Running: "{}", Saved: "{}", Disabled: "disabled"}, nil
}
func (configStub) Disabled() string                       { return "disabled" }
func (configStub) Validate(context.Context, string) error { return nil }
func (configStub) Save(context.Context, string) error     { return nil }

func startServer(t *testing.T) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	dir := t.TempDir()
	setMode(t, dir, 0700)
	path := filepath.Join(dir, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, path, emptyPeers{}, status.NewRadioStatus("none"), configStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("shutdown timed out")
		}
	})
	for end := time.Now().Add(time.Second); time.Now().Before(end); {
		if _, err := os.Stat(path); err == nil {
			return path, cancel, done
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("server did not start")
	return "", cancel, done
}
func exchange(t *testing.T, path, frame string) response {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(t, conn)
	if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(conn, frame); err != nil {
		t.Fatal(err)
	}
	data, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var reply response
	if err = json.Unmarshal(data, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}
func TestFramesAndModes(t *testing.T) {
	path, _, _ := startServer(t)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("socket mode", info, err)
	}
	tests := []struct{ frame, code string }{
		{`{"op":"status"}` + "\n", ""},
		{`{"op":"config.get"}` + "\n", ""},
		{`{"op":"wat"}` + "\n", "unknown_operation"},
		{`{"op":"status","op":"status"}` + "\n", "invalid_request"},
		{`{"op":"status","OP":"status"}` + "\n", "invalid_request"},
		{`{"op":"status","config":"{}"}` + "\n", "invalid_request"},
		{`{"op":"status","extra":0}` + "\n", "invalid_request"},
		{`{"op":null}` + "\n", "invalid_request"},
		{`{"op":"config.save","config":null}` + "\n", "invalid_request"},
		{`{"op":"config.save","config":"{}"}` + "\n", "save_disabled"},
		{`{"op":"config.validate","config":"` + strings.Repeat("x", (64<<10)+1) + `"}` + "\n", "config_too_large"},
		{"{\"op\":\"\xff\"}\n", "invalid_request"},
		{"{} {}\n", "invalid_request"},
		{strings.Repeat("x", MaxFrame) + "\n", "frame_too_large"},
	}
	for _, test := range tests {
		if r := exchange(t, path, test.frame); r.Error != test.code || r.OK != (test.code == "") {
			t.Errorf("frame %.50q got %+v want %s", test.frame, r, test.code)
		}
	}
}
func TestUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	setMode(t, dir, 0700)
	path := filepath.Join(dir, "control.sock")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := listen(path); err == nil {
		t.Fatal("replaced regular file")
	}
	data, _ := readTestFile(path)
	if string(data) != "keep" {
		t.Fatal("changed existing file")
	}
	must(t, os.Remove(path))
	must(t, os.Symlink(filepath.Join(dir, "absent"), path))
	if _, err := listen(path); err == nil {
		t.Fatal("accepted socket symlink")
	}
	must(t, os.Remove(path))
	alias := filepath.Join(t.TempDir(), "alias")
	must(t, os.Symlink(dir, alias))
	if _, err := listen(filepath.Join(alias, "control.sock")); err == nil {
		t.Fatal("accepted directory symlink")
	}
	setMode(t, dir, 0755)
	if _, err := listen(path); err == nil {
		t.Fatal("accepted public directory")
	}
}
func TestConcurrencyAndCancellation(t *testing.T) {
	path, cancel, _ := startServer(t)
	var conns []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		defer closeQuietly(t, c)
	}
	// Two incomplete requests occupy both workers; the third is not processed.
	if _, err := io.WriteString(conns[2], "{\"op\":\"status\"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := conns[2].SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := conns[2].Read(make([]byte, 1)); err == nil {
		t.Fatal("third client processed while both workers occupied")
	}
	cancel()
	for end := time.Now().Add(time.Second); time.Now().Before(end); {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("socket retained on shutdown")
	}
	for _, c := range conns[:2] {
		if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("client retained on cancellation")
		}
	}
}
func TestMissingNewlineDeadline(t *testing.T) {
	path, _, _ := startServer(t)
	r := exchange(t, path, `{"op":"status"}`)
	if r.Error != "invalid_request" {
		t.Fatal(r)
	}
}

func TestStaleSocketRecoveryAndPersistentLock(t *testing.T) {
	dir := t.TempDir()
	setMode(t, dir, 0700)
	path := filepath.Join(dir, "control.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	setMode(t, path, 0600)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A second name keeps the stale inode allocated, so the replacement socket
	// cannot reuse its inode number and SameFile reliably detects replacement.
	if err := os.Link(path, filepath.Join(dir, "stale.ref")); err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	owned, err := listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(t, owned.lock)
	defer closeQuietly(t, owned.listener)
	after, err := os.Lstat(path)
	if err != nil || os.SameFile(before, after) {
		t.Fatal("stale socket was not replaced", err)
	}
	if _, err := listen(path); err == nil {
		t.Fatal("second instance acquired lock")
	}
	lockBefore, err := os.Lstat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	closeQuietly(t, owned.listener)
	closeQuietly(t, owned.lock)
	lockAfter, err := os.Lstat(path + ".lock")
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("lock inode changed", err)
	}
}

func TestUnsafeLockPaths(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "public", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			setMode(t, dir, 0700)
			path := filepath.Join(dir, "control.sock")
			lock := path + ".lock"
			switch kind {
			case "symlink":
				if err := os.Symlink(filepath.Join(dir, "absent"), lock); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(lock, 0700); err != nil {
					t.Fatal(err)
				}
			case "public":
				// Deliberately world-readable: the lock check must reject it.
				if err := os.WriteFile(lock, nil, 0644); err != nil { //nolint:gosec // G306: the unsafe mode is the fixture
					t.Fatal(err)
				}
			case "hardlink":
				original := filepath.Join(dir, "original")
				if err := os.WriteFile(original, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(original, lock); err != nil {
					t.Fatal(err)
				}
			}
			if owned, err := listen(path); err == nil {
				closeQuietly(t, owned.listener)
				closeQuietly(t, owned.lock)
				t.Fatal("accepted unsafe lock")
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("created socket despite unsafe lock", err)
			}
		})
	}
}

func TestActiveSocketIsPreserved(t *testing.T) {
	dir := t.TempDir()
	setMode(t, dir, 0700)
	path := filepath.Join(dir, "control.sock")
	active, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(t, active)
	setMode(t, path, 0600)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if owned, err := listen(path); err == nil {
		closeQuietly(t, owned.listener)
		closeQuietly(t, owned.lock)
		t.Fatal("accepted active socket")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("active socket changed", err)
	}
}

// flakyAcceptor fails its first accepts with a fixed error, then defers to the real listener.
type flakyAcceptor struct {
	*net.UnixListener
	failures atomic.Int32
	err      error
}

func (f *flakyAcceptor) AcceptUnix() (*net.UnixConn, error) {
	if f.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "unix", Err: f.err}
	}
	return f.UnixListener.AcceptUnix()
}

func newFlaky(t *testing.T, failures int32, err error) (string, *flakyAcceptor, *controlListener) {
	t.Helper()
	dir := t.TempDir()
	setMode(t, dir, 0700)
	path := filepath.Join(dir, "control.sock")
	owned, listenErr := listen(path)
	if listenErr != nil {
		t.Fatal(listenErr)
	}
	t.Cleanup(func() { closeQuietly(t, owned.listener); closeQuietly(t, owned.lock) })
	flaky := &flakyAcceptor{UnixListener: owned.listener, err: err}
	flaky.failures.Store(failures)
	return path, flaky, owned
}

func TestTransientAcceptErrorsBackOffAndRetry(t *testing.T) {
	for _, cause := range []error{syscall.EMFILE, syscall.ENFILE} {
		path, flaky, owned := newFlaky(t, 3, cause)
		ctx, cancel := context.WithCancel(t.Context())
		clients := &clientSet{conns: make(map[net.Conn]struct{})}
		done := make(chan error, 1)
		go func() {
			done <- acceptLoop(ctx, flaky, clients, emptyPeers{}, status.NewRadioStatus("none"), configStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		}()
		if reply := exchange(t, path, `{"op":"status"}`+"\n"); !reply.OK {
			t.Fatalf("%v: server did not recover: %+v", cause, reply)
		}
		cancel()
		closeQuietly(t, owned.listener)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%v: accept loop ended with %v", cause, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("accept loop did not stop")
		}
		clients.wg.Wait()
	}
}

func TestPermanentAcceptErrorEndsServer(t *testing.T) {
	_, flaky, _ := newFlaky(t, 1, syscall.EBADF)
	clients := &clientSet{conns: make(map[net.Conn]struct{})}
	err := acceptLoop(t.Context(), flaky, clients, emptyPeers{}, status.NewRadioStatus("none"), configStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, syscall.EBADF) {
		t.Fatalf("error = %v", err)
	}
}

func TestTransientAcceptClassification(t *testing.T) {
	for _, err := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ECONNABORTED, &net.OpError{Err: syscall.EMFILE}} {
		if !transientAccept(err) {
			t.Errorf("%v not transient", err)
		}
	}
	for _, err := range []error{net.ErrClosed, syscall.EBADF, errors.New("other")} {
		if transientAccept(err) {
			t.Errorf("%v classified transient", err)
		}
	}
}

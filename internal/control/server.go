package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"arednlink-hf/internal/core"
	"arednlink-hf/internal/jsoncheck"
	"arednlink-hf/internal/status"
)

const MaxFrame = 512 << 10
const timeout = 3 * time.Second

type PeerSource interface {
	Snapshot() []core.Peer
	PeerTable() core.PeerTableStats
}
type response struct {
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}
type StatusSnapshot struct {
	Health    status.RadioView    `json:"health"`
	Peers     []core.Peer         `json:"peers"`
	PeerTable core.PeerTableStats `json:"peer_table"`
}

// A persistent lock inode serializes startup and stale-socket removal across processes.
// It must never be unlinked: waiters may already hold an open descriptor.
type controlListener struct {
	listener *net.UnixListener
	lock     *os.File
}

func secureFile(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		return fmt.Errorf("unsafe control lock: %s", path)
	}
	return nil
}

// closeBestEffort closes a resource being abandoned after an earlier failure.
// Discarded: the Close error; the failure that caused the abandonment is the
// outcome the caller needs, and the descriptor is released either way.
func closeBestEffort(c io.Closer) {
	_ = c.Close()
}

// closeLogged closes a resource and records an unexpected failure at debug level.
func closeLogged(logger *slog.Logger, what string, c io.Closer) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		logger.Debug("close failed", "resource", what, "error", err)
	}
}

// checkControlDir validates the socket path and its private parent directory.
func checkControlDir(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("unsafe control path")
	}
	dir := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved != dir {
		return errors.New("unsafe control directory")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || st.Uid != uint32(os.Geteuid()) {
		return errors.New("unsafe control directory ownership or permissions")
	}
	return nil
}

// acquireLock opens and exclusively locks the persistent lock inode.
func acquireLock(path string) (*os.File, error) {
	lockPath := path + ".lock"
	// O_NOFOLLOW prevents following a malicious lock symlink, including a dangling one.
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	lockedInfo, err := lock.Stat()
	if err != nil {
		closeBestEffort(lock)
		return nil, err
	}
	if err := secureFile(lockPath, lockedInfo); err != nil {
		closeBestEffort(lock)
		return nil, err
	}
	pathInfo, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(lockedInfo, pathInfo) {
		closeBestEffort(lock)
		return nil, errors.New("control lock changed")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeBestEffort(lock)
		return nil, fmt.Errorf("control lock busy: %w", err)
	}
	// The pathname must still identify the locked inode after flock succeeds.
	pathInfo, err = os.Lstat(lockPath)
	if err != nil || !os.SameFile(lockedInfo, pathInfo) {
		closeBestEffort(lock)
		return nil, errors.New("control lock changed")
	}
	return lock, nil
}

// removeStaleSocket deletes an existing socket only if nothing answers on it.
func removeStaleSocket(path string) error {
	old, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	oldStat, ok := old.Sys().(*syscall.Stat_t)
	if !ok || old.Mode()&os.ModeSocket == 0 || old.Mode().Perm() != 0600 || oldStat.Uid != uint32(os.Geteuid()) {
		return errors.New("unsafe existing control socket")
	}
	conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
	if dialErr == nil {
		closeBestEffort(conn)
		return errors.New("control socket is active")
	}
	var opErr *net.OpError
	if !errors.As(dialErr, &opErr) || !errors.Is(opErr.Err, syscall.ECONNREFUSED) {
		return fmt.Errorf("control socket state uncertain: %w", dialErr)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(old, current) {
		return errors.New("control socket changed")
	}
	return os.Remove(path)
}

// bindSocket listens on path and restricts it to the owner.
func bindSocket(path string) (*net.UnixListener, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	socketInfo, err := os.Lstat(path)
	if err != nil {
		closeBestEffort(listener)
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		closeBestEffort(listener)
		if current, statErr := os.Lstat(path); statErr == nil && os.SameFile(socketInfo, current) {
			// Discarded: the Remove error; the chmod failure is returned and
			// a leftover socket is removed as stale on the next start.
			_ = os.Remove(path)
		}
		return nil, err
	}
	return listener, nil
}

func listen(path string) (*controlListener, error) {
	if err := checkControlDir(path); err != nil {
		return nil, err
	}
	lock, err := acquireLock(path)
	if err != nil {
		return nil, err
	}
	if err := removeStaleSocket(path); err != nil {
		closeBestEffort(lock)
		return nil, err
	}
	listener, err := bindSocket(path)
	if err != nil {
		closeBestEffort(lock)
		return nil, err
	}
	return &controlListener{listener: listener, lock: lock}, nil
}

// clientSet tracks live client connections so shutdown can close and join them.
type clientSet struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// add registers conn unless the server is already stopping.
func (c *clientSet) add(ctx context.Context, conn net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	c.conns[conn] = struct{}{}
	return true
}

func (c *clientSet) remove(conn net.Conn) {
	c.mu.Lock()
	delete(c.conns, conn)
	c.mu.Unlock()
}

func (c *clientSet) closeAll(logger *slog.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for conn := range c.conns {
		closeLogged(logger, "control client", conn)
	}
}

// Run serves at most two clients and joins every client before returning.
func Run(ctx context.Context, path string, peers PeerSource, radio *status.RadioStatus, config ConfigBackend, logger *slog.Logger) error {
	if ctx.Err() != nil {
		return nil
	}
	owned, err := listen(path)
	if err != nil {
		return err
	}
	defer closeLogged(logger, "control lock", owned.lock)
	socketInfo, err := os.Lstat(path)
	if err != nil {
		closeBestEffort(owned.listener)
		return err
	}
	defer removeSocket(path, socketInfo, logger)
	clients := &clientSet{conns: make(map[net.Conn]struct{})}
	stopping := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
		case <-stopping:
		}
		closeLogged(logger, "control listener", owned.listener)
		clients.closeAll(logger)
	}()
	serveErr := acceptLoop(ctx, owned.listener, clients, peers, radio, config, logger)
	close(stopping)
	<-watcherDone
	clients.wg.Wait()
	return serveErr
}

func removeSocket(path string, socketInfo os.FileInfo, logger *slog.Logger) {
	if current, err := os.Lstat(path); err == nil && os.SameFile(socketInfo, current) {
		if err := os.Remove(path); err != nil {
			logger.Warn("control socket cleanup failed", "error", err)
		}
	}
}

// unixAcceptor is the part of *net.UnixListener the accept loop uses.
type unixAcceptor interface {
	AcceptUnix() (*net.UnixConn, error)
}

const (
	acceptBackoffMin = 10 * time.Millisecond
	acceptBackoffMax = 5 * time.Second
)

// transientAccept reports resource-exhaustion and interruption errors that a
// later accept can outlive; anything else ends the server.
func transientAccept(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM, syscall.ECONNABORTED, syscall.EINTR} {
		if errors.Is(err, errno) {
			return true
		}
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// acceptLoop serves clients until cancellation or a non-transient accept error.
// Transient accept errors back off with jitter and retry.
func acceptLoop(ctx context.Context, listener unixAcceptor, clients *clientSet, peers PeerSource, radio *status.RadioStatus, config ConfigBackend, logger *slog.Logger) error {
	// The two tokens are the contract's maximum in-flight client handlers.
	slots := make(chan struct{}, 2)
	delay := acceptBackoffMin
	for ctx.Err() == nil {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		conn, err := listener.AcceptUnix()
		if err != nil {
			<-slots
			if ctx.Err() != nil {
				return nil
			}
			if !transientAccept(err) {
				return err
			}
			logger.Warn("control accept failed; retrying", "error", err, "delay", delay)
			if !sleepContext(ctx, delay+rand.N(delay/4+1)) {
				return nil
			}
			delay = min(delay*2, acceptBackoffMax)
			continue
		}
		delay = acceptBackoffMin
		if !clients.add(ctx, conn) {
			closeLogged(logger, "control client", conn)
			<-slots
			return nil
		}
		clients.wg.Add(1)
		go func() {
			defer clients.wg.Done()
			defer func() { closeLogged(logger, "control client", conn); clients.remove(conn); <-slots }()
			handle(ctx, conn, peers, radio, config, logger)
		}()
	}
	return nil
}

// sleepContext waits for d and reports false if ctx ended first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func handle(ctx context.Context, conn net.Conn, peers PeerSource, radio *status.RadioStatus, config ConfigBackend, logger *slog.Logger) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return
	}
	// ReadSlice is bounded by this fixed buffer, including the terminating newline.
	reader := bufio.NewReaderSize(conn, MaxFrame)
	frame, err := reader.ReadSlice('\n')
	result := response{Error: "invalid_request"}
	if errors.Is(err, bufio.ErrBufferFull) {
		result.Error = "frame_too_large"
	} else if err == nil {
		result = dispatch(ctx, frame[:len(frame)-1], peers, radio, config)
	}
	data, err := json.Marshal(result)
	if err != nil || len(data)+1 > MaxFrame {
		data = []byte(`{"ok":false,"error":"response_too_large"}`)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return
	}
	if _, err := io.Copy(conn, bytes.NewReader(append(data, '\n'))); err != nil {
		logger.Debug("control response write failed")
	}
}

func dispatch(ctx context.Context, frame []byte, peers PeerSource, radio *status.RadioStatus, config ConfigBackend) response {
	fail := func(code string) response { return response{Error: code} }
	if jsoncheck.Validate(frame, 2) != nil {
		return fail("invalid_request")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(frame, &fields) != nil {
		return fail("invalid_request")
	}
	for key := range fields {
		if key != "op" && key != "config" {
			return fail("invalid_request")
		}
	}
	var op string
	if string(fields["op"]) == "null" || json.Unmarshal(fields["op"], &op) != nil {
		return fail("invalid_request")
	}
	if op != "status" && op != "config.get" && op != "config.validate" && op != "config.save" {
		return fail("unknown_operation")
	}
	if op == "status" || op == "config.get" {
		if len(fields) != 1 {
			return fail("invalid_request")
		}
		if op == "status" {
			return response{OK: true, Result: StatusSnapshot{Health: radio.Snapshot(), Peers: peers.Snapshot(), PeerTable: peers.PeerTable()}}
		}
		value, err := config.Snapshot(ctx)
		if err != nil {
			return fail("config_read_failed")
		}
		return response{OK: true, Result: value}
	}
	var draft string
	if len(fields) != 2 || string(fields["config"]) == "null" || json.Unmarshal(fields["config"], &draft) != nil {
		return fail("invalid_request")
	}
	if len(draft) > 64<<10 {
		return fail("config_too_large")
	}
	if config.Validate(ctx, draft) != nil {
		return fail("invalid_config")
	}
	if op == "config.save" {
		// Only the disabled state gates a save: the saved file may be the very
		// thing that is broken, and the operator must still be able to replace it.
		if config.Disabled() != "" {
			return fail("save_disabled")
		}
		if config.Save(ctx, draft) != nil {
			return fail("config_save_failed")
		}
	}
	return response{OK: true, Result: struct{}{}}
}

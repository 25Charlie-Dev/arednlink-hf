package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"arednlink-hf/internal/control"
)

// ConfigEditor validates drafts and saves only to the startup configuration.
// The running configuration is an immutable serialized snapshot.
type ConfigEditor struct {
	mu       sync.Mutex
	running  string
	root     *os.Root
	name     string
	disabled string
}

var _ control.ConfigBackend = (*ConfigEditor)(nil)

// NewConfigEditor pins a private configuration directory for this process.
// An unsafe or absent path disables saving without disabling validation.
func NewConfigEditor(cfg Config, path string) (*ConfigEditor, error) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	e := &ConfigEditor{running: string(data), disabled: "Save is disabled: start with -config in a private directory owned by the runtime user."}
	if path == "" {
		return e, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return e, nil
	}
	parent := filepath.Dir(abs)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return e, nil
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return e, nil
	}
	e.root, e.name = root, filepath.Base(abs)
	if e.checkFile() != nil {
		if err := root.Close(); err != nil {
			return nil, err
		}
		e.root = nil
		return e, nil
	}
	e.disabled = ""
	return e, nil
}

// Close releases the pinned directory after all control requests finish.
func (e *ConfigEditor) Close() error {
	if e.root != nil {
		return e.root.Close()
	}
	return nil
}

func privateOwned(info os.FileInfo, directory bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0022 == 0 && (directory && info.IsDir() || !directory && info.Mode().IsRegular())
}

func (e *ConfigEditor) checkFile() error {
	dir, err := e.root.Stat(".")
	if err != nil || !privateOwned(dir, true) {
		return errors.New("configuration directory must be private and owned by the runtime user")
	}
	info, err := e.root.Lstat(e.name)
	if err != nil || !privateOwned(info, false) {
		return errors.New("configuration must be a regular file owned by the runtime user")
	}
	if info.Size() > maxConfigBytes {
		return errors.New("saved configuration exceeds 64 kib")
	}
	return nil
}

// Snapshot returns copied running and saved documents; it never substitutes defaults on read failure.
func (e *ConfigEditor) Snapshot(ctx context.Context) (control.ConfigSnapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := control.ConfigSnapshot{Running: e.running, Saved: e.running, Disabled: e.disabled}
	if e.root == nil {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := e.checkFile(); err != nil {
		return result, err
	}
	f, err := e.root.Open(e.name)
	if err != nil {
		return result, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return result, readErr
	}
	if closeErr != nil {
		return result, closeErr
	}
	cfg, err := DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return result, err
	}
	canonical, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return result, err
	}
	result.Saved = string(canonical)
	result.Pending = result.Saved != result.Running
	return result, nil
}

// Disabled returns the reason saving is unavailable, or "" when saving is allowed.
func (e *ConfigEditor) Disabled() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.disabled
}

// Validate uses the startup decoder, including build capabilities and probe policy.
func (e *ConfigEditor) Validate(ctx context.Context, draft string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := DecodeConfig(bytes.NewBufferString(draft))
	return err
}

// Save commits a validated draft atomically; it does not apply runtime settings.
func (e *ConfigEditor) Save(ctx context.Context, draft string) (resultErr error) {
	if err := e.Validate(ctx, draft); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.root == nil {
		return errors.New("saving is disabled")
	}
	if err := e.checkFile(); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	name := ".config-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := e.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err := e.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, errors.New("uncommitted temporary configuration cleanup failed"))
		}
	}()
	_, writeErr := io.WriteString(f, draft)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.checkFile(); err != nil {
		return err
	}
	if err := e.root.Rename(name, e.name); err != nil {
		return err
	}
	dir, err := e.root.Open(".")
	if err != nil {
		return errors.New("configuration saved; directory durability could not be confirmed")
	}
	syncErr := dir.Sync()
	closeErr = dir.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("configuration saved; directory durability could not be confirmed")
	}
	return nil
}

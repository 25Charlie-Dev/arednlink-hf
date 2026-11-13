package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunListenerFailureJoinsGuard(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "occupied.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, enabled := range []bool{false, true} {
		cfg := Defaults()
		cfg.ControlSocket = listener.Addr().String()
		cfg.ResourceGuard.Enabled = enabled
		cfg.ResourceGuard.MaxRSSKiB = 1048576
		cfg.ResourceGuard.MinAvailableKiB = 1024
		done := make(chan error, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		go func() { done <- Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
		select {
		case err := <-done:
			if err == nil {
				t.Error("occupied listener reported success")
			}
		case <-ctx.Done():
			t.Error("listener failure did not join resource guard")
		}
		cancel()
	}
}

func TestRunCancelledContextJoinsServices(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cfg := Defaults()
		dir := t.TempDir()
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		cfg.ControlSocket = filepath.Join(dir, "control.sock")
		cfg.ResourceGuard.Enabled = enabled
		done := make(chan error, 1)
		go func() { done <- Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled run retained an owned service")
		}
	}
}

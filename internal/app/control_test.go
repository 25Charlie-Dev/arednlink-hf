package app

import (
	"arednlink-hf/internal/control"
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func controlRequest(t *testing.T, path string, request any) map[string]json.RawMessage {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}
	frame, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(frame, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}
func startControlApp(t *testing.T, cfg Config, path string) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), path) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("app shutdown timeout")
		}
	})
	for end := time.Now().Add(time.Second); time.Now().Before(end); {
		if _, err := os.Stat(cfg.ControlSocket); err == nil {
			return cancel, done
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("app did not start")
	return cancel, done
}
func TestControlAtomicConfigSave(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.ControlSocket = filepath.Join(dir, "control.sock")
	cfg.ResourceGuard.Enabled = false
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	startControlApp(t, cfg, path)
	for _, draft := range []string{`{"unknown":true}`, `{"max_peers":2,"max_peers":3}`} {
		reply := controlRequest(t, cfg.ControlSocket, map[string]string{"op": "config.save", "config": draft})
		if string(reply["error"]) != `"invalid_config"` {
			t.Fatal(reply)
		}
		data, _ := os.ReadFile(path)
		if string(data) != "{}" {
			t.Fatal("invalid config changed file")
		}
	}
	before, _ := os.Stat(path)
	reply := controlRequest(t, cfg.ControlSocket, map[string]string{"op": "config.save", "config": `{"max_peers":12}`})
	if string(reply["ok"]) != "true" {
		t.Fatal(reply)
	}
	after, _ := os.Stat(path)
	if os.SameFile(before, after) || after.Mode().Perm() != 0600 {
		t.Fatal("not atomic private save")
	}
	reply = controlRequest(t, cfg.ControlSocket, map[string]string{"op": "config.get"})
	var snapshot control.ConfigSnapshot
	if err := json.Unmarshal(reply["result"], &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Pending || snapshot.Disabled != "" {
		t.Fatal(snapshot)
	}
	var running, saved Config
	if err := json.Unmarshal([]byte(snapshot.Running), &running); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(snapshot.Saved), &saved); err != nil {
		t.Fatal(err)
	}
	if running.MaxPeers != 64 || saved.MaxPeers != 12 {
		t.Fatal("save applied running settings")
	}
}

func TestControlSaveRepairsInvalidSavedConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.ControlSocket = filepath.Join(dir, "control.sock")
	cfg.ResourceGuard.Enabled = false
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"unknown":true`), 0600); err != nil {
		t.Fatal(err)
	}
	startControlApp(t, cfg, path)
	if reply := controlRequest(t, cfg.ControlSocket, map[string]string{"op": "config.get"}); string(reply["error"]) != `"config_read_failed"` {
		t.Fatalf("broken saved file was readable: %v", reply)
	}
	reply := controlRequest(t, cfg.ControlSocket, map[string]string{"op": "config.save", "config": `{"max_peers":12}`})
	if string(reply["ok"]) != "true" {
		t.Fatalf("save refused on invalid saved file: %v", reply)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"max_peers":12}` {
		t.Fatalf("saved file = %q, %v", data, err)
	}
	reply = controlRequest(t, cfg.ControlSocket, map[string]string{"op": "config.get"})
	if string(reply["ok"]) != "true" {
		t.Fatalf("repaired file unreadable: %v", reply)
	}
}

func TestControlStatusReportsPeerTablePolicy(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.ControlSocket = filepath.Join(dir, "control.sock")
	cfg.ResourceGuard.Enabled = false
	cfg.PeerTable.FullPolicy = "refuse"
	startControlApp(t, cfg, "")
	reply := controlRequest(t, cfg.ControlSocket, map[string]string{"op": "status"})
	var result struct {
		PeerTable map[string]any `json:"peer_table"`
	}
	if err := json.Unmarshal(reply["result"], &result); err != nil {
		t.Fatal(err)
	}
	if result.PeerTable["policy"] != "refuse" || result.PeerTable["refused"] != float64(0) || result.PeerTable["evicted"] != float64(0) {
		t.Fatalf("peer_table = %v", result.PeerTable)
	}
}

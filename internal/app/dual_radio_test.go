//go:build kiss && js8call

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"arednlink-hf/internal/control"
)

func dualKISSFrame(source, payload string) []byte {
	address := func(call string, last bool) []byte {
		out := make([]byte, 7)
		for i := 0; i < 6; i++ {
			out[i] = ' ' << 1
			if i < len(call) {
				out[i] = call[i] << 1
			}
		}
		out[6] = 0x60
		if last {
			out[6]++
		}
		return out
	}
	frame := []byte{0xc0, 0}
	frame = append(frame, address("AREDNH", false)...)
	frame = append(frame, address(source, true)...)
	frame = append(frame, 0x03, 0xf0)
	frame = append(frame, payload...)
	return append(frame, 0xc0)
}

func dualSnapshot(t *testing.T, path string) control.StatusSnapshot {
	t.Helper()
	reply := controlRequest(t, path, map[string]string{"op": "status"})
	var snapshot control.StatusSnapshot
	if err := json.Unmarshal(reply["result"], &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func waitDual(t *testing.T, path string, condition func(control.StatusSnapshot) bool) control.StatusSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := dualSnapshot(t, path)
		if condition(snapshot) {
			return snapshot
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("dual-adapter state did not settle")
	return control.StatusSnapshot{}
}

const (
	dualJS8Line = `{"type":"RX.DIRECTED","value":"%[1]s: N0CALL AL1 %[2]s:49173 90 \u2662","params":{"FROM":"%[1]s","TO":"N0CALL","CMD":" ","EXTRA":"","TEXT":"%[1]s: N0CALL AL1 %[2]s:49173 90 \u2662"}}` + "\n"
)

func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeQuietly(t, l) })
	return l
}

func acceptConn(t *testing.T, l net.Listener) net.Conn {
	t.Helper()
	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeQuietly(t, conn) })
	return conn
}

// startDualApp runs the app and registers a cleanup that stops and joins it.
func startDualApp(t *testing.T, cfg Config) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("adapter loops did not join")
		}
		if _, err := os.Stat(cfg.ControlSocket); !os.IsNotExist(err) {
			t.Errorf("control socket retained: %v", err)
		}
	})
	for deadline := time.Now().Add(2 * time.Second); ; {
		if _, err := os.Stat(cfg.ControlSocket); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

func writeJS8Directed(t *testing.T, conn net.Conn, from, ip string) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, dualJS8Line, from, ip); err != nil {
		t.Fatal(err)
	}
}

func TestDualAdaptersIndependentReconnectAndJoin(t *testing.T) {
	kiss := listenLoopback(t)
	js8 := listenLoopback(t)
	dir := t.TempDir()
	setMode(t, dir, 0700)
	cfg := Defaults()
	cfg.ControlSocket = filepath.Join(dir, "control.sock")
	cfg.Radios = []RadioConfig{{Kind: "kiss", Endpoint: kiss.Addr().String()}, {Kind: "js8call", Endpoint: js8.Addr().String(), Callsign: "N0CALL", Destination: "@AREDN"}}
	startDualApp(t, cfg)
	kissConn := acceptConn(t, kiss)
	js8Conn := acceptConn(t, js8)
	initial := waitDual(t, cfg.ControlSocket, func(s control.StatusSnapshot) bool { return len(s.Health.Adapters) == 2 && s.Health.Healthy })
	if initial.Health.Kind != "multiple" || initial.Health.Adapters[0].Kind != "kiss" || initial.Health.Adapters[1].Kind != "js8call" || initial.Health.Adapters[0].TX != nil || initial.Health.Adapters[1].TX != nil {
		t.Fatalf("wrong initial health: %+v", initial.Health)
	}
	if _, err := kissConn.Write(dualKISSFrame("W1AW", "AL1 198.51.100.27:49173 90")); err != nil {
		t.Fatal(err)
	}
	writeJS8Directed(t, js8Conn, "W2AW", "198.51.100.28")
	waitDual(t, cfg.ControlSocket, func(s control.StatusSnapshot) bool {
		return len(s.Peers) == 2 && s.Health.Adapters[1].RX != nil && len(s.Health.Adapters[1].RX.RecentCallsigns) == 1
	})
	if err := kissConn.Close(); err != nil {
		t.Fatal(err)
	}
	degraded := waitDual(t, cfg.ControlSocket, func(s control.StatusSnapshot) bool {
		return len(s.Health.Adapters) == 2 && s.Health.Adapters[0].State == "backoff" && s.Health.Adapters[1].State == "connected"
	})
	if degraded.Health.Healthy || degraded.Health.State != "degraded" {
		t.Fatalf("loss hidden: %+v", degraded.Health)
	}
	writeJS8Directed(t, js8Conn, "W3AW", "198.51.100.29")
	waitDual(t, cfg.ControlSocket, func(s control.StatusSnapshot) bool {
		return len(s.Peers) == 3 && s.Health.Adapters[1].State == "connected"
	})
	if err := kiss.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	acceptConn(t, kiss)
	waitDual(t, cfg.ControlSocket, func(s control.StatusSnapshot) bool {
		return s.Health.Healthy && s.Health.Adapters[0].State == "connected"
	})
}

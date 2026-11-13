//go:build js8call

package app

import (
	"arednlink-hf/internal/control"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestFakeRadioToControlSnapshot(t *testing.T) {
	modem, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(t, modem)
	dir := t.TempDir()
	setMode(t, dir, 0700)
	cfg := Defaults()
	cfg.ControlSocket = filepath.Join(dir, "control.sock")
	cfg.ResourceGuard.Enabled = false
	cfg.Radio = RadioConfig{Kind: "js8call", Endpoint: modem.Addr().String(), Callsign: "N0CALL", Destination: "@AREDN"}
	startControlApp(t, cfg, "")
	if err := modem.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := modem.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(t, conn)
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(conn, `{"type":"RX.DIRECTED","value":"W1AW: N0CALL AL1 198.51.100.27:49173 90 \u2662","params":{"FROM":"W1AW","TO":"N0CALL","CMD":" ","EXTRA":"","TEXT":"W1AW: N0CALL AL1 198.51.100.27:49173 90 \u2662"}}`+"\n")
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(time.Second); time.Now().Before(end); {
		reply := controlRequest(t, cfg.ControlSocket, map[string]string{"op": "status"})
		var snapshot control.StatusSnapshot
		if err := json.Unmarshal(reply["result"], &snapshot); err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Peers) > 0 {
			if snapshot.Health.State != "connected" || snapshot.Health.RX == nil || len(snapshot.Health.RX.RecentCallsigns) != 1 || snapshot.Health.RX.RecentCallsigns[0].Callsign != "W1AW" || snapshot.Health.TX != nil || snapshot.Peers[0].Probe != "NOT_CHECKED" {
				t.Fatalf("incorrect radio snapshot: %+v", snapshot)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("synthetic advertisement did not reach IPC")
}

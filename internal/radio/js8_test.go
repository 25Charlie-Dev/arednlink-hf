//go:build js8call

package radio

import (
	"arednlink-hf/internal/protocol"
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func directedJSON(from, to, command, text string) []byte {
	record := map[string]any{
		"type": "RX.DIRECTED", "value": text,
		"params": map[string]any{"FROM": from, "TO": to, "CMD": command, "EXTRA": "", "TEXT": text},
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		panic(err)
	}
	return append(encoded, '\n')
}
func TestJS8CompleteRecord(t *testing.T) {
	body := "AL1 198.51.100.27:49173 90"
	for _, to := range []string{"N0CALL", "@AREDN"} {
		for _, cmd := range []string{" ", " MSG", " MSG "} {
			parser, err := newJS8(Config{Callsign: "N0CALL", Destination: "@AREDN"})
			if err != nil {
				t.Fatal(err)
			}
			prefix, _ := directedPrefix("W1AW/P", to, cmd)
			wire := directedJSON("W1AW/P", to, cmd, prefix+body+" "+endMarker+" ")
			var got []Message
			for _, b := range wire {
				parser.feed([]byte{b}, func(m Message) { got = append(got, m) })
			}
			if len(got) != 1 || got[0].Source != "W1AW/P" || got[0].Payload != body {
				t.Fatalf("got %#v", got)
			}
		}
	}
}

func TestJS8AcceptsCompactAdvertisement(t *testing.T) {
	for _, body := range []string{"AL2 PFFBXVIDLIKO", "AL3 PFFBXVIDLIKO 00V"} {
		parser, err := newJS8(Config{Callsign: "N0CALL"})
		if err != nil {
			t.Fatal(err)
		}
		var got []Message
		parser.feed(directedJSON("W1AW", "N0CALL", " ", "W1AW: N0CALL "+body+" "+endMarker), func(m Message) { got = append(got, m) })
		if len(got) != 1 || got[0].Payload != body {
			t.Fatalf("body %q got %#v", body, got)
		}
	}
}

func TestJS8BeaconMessageEnvelope(t *testing.T) {
	transmitter, err := newJS8Transmitter(Config{Kind: "js8call", Callsign: "N0CALL", Destination: "@AREDN"})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := transmitter.encode("AL2 PFFBXVIDLIKO", 42)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Type   string `json:"type"`
		Value  string `json:"value"`
		Params struct {
			ID       uint64 `json:"_ID"`
			Priority string `json:"PRIORITY"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame[:len(frame)-1], &request); err != nil {
		t.Fatal(err)
	}
	if request.Type != "TX.SEND_MESSAGE" || request.Value != "@AREDN MSG AL2 PFFBXVIDLIKO" || request.Params.ID != 42 || request.Params.Priority != "HIGH" {
		t.Fatalf("JS8 send request = %+v", request)
	}
}

func TestJS8BeaconRefusalResponseIsNotAcceptance(t *testing.T) {
	var gotID uint64
	var gotReason string
	parser, err := newJS8(Config{Callsign: "N0CALL", OnTransmitRefused: func(id uint64, reason string) {
		gotID, gotReason = id, reason
	}})
	if err != nil {
		t.Fatal(err)
	}
	rejected := parser.feed([]byte(`{"type":"TX.SEND_MESSAGE","value":"","params":{"_ID":42,"REFUSED":true,"REASON":"auto_route_active"}}`+"\n"), func(Message) {
		t.Fatal("API refusal became an advertisement")
	})
	if rejected != 0 || gotID != 42 || gotReason != "auto_route_active" {
		t.Fatalf("refusal result id=%d reason=%q rejected=%d", gotID, gotReason, rejected)
	}
}

// serveJS8Refusal plays a synthetic API: it reads one request and answers it with a refusal.
func serveJS8Refusal(t *testing.T, listener net.Listener, requestSeen chan<- string, serverDone chan<- struct{}) {
	defer close(serverDone)
	peer, acceptErr := listener.Accept()
	if acceptErr != nil {
		return
	}
	defer closeQuietly(t, peer)
	line, readErr := bufio.NewReaderSize(peer, 1024).ReadString('\n')
	if readErr != nil {
		return
	}
	requestSeen <- line
	var request struct {
		Params struct {
			ID uint64 `json:"_ID"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(line), &request) != nil {
		return
	}
	response, marshalErr := json.Marshal(map[string]any{"type": "TX.SEND_MESSAGE", "value": "", "params": map[string]any{
		"_ID": request.Params.ID, "REFUSED": true, "REASON": "auto_route_active"}})
	if marshalErr != nil {
		return
	}
	if _, writeErr := peer.Write(append(response, '\n')); writeErr != nil {
		return
	}
}

// checkJS8Request verifies the transmit request and that its payload also round-trips through the decoder.
func checkJS8Request(t *testing.T, cfg Config, line, payload string) {
	t.Helper()
	var request struct {
		Type   string `json:"type"`
		Value  string `json:"value"`
		Params struct {
			ID       uint64 `json:"_ID"`
			Priority string `json:"PRIORITY"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &request); err != nil {
		t.Fatal(err)
	}
	if request.Type != "TX.SEND_MESSAGE" || request.Value != "@AREDN MSG "+payload || request.Params.Priority != "HIGH" || request.Params.ID != 1 {
		t.Fatalf("synthetic modem request = %+v", request)
	}
	decoder, decodeErr := newJS8(cfg)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	var received []Message
	decoder.feed(directedJSON("W1AW", "N0CALL", " ", "W1AW: N0CALL "+payload+" "+endMarker), func(message Message) { received = append(received, message) })
	if len(received) != 1 || received[0].Payload != payload {
		t.Fatalf("AL4 JS8 envelope rejected: %+v", received)
	}
}

func TestJS8BeaconThroughSyntheticAPIAndRefusal(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(t, listener)
	states := make([]string, 0, 3)
	payload, err := protocol.EncodeCompact(protocol.Advertisement{Address: netip.MustParseAddr("198.51.100.27"), ValidFor: 90 * time.Minute, Services: protocol.ServiceSignalProxy | protocol.ServiceSimpleXSMP, ServicesPresent: true})
	if err != nil || !strings.HasPrefix(payload, "AL4 ") || !protocol.HasAdvertisementPrefix(payload) {
		t.Fatalf("invalid AL4 payload %q: %v", payload, err)
	}
	cfg := Config{Kind: "js8call", Callsign: "N0CALL", Destination: "@AREDN",
		Beacon: BeaconConfig{Enabled: true, Address: netip.MustParseAddr("198.51.100.27"), Services: protocol.ServiceSignalProxy | protocol.ServiceSimpleXSMP,
			ValidityMinutes: 90, Interval: 30 * time.Minute},
		OnTransmit: func(state string) { states = append(states, state) }}
	var requestID uint64
	var refusal string
	cfg.OnTransmitRequest = func(id uint64) { requestID = id }
	cfg.OnTransmitRefused = func(id uint64, reason string) {
		if id == requestID {
			refusal = reason
		}
	}
	requestSeen := make(chan string, 1)
	serverDone := make(chan struct{})
	go serveJS8Refusal(t, listener, requestSeen, serverDone)
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := newJS8Transmitter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	schedule := &beaconSchedule{config: cfg.Beacon, writer: writer, next: time.Now().Add(-time.Second)}
	reports := eventLog{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan error, 1)
	go func() {
		done <- receive(context.Background(), client, cfg, func(context.Context, Message) error { return nil }, &reports, schedule)
	}()
	select {
	case line := <-requestSeen:
		checkJS8Request(t, cfg, line, payload)
	case <-time.After(time.Second):
		t.Fatal("synthetic API received no message")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("receive returned nil after synthetic API close")
		}
	case <-time.After(time.Second):
		t.Fatal("receive did not stop after synthetic API close")
	}
	<-serverDone
	if requestID != 1 || refusal != "auto_route_active" || strings.Join(states, ",") != "queued,writing,written" {
		t.Fatalf("tx request=%d refusal=%q states=%v", requestID, refusal, states)
	}
}
func TestJS8RejectsPartialAmbiguousAndChat(t *testing.T) {
	body := "AL1 198.51.100.27:49173 90"
	good := directedJSON("W1AW", "N0CALL", " ", "W1AW: N0CALL "+body+" "+endMarker+" ")
	for _, wire := range [][]byte{
		directedJSON("W1AW", "OTHER", " ", "W1AW: OTHER "+body+" "+endMarker),
		directedJSON("W1AW", "N0CALL", " ", "W1AW: N0CALL "+body),
		directedJSON("W1AW", "N0CALL", " ", "W1AW: N0CALL chat "+body+" "+endMarker),
		directedJSON("W1AW", "N0CALL", " ", "N1OTHER: N0CALL "+body+" "+endMarker),
		directedJSON("W1AW", "N0CALL", " ACK", "W1AW: N0CALL ACK "+body+" "+endMarker),
		directedJSON("W1AW", "N0CALL", " ", "W1AW: N0CALL "+body+" "+endMarker+endMarker),
		directedJSON("W1AW", "N0CALL", " ", "W1AW: N0CALL "+body+endMarker),
		directedJSON("W1AW", "N0CALL", " ", body+" "+endMarker),
		[]byte(strings.Replace(string(good), "RX.DIRECTED", "RX.ACTIVITY", 1)),
		[]byte(strings.Replace(string(good), `"FROM":"W1AW"`, `"FROM":"W1AW","FROM":"W2AW"`, 1)),
		[]byte(strings.Replace(string(good), `"FROM":"W1AW"`, `"FROM":"W1AW","from":"W2AW"`, 1)),
		[]byte(strings.Replace(string(good), `"EXTRA":""`, `"EXTRA":"ambiguous"`, 1)),
		[]byte("{\"type\":\"RX.DIRECTED\",\"params\":null}\n"),
		[]byte{'{', 0xff, '}', '\n'},
	} {
		parser, _ := newJS8(Config{Callsign: "N0CALL", Destination: "@AREDN"})
		count := 0
		rejected := parser.feed(wire, func(Message) { count++ })
		if count != 0 || rejected == 0 {
			t.Fatalf("accepted invalid input %q count=%d rejects=%d", wire, count, rejected)
		}
	}
}
func TestJS8BoundsAndResynchronizes(t *testing.T) {
	parser, _ := newJS8(Config{Callsign: "N0CALL"})
	body := "AL1 198.51.100.27:49173 90"
	wire := directedJSON("W1AW", "N0CALL", " ", "W1AW: N0CALL "+body+" "+endMarker)
	count := 0
	emit := func(Message) { count++ }
	rejected := parser.feed([]byte(strings.Repeat("x", maxJS8Line+100)), emit)
	rejected += parser.feed(append([]byte{'\n'}, wire...), emit)
	if rejected != 1 || count != 1 {
		t.Fatalf("count=%d rejects=%d", count, rejected)
	}
	parser, _ = newJS8(Config{Callsign: "N0CALL"})
	parser.feed(wire[:len(wire)-1], emit)
	if count != 1 {
		t.Fatal("record delivered without newline")
	}
}
func TestJS8RejectsTrailingRecord(t *testing.T) {
	parser, _ := newJS8(Config{Callsign: "N0CALL"})
	if _, ok := parser.(*js8Decoder).directed([]byte(`{} {}`)); ok {
		t.Fatal("multiple records accepted")
	}
}

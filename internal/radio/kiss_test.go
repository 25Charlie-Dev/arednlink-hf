//go:build kiss

package radio

import (
	"arednlink-hf/internal/protocol"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func address(call string, last bool) []byte {
	out := make([]byte, 7)
	for i := 0; i < 6; i++ {
		out[i] = ' ' << 1
		if i < len(call) {
			out[i] = call[i] << 1
		}
	}
	out[6] = 0x60
	if last {
		out[6] |= 1
	}
	return out
}
func axFrame(destination, body string) []byte {
	frame := []byte{0}
	frame = append(frame, address(destination, false)...)
	frame = append(frame, address("N0CALL", true)...)
	return append(append(frame, 0x03, 0xf0), []byte(body)...)
}
func kissWire(frame []byte) []byte {
	return encodeKISSWire(frame)
}
func TestKISSFragmentedEscapes(t *testing.T) {
	parser, _ := newKISS(Config{})
	body := "AL1 198.51.100.27:49173 90"
	frame := axFrame("AREDNH", body)
	frame[7] = 0xe0
	var got []Message
	wire := kissWire(frame)
	for _, value := range wire {
		parser.feed([]byte{value}, func(m Message) { got = append(got, m) })
	}
	if len(got) != 1 || got[0].Source != "N0CALL" || got[0].Payload != body {
		t.Fatalf("got %#v", got)
	}
	raw := &kissDecoder{frame: make([]byte, 0, maxKISSFrame)}
	raw.accept(fend, func(Message) {})
	for _, b := range []byte{fesc, 0xdc, fesc, 0xdd} {
		raw.accept(b, func(Message) {})
	}
	if !bytes.Equal(raw.frame, []byte{fend, fesc}) {
		t.Fatalf("decoded escape bytes %x", raw.frame)
	}
}

func TestKISSTransmitAdvertisementFraming(t *testing.T) {
	payload := "AL2 PFFBXVIDLIKO"
	wire, err := encodeKISSBeacon("N0CALL-2", "AREDNH", payload)
	if err != nil {
		t.Fatal(err)
	}
	parser, err := newKISS(Config{Callsign: "N0CALL-2", Destination: "AREDNH"})
	if err != nil {
		t.Fatal(err)
	}
	var got []Message
	parser.feed(wire, func(message Message) { got = append(got, message) })
	if len(got) != 1 || got[0].Source != "N0CALL-2" || got[0].Payload != payload {
		t.Fatalf("transmitted frame decoded as %#v", got)
	}
	if wire[0] != fend || wire[len(wire)-1] != fend {
		t.Fatalf("KISS frame boundary missing: %x", wire)
	}
}

func TestKISSTransmitRejectsInvalidPayloadAndCallsign(t *testing.T) {
	for _, tc := range []struct{ callsign, destination, payload string }{
		{"TOOLONGCALL", "AREDNH", "AL2 PFFBXVIDLIKO"},
		{"N0CALL", "AREDNH", "AL2 lower"},
		{"N0CALL", "AREDNH", strings.Repeat("A", 68)},
	} {
		if _, err := encodeKISSBeacon(tc.callsign, tc.destination, tc.payload); err == nil {
			t.Errorf("accepted invalid TX data: %+v", tc)
		}
	}
}

func TestKISSTransmitDueBeaconThroughSyntheticModem(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	payload, err := protocol.EncodeCompact(protocol.Advertisement{Address: netip.MustParseAddr("198.51.100.27"), ValidFor: 90 * time.Minute, Services: protocol.ServiceSignalProxy | protocol.ServiceSimpleXSMP, ServicesPresent: true})
	if err != nil || !strings.HasPrefix(payload, "AL4 ") || !protocol.HasAdvertisementPrefix(payload) {
		t.Fatalf("invalid AL4 payload %q: %v", payload, err)
	}
	states := make([]string, 0, 3)
	cfg := Config{Kind: "kiss", Callsign: "N0CALL", Destination: "AREDNH",
		Beacon: BeaconConfig{Enabled: true, Address: netip.MustParseAddr("198.51.100.27"), Services: protocol.ServiceSignalProxy | protocol.ServiceSimpleXSMP,
			ValidityMinutes: 90, Interval: 30 * time.Minute},
		OnTransmit: func(state string) { states = append(states, state) }}
	observed := make(chan Message, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		peer, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer peer.Close()
		decoder, _ := newKISS(cfg)
		buffer := make([]byte, 128)
		for {
			count, readErr := peer.Read(buffer)
			if count > 0 {
				received := false
				decoder.feed(buffer[:count], func(message Message) {
					observed <- message
					received = true
				})
				if received {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := makeTransmitter(cfg)
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
	case message := <-observed:
		if message.Source != "N0CALL" || message.Payload != payload || message.Transport != "kiss" {
			t.Fatalf("synthetic modem received %#v", message)
		}
		parsed, parseErr := protocol.Parse(message.Payload)
		if parseErr != nil || parsed.Address != netip.MustParseAddr("198.51.100.27") || parsed.Services != protocol.ServiceSignalProxy|protocol.ServiceSimpleXSMP {
			t.Fatalf("AL4 KISS envelope rejected: %+v, %v", parsed, parseErr)
		}
	case <-time.After(time.Second):
		t.Fatal("synthetic modem received no KISS beacon")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("receive returned nil after synthetic modem close")
		}
	case <-time.After(time.Second):
		t.Fatal("receive did not stop after synthetic modem close")
	}
	<-serverDone
	if got := strings.Join(states, ","); got != "queued,writing,written" {
		t.Fatalf("TX states = %q", got)
	}
}
func TestKISSResynchronizes(t *testing.T) {
	for _, bad := range [][]byte{
		{fend, 0, fesc, 1, 2, fend}, {fend, 0, fesc, fend},
		append(append([]byte{fend}, bytes.Repeat([]byte{1}, maxKISSFrame+20)...), fend),
	} {
		parser, _ := newKISS(Config{})
		count := 0
		emit := func(Message) { count++ }
		rejected := parser.feed(append(bad, kissWire(axFrame("AREDNH", "AL1 198.51.100.27:49173 90"))...), emit)
		if count != 1 || rejected != 1 {
			t.Fatalf("accepted %d rejected %d", count, rejected)
		}
	}
}
func TestAX25Validation(t *testing.T) {
	body := "AL1 198.51.100.27:49173 90"
	for _, tt := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"command", func(b []byte) { b[0] = 1 }}, {"non-ui", func(b []byte) { b[15] = 0x13 }},
		{"pid", func(b []byte) { b[16] = 0xcf }}, {"reserved", func(b []byte) { b[14] &= ^byte(0x60) }},
		{"lowbit", func(b []byte) { b[1] |= 1 }}, {"early-end", func(b []byte) { b[7] |= 1 }},
		{"missing-end", func(b []byte) { b[14] &= ^byte(1) }},
		{"bad-padding", func(b []byte) { b[2] = ' ' << 1; b[3] = 'A' << 1 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			frame := axFrame("AREDNH", body)
			tt.mutate(frame)
			if _, ok := ax25Message(frame, "AREDNH", ""); ok {
				t.Fatal("accepted invalid frame")
			}
		})
	}
	if _, ok := ax25Message(axFrame("OTHER", body), "AREDNH", ""); ok {
		t.Fatal("wrong destination accepted")
	}
	frame := axFrame("AREDNH", body)
	frame[14] |= 4
	m, ok := ax25Message(frame, "AREDNH", "")
	if !ok || m.Source != "N0CALL-2" {
		t.Fatalf("SSID got %#v %v", m, ok)
	}
	long := []byte{0}
	for i := 0; i < 11; i++ {
		long = append(long, address("N0CALL", i == 10)...)
	}
	long = append(long, 3, 0xf0)
	long = append(long, body...)
	if _, ok := ax25Message(long, "N0CALL", ""); ok {
		t.Fatal("accepted eleven addresses")
	}
}
func TestAPRSDirectedWrapper(t *testing.T) {
	body := "AL1 198.51.100.27:49173 90"
	for _, tt := range []struct {
		info string
		ok   bool
	}{
		{":AREDNH   :" + body, true}, {":AREDNH   :" + body + "{a12", true},
		{":AREDNH   :AL2 PFFBXVIDLIKO", true},
		{":AREDNH   :AL2 AJTSGXIYMFLMVZANOYEVIDNKRFDPUJLYW", true},
		{":AREDNH   :AL3 PFFBXVIDLIKO 00V", true},
		{":N0CALL   :" + body, true}, {":OTHER    :" + body, false},
		{":AREDNH   :acka12", false}, {":AREDNH   :" + body + "{123456", false},
		{":AREDNH   :" + body + "{a!", false}, {":AREDNH   :chat AL1 x", false},
		{":AREDNH   :AL1 " + strings.Repeat("x", 68), false},
	} {
		m, ok := ax25Message(axFrame("APRS", tt.info), "AREDNH", "N0CALL")
		want := body
		if strings.Contains(tt.info, "AL2 ") || strings.Contains(tt.info, "AL3 ") {
			want = strings.TrimPrefix(tt.info, ":AREDNH   :")
		}
		if ok != tt.ok || ok && m.Payload != want {
			t.Fatalf("%q got %#v %v", tt.info, m, ok)
		}
	}
	for _, payload := range []string{"AL2 PFFBXVIDLIKO", "AL2 AJTSGXIYMFLMVZANOYEVIDNKRFDPUJLYW", "AL3 PFFBXVIDLIKO 00V"} {
		m, ok := ax25Message(axFrame("AREDNH", payload), "AREDNH", "")
		if !ok || m.Payload != payload {
			t.Fatalf("direct KISS payload %q got %#v %v", payload, m, ok)
		}
	}
}

func TestKISSConfigValidation(t *testing.T) {
	for _, call := range []string{"N0CALL", "N0CALL-0", "N0CALL-15"} {
		if _, err := newKISS(Config{Callsign: call}); err != nil {
			t.Fatal(err)
		}
	}
	for _, call := range []string{"N0CALL/P", "N0CALL-16", "N0CALL-01", "N0CALL-", "N0CALL-too-long", "TOOLONG"} {
		if _, err := newKISS(Config{Callsign: call}); err == nil {
			t.Fatalf("accepted %q", call)
		}
	}
}

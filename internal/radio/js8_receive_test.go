//go:build js8call

package radio

import (
	"strings"
	"testing"
)

func TestJS8PassiveReceptionBeforeAdvertisementFiltering(t *testing.T) {
	var heard []string
	parser, err := newJS8(Config{Callsign: "N0CALL", Destination: "@AREDN",
		OnReceive: func(call string) { heard = append(heard, call) }})
	if err != nil {
		t.Fatal(err)
	}
	frames := [][]byte{
		directedJSON("W1AW", "W2OTHER", " ", "W1AW: W2OTHER ordinary chat"),
		directedJSON("W2AW", "@ALLCALL", " CQ", "W2AW: @ALLCALL CQ"),
		directedJSON("W3AW", "@HB", " HEARTBEAT", "W3AW: @HB HEARTBEAT"),
		[]byte(`{"type":"RX.ACTIVITY","value":"W4AW/P: first frame","params":{"UTC":1}}` + "\n"),
		[]byte(`{"type":"RX.ACTIVITY","value":"continuation mentions W9AW: text","params":{}}` + "\n"),
		[]byte(`{"type":"RX.DIRECTED","value":"value only","params":{"FROM":"W5AW"}}` + "\n"),
		[]byte(`{"type":"RX.DIRECTED","params":{"FROM":"W6AW","TEXT":"text only"}}` + "\n"),
		directedJSON("N0CALL", "@HB", " HEARTBEAT", "N0CALL: @HB HEARTBEAT"),
		[]byte(`{"type":"RX.ACTIVITY","value":"N0CALL: local station","params":{}}` + "\n"),
	}
	for _, frame := range frames {
		for _, value := range frame {
			parser.feed([]byte{value}, func(Message) { t.Fatal("ordinary receive event became advertisement") })
		}
	}
	if got := strings.Join(heard, ","); got != "W1AW,W2AW,W3AW,W4AW/P,,W5AW,W6AW" {
		t.Fatalf("receive claims: %q", got)
	}
}

func TestJS8InvalidAndHistoricalEventsDoNotRefreshReception(t *testing.T) {
	frames := []string{
		`{"type":"RX.ACTIVITY","value":"","params":{}}`,
		`{"type":"RX.ACTIVITY","value":"   ","params":{}}`,
		`{"type":"RX.ACTIVITY","params":{}}`,
		`{"type":"RX.ACTIVITY","value":null,"params":{}}`,
		`{"type":"RX.ACTIVITY","value":12,"params":{}}`,
		`{"type":"RX.ACTIVITY","value":"text"}`,
		`{"type":"RX.ACTIVITY","value":"text","params":null}`,
		`{"type":"RX.ACTIVITY","value":"text","params":[]}`,
		`{"type":"RX.ACTIVITY","value":"text","params":{}} {}`,
		`{"type":"RX.ACTIVITY","value":"text","VALUE":"other","params":{}}`,
		`{"type":"RX.ACTIVITY","value":"text","params":{"x":1,"X":2}}`,
		`{"type":"RX.ACTIVITY","value":"text\u0000","params":{}}`,
		`{"type":"RX.DIRECTED","value":"chat","params":{"FROM":null}}`,
		`{"type":"RX.DIRECTED","value":"chat","params":{"FROM":42}}`,
		`{"type":"RX.DIRECTED","value":"chat","params":{"FROM":"w1aw"}}`,
		`{"type":"RX.DIRECTED","value":"chat","params":{"FROM":"W1AW","from":"W2AW"}}`,
		`{"type":"RX.DIRECTED","value":"chat","params":{"FROM":"W1AW","TEXT":"different"}}`,
		`{"type":"RX.DIRECTED","value":"chat","params":{"FROM":"W1AW","TEXT":null}}`,
		`{"type":"RX.DIRECTED","value":"chat","params":{"FROM":"W1AW","TEXT":42}}`,
		`{"type":"RX.DIRECTED","params":{"FROM":"W1AW"}}`,
		`{"type":"RX.DIRECTED","value":"","params":{"FROM":"W1AW","TEXT":""}}`,
		`{"type":"RX.CALL_ACTIVITY","value":"W1AW: chat","params":{"FROM":"W1AW","TEXT":"W1AW: chat"}}`,
		`{"type":"TX.FRAME","value":"W1AW: chat","params":{}}`,
		`{"type":"RIG.FREQ","value":"W1AW: chat","params":{}}`,
		`null`,
		"{\"type\":\"RX.ACTIVITY\",\"value\":\"\xff\",\"params\":{}}",
		`{"type":"RX.ACTIVITY","value":"` + strings.Repeat("x", maxJS8Line) + `","params":{}}`,
	}
	parser, _ := newJS8(Config{Callsign: "N0CALL", OnReceive: func(string) { t.Fatal("invalid or historical event counted") }})
	for _, frame := range frames {
		parser.feed([]byte(frame+"\n"), func(Message) { t.Fatalf("invalid event became advertisement: %s", frame) })
	}
}

func TestJS8AdvertisementAlsoRefreshesReception(t *testing.T) {
	count, received := 0, 0
	parser, _ := newJS8(Config{Callsign: "N0CALL", OnReceive: func(call string) {
		if call != "W1AW" {
			t.Fatalf("unexpected claim %q", call)
		}
		received++
	}})
	parser.feed(directedJSON("W1AW", "N0CALL", " ", "W1AW: N0CALL AL1 198.51.100.27:49173 90 "+endMarker),
		func(Message) { count++ })
	if count != 1 || received != 1 {
		t.Fatalf("advertisements=%d received=%d", count, received)
	}
}

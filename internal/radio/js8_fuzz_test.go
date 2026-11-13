//go:build js8call

package radio

import (
	"slices"
	"testing"
)

// FuzzJS8Feed checks that every emitted message meets the decoder contract,
// that the line buffer stays bounded, and that decoding does not depend on
// how the byte stream is split into reads.
func FuzzJS8Feed(f *testing.F) {
	f.Add([]byte(`{"type":"RX.DIRECTED","value":"W1AW: N0CALL AL2 PFFBXVIDLIKO","params":{"FROM":"W1AW","TO":"N0CALL","CMD":" ","EXTRA":"","TEXT":"W1AW: N0CALL AL2 PFFBXVIDLIKO"}}` + "\n"))
	f.Add([]byte(`{"type":"RX.ACTIVITY","value":"W1AW: hi","params":{}}` + "\n"))
	f.Add([]byte(`{"type":"RX.DIRECTED"`))
	f.Add([]byte("\n\n{}\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		run := func(chunk int) ([]Message, uint64) {
			d, err := newJS8(Config{Callsign: "N0CALL", Destination: "@AREDN", OnReceive: func(string) {}, OnTransmitRefused: func(uint64, string) {}})
			if err != nil {
				t.Fatal(err)
			}
			var got []Message
			var rejected uint64
			emit := func(m Message) {
				if len(m.Payload) > 67 || m.Transport != "js8call" {
					t.Fatalf("emitted out-of-contract message %#v", m)
				}
				got = append(got, m)
			}
			if chunk == 0 {
				rejected = d.feed(data, emit)
			}
			for i := 0; chunk > 0 && i < len(data); i += chunk {
				rejected += d.feed(data[i:min(i+chunk, len(data))], emit)
			}
			if cap(d.(*js8Decoder).line) > maxJS8Line {
				t.Fatalf("line buffer grew to %d", cap(d.(*js8Decoder).line))
			}
			return got, rejected
		}
		whole, wholeRejected := run(0)
		bytewise, bytewiseRejected := run(1)
		if !slices.Equal(whole, bytewise) || wholeRejected != bytewiseRejected {
			t.Fatalf("decoding depends on read boundaries: %#v/%d vs %#v/%d", whole, wholeRejected, bytewise, bytewiseRejected)
		}
	})
}

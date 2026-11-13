//go:build kiss

package radio

import (
	"slices"
	"testing"
	"unicode/utf8"
)

// FuzzKISSFeed checks that every emitted message meets the decoder contract,
// that the frame buffer stays bounded, and that decoding does not depend on
// how the byte stream is split into reads.
func FuzzKISSFeed(f *testing.F) {
	f.Add(encodeKISSWire(append([]byte{0}, []byte("xxxxxxxxxxxxxxxxxxxxxx")...)))
	f.Add([]byte{fend, fesc, fend})
	f.Add([]byte{fend, 0, fesc, 0x00, fend})
	if w, err := encodeKISSBeacon("N0CALL", "AREDNH", "AL2 PFFBXVIDLIKO"); err == nil {
		f.Add(w)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		run := func(chunk int) ([]Message, uint64) {
			d, err := newKISS(Config{Callsign: "N0CALL"})
			if err != nil {
				t.Fatal(err)
			}
			var got []Message
			var rejected uint64
			emit := func(m Message) {
				if len(m.Payload) > 67 || !utf8.ValidString(m.Payload) || m.Transport != "kiss" || m.Source == "" {
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
			if cap(d.(*kissDecoder).frame) > maxKISSFrame {
				t.Fatalf("frame buffer grew to %d", cap(d.(*kissDecoder).frame))
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

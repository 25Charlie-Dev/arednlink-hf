package control

import (
	"context"
	"encoding/json"
	"testing"

	"arednlink-hf/internal/status"
)

// FuzzDispatch checks that every frame yields exactly one of success or a
// named error code, that the response is marshalable, and that requests
// rejected by parseRequest never reach the backend.
func FuzzDispatch(f *testing.F) {
	for _, s := range []string{
		`{"op":"status"}`,
		`{"op":"config.get"}`,
		`{"op":"config.validate","config":"{}"}`,
		`{"op":"config.save","config":"{}"}`,
		`{"op":"config.get","op":"status"}`,
		`{"op":"status","extra":1}`,
		`{"op":null}`,
		`{"op":"nope"}`,
		`{"op":"config.save","config":null}`,
		`[]`,
		``,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, frame []byte) {
		r := dispatch(context.Background(), frame, emptyPeers{}, status.NewRadioStatus("none"), configStub{})
		if r.OK == (r.Error != "") {
			t.Fatalf("inconsistent response %#v for %q", r, frame)
		}
		if _, err := json.Marshal(r); err != nil {
			t.Fatalf("response for %q not marshalable: %v", frame, err)
		}
		op, _, code := parseRequest(frame)
		if (code != "") && r.Error != code {
			t.Fatalf("parseRequest rejected %q with %q but dispatch returned %#v", frame, code, r)
		}
		if code == "" && op == "" {
			t.Fatalf("parseRequest accepted %q without an operation", frame)
		}
	})
}

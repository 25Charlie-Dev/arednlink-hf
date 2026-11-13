package jsoncheck

import (
	"encoding/json"
	"testing"
)

// FuzzValidateImpliesDecodable checks that input accepted by Validate always
// decodes into a generic object and into a map of raw members.
func FuzzValidateImpliesDecodable(f *testing.F) {
	for _, s := range []string{
		`{}`,
		`{"a":1,"b":[{"c":"d"}]}`,
		`{"a":1,"A":2}`,
		`{"a":1,"a":2}`,
		`{"a":{"b":{"c":{}}}}`,
		`{"a":1} {"b":2}`,
		`[1,2]`,
		`{"a":`,
		``,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			return
		}
		if Validate(data, 8) != nil {
			return
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("validated but undecodable: %q: %v", data, err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("validated but raw decode failed: %q: %v", data, err)
		}
		if len(raw) != len(m) {
			t.Fatalf("validated input has ambiguous members: %q", data)
		}
	})
}

package protocol

import "testing"

// FuzzParseRoundTrip checks that any accepted advertisement re-encodes to a
// canonical text that parses back to an identical value.
func FuzzParseRoundTrip(f *testing.F) {
	for _, s := range []string{
		"AL1 198.51.100.27:49173 90",
		"AL1 [2001:db8::1]:49173 90",
		"AL1 198.51.100.27:49173 181",
		"AL2 PFFBXVIDLIKO",
		"AL2 AJTSGXIYMFLMVZANOYEVIDNKRFDPUJLYW",
		"AL3 PFFBXVIDLIKO 00V",
		"AL4 AAAAAAAAA 000",
		"AL5 x",
		"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ad, err := Parse(s)
		if err != nil {
			return
		}
		text, err := ad.Canonical()
		if err != nil {
			t.Fatalf("accepted %q but Canonical failed: %v", s, err)
		}
		if text == "" || text == invalidAdvertisementText {
			t.Fatalf("accepted %q but canonical text is %q", s, text)
		}
		again, err := Parse(text)
		if err != nil {
			t.Fatalf("accepted %q, canonical %q rejected: %v", s, text, err)
		}
		if again != ad {
			t.Fatalf("round trip changed %q: %+v -> %q -> %+v", s, ad, text, again)
		}
	})
}

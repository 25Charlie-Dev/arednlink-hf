package protocol

import (
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseCanonical(t *testing.T) {
	cases := []struct{ payload, canonical string }{
		{"AL1 198.51.100.27:49173 90", "AL1 198.51.100.27:49173 90"},
		{"AL1 [2001:0DB8::1]:49173 001", "AL1 [2001:db8::1]:49173 1"},
		{"AL1 10.1.2.3:49173 180", "AL1 10.1.2.3:49173 180"},
		{"AL1 [fd00::1]:49173 1", "AL1 [fd00::1]:49173 1"},
	}
	for _, tc := range cases {
		t.Run(tc.payload, func(t *testing.T) {
			ad, err := Parse(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			if ad.String() != tc.canonical {
				t.Fatalf("canonical = %q, want %q", ad.String(), tc.canonical)
			}
			if ad.ValidFor < time.Minute || ad.ValidFor > 180*time.Minute {
				t.Fatalf("validity = %s", ad.ValidFor)
			}
		})
	}
	if got := len("AL1 198.51.100.27:49173 90"); got != 26 {
		t.Fatalf("fixed fixture byte length = %d", got)
	}
}

func TestPayloadBoundary(t *testing.T) {
	base := "AL1 [2001:db8:ffff:ffff:ffff:ffff:ffff:ffff]:65535 "
	payload := base + strings.Repeat("0", MaxPayloadBytes-len(base)-1) + "1"
	if len(payload) != MaxPayloadBytes {
		t.Fatal("incorrect boundary fixture")
	}
	if _, err := Parse(payload); err != nil {
		t.Fatalf("67-byte payload: %v", err)
	}
	if _, err := Parse(payload + "0"); !errors.Is(err, ErrInvalidAdvertisement) {
		t.Fatalf("68-byte payload error = %v", err)
	}
}

func TestMalformedAdvertisements(t *testing.T) {
	cases := []string{
		"", "AL2 198.51.100.27:49173 90", "AL1 198.51.100.27:49173",
		" AL1 198.51.100.27:49173 90", "AL1 198.51.100.27:49173 90 ",
		"AL1  198.51.100.27:49173 90", "AL1\t198.51.100.27:49173 90",
		"AL1 198.51.100.27:49173 0", "AL1 198.51.100.27:49173 181",
		"AL1 198.51.100.27:49173 -1", "AL1 198.51.100.27:49173 +1",
		"AL1 198.51.100.27:49173 1.5", "AL1 198.51.100.27:49173 99999999",
		"AL1 198.51.100.27:49173 90\n", "AL1 198.51.100.27:49173 9\x00",
		"AL1 198.51.100.27:49173 ９", "AL1 198.51.100.27:49173 1\xff",
		"AL1 tcp://198.51.100.27:49173 90", "AL1 host.example:49173 90",
		"AL1 198.51.100.27:0 90", "AL1 198.51.100.27:65536 90",
		"AL1 198.51.100.27 90", "AL1 2001:db8::1:49173 90",
		"AL1 [::ffff:198.51.100.27]:49173 90", "AL1 [fe80::1%eth0]:49173 90",
		"AL1 0.0.0.0:49173 90", "AL1 [::]:49173 90",
		"AL1 127.0.0.1:49173 90", "AL1 [::1]:49173 90",
		"AL1 169.254.1.1:49173 90", "AL1 [fe80::1]:49173 90",
		"AL1 224.0.0.1:49173 90", "AL1 [ff02::1]:49173 90",
		"AL1 255.255.255.255:49173 90", "AL1 198.51.100.27:49173 90 N0CALL",
	}
	for _, payload := range cases {
		t.Run(payload, func(t *testing.T) {
			if _, err := Parse(payload); !errors.Is(err, ErrInvalidAdvertisement) {
				t.Fatalf("error = %v, want invalid advertisement", err)
			}
		})
	}
}

func TestAdvertisementPrefixes(t *testing.T) {
	for _, test := range []struct {
		payload string
		want    bool
	}{{"AL1 ", true}, {"AL2 ", true}, {"AL3 ", true}, {"AL3", false}, {"AL3X ", false}, {"al3 ", false}} {
		if got := HasAdvertisementPrefix(test.payload); got != test.want {
			t.Errorf("HasAdvertisementPrefix(%q) = %v, want %v", test.payload, got, test.want)
		}
	}
}

func TestCompactGoldensAndRoundTrips(t *testing.T) {
	cases := []struct{ plain, compact string }{
		{"AL1 198.51.100.27:49173 90", "AL2 PFFBXVIDLIKO"},
		{"AL1 [2001:db8::1]:49173 90", "AL2 AJTSGXIYMFLMVZANOYEVIDNKRFDPUJLYW"},
		{"AL1 1.1.1.1:1 1", "AL2 ACABELVPBYLL"},
		{"AL1 198.51.100.27:65535 180", ""},
		{"AL1 [2001:db8::1]:65535 180", ""},
	}
	for _, tc := range cases {
		ad, err := Parse(tc.plain)
		if err != nil {
			t.Fatal(err)
		}
		got, err := EncodeCompact(ad)
		if err != nil {
			t.Fatal(err)
		}
		if tc.compact != "" && got != tc.compact {
			t.Errorf("encoded %q = %q, want %q", tc.plain, got, tc.compact)
		}
		if len(got) != 16 && len(got) != 37 {
			t.Errorf("encoded byte length = %d", len(got))
		}
		decoded, err := Parse(got)
		if err != nil || decoded.Endpoint != ad.Endpoint || decoded.ValidFor != ad.ValidFor {
			t.Errorf("round trip = %#v, %v", decoded, err)
		}
	}
}

func TestCompactRejectsMalformedAndOutOfRangeValues(t *testing.T) {
	badTTL := make([]byte, 7)
	copy(badTTL, []byte{198, 51, 100, 27, 0xc0, 0x15, 0})
	badTTLString, _ := encodeBase26(badTTL, 12)
	tooLongTTL := append([]byte(nil), badTTL...)
	tooLongTTL[6] = 181
	tooLongTTLString, _ := encodeBase26(tooLongTTL, 12)
	loopback := []byte{127, 0, 0, 1, 0, 80, 1}
	loopbackString, _ := encodeBase26(loopback, 12)
	mapped := make([]byte, 19)
	copy(mapped[10:], []byte{0xff, 0xff, 198, 51, 100, 27, 0, 80, 1})
	mappedString, _ := encodeBase26(mapped, 33)
	cases := []string{
		"AL2 ZZZZZZZZZZZZ", "AL2 pffbxvidliko", "AL2 PFFBXVIDLIK", "AL2 PFFBXVIDLIKOO",
		"AL2 " + strings.Repeat("A", 32), "AL2 " + strings.Repeat("A", 33),
		"AL2 AAAAAAAAAAAA", // port and TTL are zero
		"AL2 " + strings.Repeat("Z", 12), "AL2 " + strings.Repeat("Z", 33),
		"AL2 " + badTTLString, "AL2 " + tooLongTTLString, "AL2 " + loopbackString, "AL2 " + mappedString,
	}
	for _, payload := range cases {
		if _, err := Parse(payload); !errors.Is(err, ErrInvalidAdvertisement) {
			t.Errorf("Parse(%q) error = %v", payload, err)
		}
	}
}

func TestCompactEncoderRejectsInvalidAdvertisements(t *testing.T) {
	valid, _ := Parse("AL1 198.51.100.27:49173 90")
	for _, ad := range []Advertisement{
		{Endpoint: valid.Endpoint, ValidFor: 90*time.Minute + time.Second},
		{Endpoint: valid.Endpoint, ValidFor: 0},
		{Endpoint: netip.AddrPort{}, ValidFor: time.Minute},
		{Endpoint: netip.MustParseAddrPort("127.0.0.1:80"), ValidFor: time.Minute},
		{Endpoint: netip.MustParseAddrPort("[::ffff:198.51.100.27]:80"), ValidFor: time.Minute},
	} {
		if _, err := EncodeCompact(ad); !errors.Is(err, ErrInvalidAdvertisement) {
			t.Errorf("EncodeCompact(%#v) error = %v", ad, err)
		}
	}
}

func TestAL3ServiceFlagsAssignedCombinationsAndFormatting(t *testing.T) {
	base, err := Parse("AL1 198.51.100.27:49173 90")
	if err != nil {
		t.Fatal(err)
	}
	for mask := uint16(0); mask < 32; mask++ {
		ad := base
		ad.Services, ad.ServicesPresent = mask, true
		wire, err := EncodeCompact(ad)
		if err != nil {
			t.Fatalf("mask %d encode: %v", mask, err)
		}
		if len(wire) != 20 {
			t.Fatalf("mask %d AL3 length = %d", mask, len(wire))
		}
		parsed, err := Parse(wire)
		if err != nil || parsed.Services != mask || !parsed.ServicesPresent || parsed.Endpoint != base.Endpoint || parsed.ValidFor != base.ValidFor {
			t.Fatalf("mask %d round trip = %#v, %v", mask, parsed, err)
		}
		if parsed.String() != wire {
			t.Fatalf("AL3 String() = %q, want %q", parsed.String(), wire)
		}
	}
	for _, tc := range []struct {
		mask uint16
		want string
	}{{0, "000"}, {31, "00V"}, {MaxServiceMask, "PA7"}} {
		ad := base
		ad.Services, ad.ServicesPresent = tc.mask, true
		wire, err := EncodeCompact(ad)
		if err != nil || wire[len(wire)-3:] != tc.want {
			t.Fatalf("mask %d flags = %q, %v; want %q", tc.mask, wire, err, tc.want)
		}
	}
	if got, want := ServiceNames(0x001f), []string{"signal_proxy", "briar_contact", "simplex_smp", "yggdrasil_connectivity", "matrix_federation"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("assigned service names = %v, want %v", got, want)
	}
	if got := UnassignedServiceBits(0x7fff); got != 0x7fe0 {
		t.Fatalf("unassigned bits = %04x, want 7fe0", got)
	}
	// Service metadata is never inferred from a legacy AL1/AL2 record.
	compact, err := EncodeCompact(base)
	if err != nil || !strings.HasPrefix(compact, "AL2 ") {
		t.Fatalf("legacy advertisement encoded as %q, %v", compact, err)
	}
}

func TestAL3IPv6AndRejectsInvalidServiceCodes(t *testing.T) {
	base, err := Parse("AL1 [2001:db8::1]:49173 90")
	if err != nil {
		t.Fatal(err)
	}
	base.Services, base.ServicesPresent = 0x4001, true
	wire, err := EncodeCompact(base)
	if err != nil || len(wire) != 41 {
		t.Fatalf("IPv6 AL3 = %q, %v", wire, err)
	}
	parsed, err := Parse(wire)
	if err != nil || parsed.Services != 0x4001 || parsed.Endpoint != base.Endpoint || UnassignedServiceBits(parsed.Services) != 0x4000 {
		t.Fatalf("IPv6 AL3 round trip = %#v, %v", parsed, err)
	}
	base.Services, base.ServicesPresent = MaxServiceMask, true
	maximum, err := EncodeCompact(base)
	if err != nil {
		t.Fatal(err)
	}
	parsedMaximum, err := Parse(maximum)
	if err != nil || parsedMaximum.Services != MaxServiceMask || parsedMaximum.ServicesPresent != true {
		t.Fatalf("maximum service mask round trip = %#v, %v", parsedMaximum, err)
	}
	base.Services = MaxServiceMask + 1
	if _, err := EncodeCompact(base); !errors.Is(err, ErrInvalidAdvertisement) {
		t.Fatalf("encoder accepted overflow mask: %v", err)
	}
	legacy, _ := EncodeCompact(Advertisement{Endpoint: base.Endpoint, ValidFor: 90 * time.Minute})
	compactBody := strings.TrimPrefix(legacy, "AL2 ")
	for _, flags := range []string{"pa7", "PA", "PA70", "ZZZ", "PA8", "00/"} {
		bad := "AL3 " + compactBody + " " + flags
		if _, err := Parse(bad); !errors.Is(err, ErrInvalidAdvertisement) {
			t.Errorf("Parse(%q) error = %v", bad, err)
		}
	}
	for _, bad := range []string{
		"AL3 " + strings.Repeat("A", 12) + " 000",
		"AL3 " + strings.Repeat("A", 32) + " 000",
		"AL3 " + strings.Repeat("A", 33) + " 000",
		"AL3 " + compactBody + "000",
	} {
		if _, err := Parse(bad); !errors.Is(err, ErrInvalidAdvertisement) {
			t.Errorf("Parse(%q) error = %v", bad, err)
		}
	}
}

func TestCallsignClaims(t *testing.T) {
	for _, claim := range []string{"N0CALL", "W1ABC-15", "K1ABC/P", "A1", "A123456789012345"} {
		if !ValidCallsign(claim) {
			t.Errorf("rejected %q", claim)
		}
	}
	for _, claim := range []string{"", "NOCALL", "123", "n0call", "N0 CALL", "N0CALL\n", "N0CALL\x00", "N0CÁLL", "A1234567890123456", "N0.CALL"} {
		if ValidCallsign(claim) {
			t.Errorf("accepted %q", claim)
		}
	}
}

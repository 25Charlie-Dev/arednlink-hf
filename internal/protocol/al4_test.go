package protocol

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestAL4RoundTripAndLegacy(t *testing.T) {
	for _, tc := range []struct {
		ip     string
		length int
	}{{"198.51.100.27", 17}, {"2001:db8::1", 37}} {
		ad := Advertisement{Address: netip.MustParseAddr(tc.ip), ValidFor: 90 * time.Minute, Services: 0x4001}
		wire, err := EncodeCompact(ad)
		if err != nil || len(wire) != tc.length || !strings.HasPrefix(wire, "AL4 ") {
			t.Fatalf("wire = %q, %v", wire, err)
		}
		got, err := Parse(wire)
		if err != nil || got.Address != ad.Address || got.Endpoint.IsValid() || got.Services != ad.Services || !got.ServicesPresent || got.String() != wire || !HasAdvertisementPrefix(wire) {
			t.Fatalf("round trip = %+v, %v", got, err)
		}
	}
	legacy, err := Parse("AL1 198.51.100.27:49173 90")
	if err != nil {
		t.Fatal(err)
	}
	for _, services := range []bool{false, true} {
		legacy.ServicesPresent = services
		wire, err := EncodeCompact(legacy)
		if err != nil {
			t.Fatal(err)
		}
		want := "AL2 "
		if services {
			want = "AL3 "
		}
		if !strings.HasPrefix(wire, want) {
			t.Fatalf("legacy wire = %q", wire)
		}
	}
}

func TestAL4RejectsMalformedClaims(t *testing.T) {
	base := Advertisement{Address: netip.MustParseAddr("198.51.100.27"), ValidFor: time.Minute}
	wire, err := EncodeCompact(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{wire[:len(wire)-1], wire + "A", strings.Replace(wire, "AL4 ", "AL4 a", 1), wire[:len(wire)-3] + "ZZZ", "AL4 " + strings.Repeat("Z", 9) + " 000", "AL4 " + strings.Repeat("Z", 29) + " 000"} {
		if _, err := Parse(bad); !errors.Is(err, ErrInvalidAdvertisement) {
			t.Errorf("accepted %q: %v", bad, err)
		}
	}
	for _, ad := range []Advertisement{
		{Address: base.Address, Endpoint: netip.MustParseAddrPort("198.51.100.27:80"), ValidFor: time.Minute},
		{Address: base.Address, ValidFor: time.Second},
		{Address: base.Address, ValidFor: 181 * time.Minute},
		{Address: netip.MustParseAddr("::ffff:198.51.100.27"), ValidFor: time.Minute},
		{Address: netip.MustParseAddr("127.0.0.1"), ValidFor: time.Minute},
		{Address: base.Address, ValidFor: time.Minute, Services: MaxServiceMask + 1},
	} {
		if _, err := EncodeCompact(ad); !errors.Is(err, ErrInvalidAdvertisement) {
			t.Errorf("accepted %+v: %v", ad, err)
		}
	}
}

func TestServiceMask(t *testing.T) {
	mask, err := ServiceMask([]string{"matrix_federation", "signal_proxy"})
	if err != nil || mask != ServiceMatrixFederation|ServiceSignalProxy {
		t.Fatalf("mask = %d, %v", mask, err)
	}
	for _, names := range [][]string{{"signal_proxy", "signal_proxy"}, {"unknown"}, {""}} {
		if _, err := ServiceMask(names); !errors.Is(err, ErrInvalidAdvertisement) {
			t.Errorf("accepted %v: %v", names, err)
		}
	}
	if mask, err := ServiceMask(nil); err != nil || mask != 0 {
		t.Fatalf("empty mask = %d, %v", mask, err)
	}
}

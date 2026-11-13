// Package protocol validates compact radio endpoint advertisements.
package protocol

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxPayloadBytes is the application-message byte ceiling.
	MaxPayloadBytes = 67
	// MaxValidityMinutes is the provisional implementation validity bound.
	MaxValidityMinutes = 180
	// MaxServiceMask is the highest permitted 15-bit service bitmap value.
	MaxServiceMask = 1<<15 - 1
	// ServiceSignalProxy identifies optional Signal proxy capability (bit 0).
	ServiceSignalProxy = 1 << 0
	// ServiceBriarContact identifies Briar contact availability (bit 1).
	ServiceBriarContact = 1 << 1
	// ServiceSimpleXSMP identifies SimpleX SMP capability (bit 2).
	ServiceSimpleXSMP = 1 << 2
	// ServiceYggdrasilConnectivity identifies Yggdrasil connectivity (bit 3).
	ServiceYggdrasilConnectivity = 1 << 3
	// ServiceMatrixFederation identifies Matrix federation (bit 4).
	ServiceMatrixFederation = 1 << 4
)

// ErrInvalidAdvertisement identifies malformed or unsupported advertisements.
var ErrInvalidAdvertisement = errors.New("invalid advertisement")

// Advertisement carries unverified endpoint and optional service claims.
type Advertisement struct {
	Endpoint        netip.AddrPort
	Address         netip.Addr
	ValidFor        time.Duration
	Services        uint16
	ServicesPresent bool
}

// Parse accepts AL1 literal endpoints, AL2 packed endpoints, or AL3 packed
// endpoints with an explicit service capability bitmap.
// Private and documentation addresses require explicit core probe policy.
func Parse(payload string) (Advertisement, error) {
	if strings.HasPrefix(payload, "AL4 ") {
		return parseCompact(payload)
	}
	if strings.HasPrefix(payload, "AL3 ") {
		return parseCompact(payload)
	}
	if strings.HasPrefix(payload, "AL2 ") {
		return parseCompact(payload)
	}
	if len(payload) == 0 || len(payload) > MaxPayloadBytes || !printableASCII(payload) {
		return Advertisement{}, fmt.Errorf("%w: payload length or characters", ErrInvalidAdvertisement)
	}
	fields := strings.Split(payload, " ")
	if len(fields) != 3 || fields[0] != "AL1" || fields[1] == "" || !decimal(fields[2]) {
		return Advertisement{}, fmt.Errorf("%w: expected AL1 endpoint minutes", ErrInvalidAdvertisement)
	}
	endpoint, err := netip.ParseAddrPort(fields[1])
	if err != nil || !validEndpoint(endpoint) {
		return Advertisement{}, fmt.Errorf("%w: literal endpoint required", ErrInvalidAdvertisement)
	}
	minutes, err := strconv.ParseUint(fields[2], 10, 16)
	if err != nil || minutes < 1 || minutes > MaxValidityMinutes {
		return Advertisement{}, fmt.Errorf("%w: validity outside 1..180 minutes", ErrInvalidAdvertisement)
	}
	return Advertisement{Endpoint: endpoint, ValidFor: time.Duration(minutes) * time.Minute}, nil
}

// HasAdvertisementPrefix identifies payloads whose complete syntax is validated by Parse.
func HasAdvertisementPrefix(payload string) bool {
	return strings.HasPrefix(payload, "AL1 ") || strings.HasPrefix(payload, "AL2 ") || strings.HasPrefix(payload, "AL3 ") || strings.HasPrefix(payload, "AL4 ")
}

// Canonical emits canonical AL1 text unless explicit service metadata or an
// address requires a compact form, and reports an encoding failure as an error.
func (a Advertisement) Canonical() (string, error) {
	if a.Address.IsValid() || a.ServicesPresent || a.Services != 0 {
		return EncodeCompact(a)
	}
	return "AL1 " + a.Endpoint.String() + " " + strconv.FormatInt(int64(a.ValidFor/time.Minute), 10), nil
}

// invalidAdvertisementText is what String returns when Canonical fails, so a
// failure is never mistaken for an empty advertisement.
const invalidAdvertisementText = "invalid advertisement"

// String is for diagnostics; use Canonical where the error matters.
func (a Advertisement) String() string {
	text, err := a.Canonical()
	if err != nil {
		return invalidAdvertisementText
	}
	return text
}

// ServiceNames returns names only for currently assigned service bits.
func ServiceNames(mask uint16) []string {
	services := [...]struct {
		flag uint16
		name string
	}{{ServiceSignalProxy, "signal_proxy"}, {ServiceBriarContact, "briar_contact"},
		{ServiceSimpleXSMP, "simplex_smp"}, {ServiceYggdrasilConnectivity, "yggdrasil_connectivity"},
		{ServiceMatrixFederation, "matrix_federation"}}
	result := make([]string, 0, len(services))
	for _, service := range services {
		if mask&service.flag != 0 {
			result = append(result, service.name)
		}
	}
	return result
}

// ServiceMask maps assigned service names to their fixed wire bits.
func ServiceMask(names []string) (uint16, error) {
	var mask uint16
	for _, name := range names {
		var bit uint16
		switch name {
		case "signal_proxy":
			bit = ServiceSignalProxy
		case "briar_contact":
			bit = ServiceBriarContact
		case "simplex_smp":
			bit = ServiceSimpleXSMP
		case "yggdrasil_connectivity":
			bit = ServiceYggdrasilConnectivity
		case "matrix_federation":
			bit = ServiceMatrixFederation
		default:
			return 0, fmt.Errorf("%w: unknown service %q", ErrInvalidAdvertisement, name)
		}
		if mask&bit != 0 {
			return 0, fmt.Errorf("%w: duplicate service %q", ErrInvalidAdvertisement, name)
		}
		mask |= bit
	}
	return mask, nil
}

func validAddress(addr netip.Addr) bool {
	return addr.IsValid() && addr.Zone() == "" && !addr.Is4In6() &&
		addr.IsGlobalUnicast() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast()
}

// UnassignedServiceBits returns reserved bits without assigning them meaning.
func UnassignedServiceBits(mask uint16) uint16 { return mask &^ 0x001f }

// ValidCallsign checks a bounded station claim without authenticating ownership.
func ValidCallsign(claim string) bool {
	if len(claim) < 1 || len(claim) > 16 {
		return false
	}
	letter, digit := false, false
	for i := 0; i < len(claim); i++ {
		c := claim[i]
		switch {
		case c >= 'A' && c <= 'Z':
			letter = true
		case c >= '0' && c <= '9':
			digit = true
		case c == '/' || c == '-':
		default:
			return false
		}
	}
	return letter && digit
}

func validEndpoint(endpoint netip.AddrPort) bool {
	addr := endpoint.Addr()
	return endpoint.IsValid() && endpoint.Port() != 0 && addr.Zone() == "" &&
		!addr.Is4In6() && addr.IsGlobalUnicast() && !addr.IsLoopback() &&
		!addr.IsLinkLocalUnicast()
}

func printableASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func decimal(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

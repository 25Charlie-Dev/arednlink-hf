package protocol

import (
	"fmt"
	"net/netip"
	"strconv"
	"time"
)

// EncodeCompact returns AL2 for endpoint-only claims and AL3 when service metadata is explicit.
func EncodeCompact(a Advertisement) (string, error) {
	ipOnly := a.Address.IsValid()
	if (ipOnly && (a.Endpoint.IsValid() || !validAddress(a.Address))) || (!ipOnly && !validEndpoint(a.Endpoint)) || a.ValidFor < time.Minute || a.ValidFor > MaxValidityMinutes*time.Minute || a.ValidFor%time.Minute != 0 {
		return "", fmt.Errorf("%w: endpoint or whole-minute validity required", ErrInvalidAdvertisement)
	}
	if a.Services > MaxServiceMask || !ipOnly && !a.ServicesPresent && a.Services != 0 {
		return "", fmt.Errorf("%w: service mask outside supported range", ErrInvalidAdvertisement)
	}
	var raw []byte
	addr := a.Endpoint.Addr()
	if ipOnly {
		addr = a.Address
	}
	if addr.Is4() {
		ip := addr.As4()
		raw = append(raw, ip[:]...)
	} else {
		ip := addr.As16()
		raw = append(raw, ip[:]...)
	}
	if !ipOnly {
		raw = append(raw, byte(a.Endpoint.Port()>>8), byte(a.Endpoint.Port()))
	}
	raw = append(raw, byte(a.ValidFor/time.Minute))
	digits := 12
	if len(raw) == 19 {
		digits = 33
	}
	if ipOnly {
		digits = 9
		if len(raw) == 17 {
			digits = 29
		}
	}
	encoded, ok := encodeBase26(raw, digits)
	if !ok {
		return "", fmt.Errorf("%w: compact value overflow", ErrInvalidAdvertisement)
	}
	if !a.ServicesPresent && !ipOnly {
		return "AL2 " + encoded, nil
	}
	serviceDigits := strconv.FormatUint(uint64(a.Services), 36)
	for len(serviceDigits) < 3 {
		serviceDigits = "0" + serviceDigits
	}
	if ipOnly {
		return "AL4 " + encoded + " " + upperASCII(serviceDigits), nil
	}
	return "AL3 " + encoded + " " + upperASCII(serviceDigits), nil
}

func parseCompact(payload string) (Advertisement, error) {
	invalid := func(reason string) (Advertisement, error) {
		return Advertisement{}, fmt.Errorf("%w: %s", ErrInvalidAdvertisement, reason)
	}
	version := payload[:4]
	isAL3 := version == "AL3 "
	isAL4 := version == "AL4 "
	if version != "AL2 " && !isAL3 && !isAL4 {
		return invalid("compact version")
	}
	if (!isAL3 && !isAL4 && len(payload) != 16 && len(payload) != 37) || (isAL3 && len(payload) != 20 && len(payload) != 41) || (isAL4 && len(payload) != 17 && len(payload) != 37) {
		return invalid("compact payload length")
	}
	end := len(payload)
	var services uint16
	if isAL3 || isAL4 {
		flagPos := end - 3
		if payload[flagPos-1] != ' ' {
			return invalid("service flag separator")
		}
		for i := flagPos; i < end; i++ {
			c := payload[i]
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
				return invalid("service flag alphabet")
			}
		}
		parsed, err := strconv.ParseUint(payload[flagPos:], 36, 16)
		if err != nil || parsed > MaxServiceMask {
			return invalid("service mask outside supported range")
		}
		services = uint16(parsed)
		end = flagPos - 1
	}
	body := payload[4:end]
	width := 7
	if isAL4 {
		width = 5
		if len(body) == 29 {
			width = 17
		}
	} else if len(body) == 33 {
		width = 19
	}
	raw, ok := decodeBase26(body, width)
	if !ok {
		return invalid("compact alphabet or value overflow")
	}
	var addr netip.Addr
	portOffset := 4
	if width == 7 || width == 5 {
		var ip [4]byte
		copy(ip[:], raw[:4])
		addr = netip.AddrFrom4(ip)
	} else {
		var ip [16]byte
		copy(ip[:], raw[:16])
		addr = netip.AddrFrom16(ip)
		portOffset = 16
	}
	minutes := raw[width-1]
	if isAL4 {
		if !validAddress(addr) || minutes < 1 || minutes > MaxValidityMinutes {
			return invalid("address or validity outside supported range")
		}
		return Advertisement{Address: addr, ValidFor: time.Duration(minutes) * time.Minute, Services: services, ServicesPresent: true}, nil
	}
	port := uint16(raw[portOffset])<<8 | uint16(raw[portOffset+1])
	endpoint := netip.AddrPortFrom(addr, port)
	if !validEndpoint(endpoint) || minutes < 1 || minutes > MaxValidityMinutes {
		return invalid("endpoint or validity outside supported range")
	}
	return Advertisement{Endpoint: endpoint, ValidFor: time.Duration(minutes) * time.Minute,
		Services: services, ServicesPresent: isAL3}, nil
}

func upperASCII(value string) string {
	bytes := []byte(value)
	for i, c := range bytes {
		if c >= 'a' && c <= 'z' {
			bytes[i] = c - ('a' - 'A')
		}
	}
	return string(bytes)
}

func encodeBase26(raw []byte, digits int) (string, bool) {
	result := make([]byte, digits)
	for i := range result {
		result[i] = 'A'
	}
	value := append([]byte(nil), raw...)
	for pos := digits - 1; pos >= 0; pos-- {
		var remainder uint16
		for i := range value {
			current := remainder*256 + uint16(value[i])
			value[i] = byte(current / 26)
			remainder = current % 26
		}
		result[pos] += byte(remainder)
	}
	for _, b := range value {
		if b != 0 {
			return "", false
		}
	}
	return string(result), true
}

func decodeBase26(digits string, width int) ([]byte, bool) {
	value := make([]byte, width)
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if c < 'A' || c > 'Z' {
			return nil, false
		}
		carry := uint16(c - 'A')
		for j := len(value) - 1; j >= 0; j-- {
			current := uint16(value[j])*26 + carry
			value[j] = byte(current)
			carry = current >> 8
		}
		if carry != 0 {
			return nil, false
		}
	}
	return value, true
}

//go:build kiss

package radio

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"arednlink-hf/internal/protocol"
)

const maxKISSFrame = 5000
const fend byte = 0xc0
const fesc byte = 0xdb

func kissAvailable() bool { return true }
func newKISS(cfg Config) (decoder, error) {
	if cfg.Callsign != "" && !validAX25Call(cfg.Callsign) {
		return nil, &ConfigError{Field: "callsign"}
	}
	destination := cfg.Destination
	if destination == "" {
		destination = "AREDNH"
	}
	if len(destination) > 6 || len(destination) == 0 {
		return nil, &ConfigError{Field: "destination"}
	}
	for i := 0; i < len(destination); i++ {
		if destination[i] < 'A' || destination[i] > 'Z' {
			return nil, &ConfigError{Field: "destination"}
		}
	}
	return &kissDecoder{destination: destination, callsign: cfg.Callsign, frame: make([]byte, 0, maxKISSFrame)}, nil
}

type kissTransmitter struct {
	callsign, destination string
}

func newKISSTransmitter(cfg Config) (transmitter, error) {
	if !validAX25Call(cfg.Callsign) || cfg.Destination == "" {
		return nil, &ConfigError{Field: "advert"}
	}
	if _, err := newKISS(cfg); err != nil {
		return nil, err
	}
	return kissTransmitter{callsign: cfg.Callsign, destination: cfg.Destination}, nil
}

func (t kissTransmitter) encode(payload string, _ uint64) ([]byte, error) {
	return encodeKISSBeacon(t.callsign, t.destination, payload)
}

type kissDecoder struct {
	destination, callsign      string
	frame                      []byte
	started, escaped, dropping bool
}

func (d *kissDecoder) feed(data []byte, emit func(Message)) uint64 {
	var rejected uint64
	for _, value := range data {
		rejected += d.accept(value, emit)
	}
	return rejected
}

func (d *kissDecoder) accept(value byte, emit func(Message)) uint64 {
	if value == fend {
		return d.boundary(emit)
	}
	if !d.started || d.dropping {
		return 0
	}
	if d.escaped {
		d.escaped = false
		switch value {
		case 0xdc:
			value = fend
		case 0xdd:
			value = fesc
		default:
			d.dropping = true
			return 1
		}
	} else if value == fesc {
		d.escaped = true
		return 0
	}
	if len(d.frame) >= maxKISSFrame {
		d.dropping = true
		return 1
	}
	d.frame = append(d.frame, value)
	return 0
}

func (d *kissDecoder) boundary(emit func(Message)) uint64 {
	var rejected uint64
	if d.started && !d.dropping && d.escaped {
		rejected = 1
	}
	if d.started && !d.dropping && !d.escaped && len(d.frame) > 0 {
		message, ok := ax25Message(d.frame, d.destination, d.callsign)
		if ok {
			emit(message)
		} else {
			rejected = 1
		}
	}
	d.frame = d.frame[:0]
	d.started, d.escaped, d.dropping = true, false, false
	return rejected
}

// ax25Message accepts command-zero KISS frames containing AX.25 UI/PID-F0 data.
// Native modem wrappers are not AX.25 and are refused.
func ax25Message(frame []byte, destination, callsign string) (Message, bool) {
	if len(frame) < 17 || frame[0]&0x0f != 0 {
		return Message{}, false
	}
	data := frame[1:]
	var source, target string
	offset, terminated := 0, false
	for count := 0; count < 10 && offset+7 <= len(data); count++ {
		address, end, ok := ax25Address(data[offset : offset+7])
		if !ok {
			return Message{}, false
		}
		if count == 0 {
			target = strings.SplitN(address, "-", 2)[0]
		}
		if count == 1 {
			source = address
		}
		offset += 7
		if end {
			terminated = count >= 1
			break
		}
	}
	if !terminated || offset+2 >= len(data) || data[offset] != 0x03 || data[offset+1] != 0xf0 {
		return Message{}, false
	}
	payload := data[offset+2:]
	if !utf8.Valid(payload) {
		return Message{}, false
	}
	body, ok := kissPayload(string(payload), target, destination, callsign)
	if !ok {
		return Message{}, false
	}
	return Message{Source: source, Transport: "kiss", Payload: body}, true
}

func encodeKISSBeacon(callsign, destination, payload string) ([]byte, error) {
	if !validAX25Call(callsign) || len(destination) == 0 || len(destination) > 6 || payload == "" || len(payload) > 67 {
		return nil, &ConfigError{Field: "advert"}
	}
	for i := 0; i < len(destination); i++ {
		if destination[i] < 'A' || destination[i] > 'Z' {
			return nil, &ConfigError{Field: "destination"}
		}
	}
	if _, err := protocol.Parse(payload); err != nil {
		return nil, err
	}
	frame := []byte{0}
	frame = append(frame, ax25AddressBytes(destination, 0, false)...)
	base, ssid, hasSSID := strings.Cut(callsign, "-")
	var value uint64
	if hasSSID {
		parsed, err := strconv.ParseUint(ssid, 10, 4)
		if err != nil {
			return nil, &ConfigError{Field: "callsign"}
		}
		value = parsed
	}
	frame = append(frame, ax25AddressBytes(base, byte(value), true)...)
	frame = append(frame, 0x03, 0xf0)
	frame = append(frame, payload...)
	return encodeKISSWire(frame), nil
}

func ax25AddressBytes(call string, ssid byte, last bool) []byte {
	address := make([]byte, 7)
	for i := range 6 {
		address[i] = ' ' << 1
		if i < len(call) {
			address[i] = call[i] << 1
		}
	}
	address[6] = 0x60 | ssid<<1
	if last {
		address[6] |= 1
	}
	return address
}

func encodeKISSWire(frame []byte) []byte {
	wire := make([]byte, 0, len(frame)+2)
	wire = append(wire, fend)
	for _, value := range frame {
		switch value {
		case fend:
			wire = append(wire, fesc, 0xdc)
		case fesc:
			wire = append(wire, fesc, 0xdd)
		default:
			wire = append(wire, value)
		}
	}
	return append(wire, fend)
}

func ax25Address(raw []byte) (string, bool, bool) {
	if len(raw) != 7 || raw[6]&0x60 != 0x60 {
		return "", false, false
	}
	var call [6]byte
	length, padded := 0, false
	for i := 0; i < 6; i++ {
		if raw[i]&1 != 0 {
			return "", false, false
		}
		c := raw[i] >> 1
		if c == ' ' {
			padded = true
			continue
		}
		if padded || !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return "", false, false
		}
		call[length] = c
		length++
	}
	if length == 0 {
		return "", false, false
	}
	result := string(call[:length])
	if ssid := (raw[6] >> 1) & 15; ssid > 0 {
		result += "-" + strconv.Itoa(int(ssid))
	}
	return result, raw[6]&1 != 0, true
}

func kissPayload(info, target, destination, callsign string) (string, bool) {
	if target == destination && protocol.HasAdvertisementPrefix(info) {
		return info, true
	}
	if !strings.HasPrefix(target, "AP") || len(info) < 11 || info[0] != ':' || info[10] != ':' {
		return "", false
	}
	recipient := strings.TrimRight(info[1:10], " ")
	if recipient != destination && (callsign == "" || recipient != callsign) {
		return "", false
	}
	body := info[11:]
	if pos := strings.IndexByte(body, '{'); pos >= 0 {
		id := body[pos+1:]
		if len(id) < 1 || len(id) > 5 {
			return "", false
		}
		for i := 0; i < len(id); i++ {
			if !(id[i] >= 'A' && id[i] <= 'Z' || id[i] >= 'a' && id[i] <= 'z' || id[i] >= '0' && id[i] <= '9') {
				return "", false
			}
		}
		body = body[:pos]
	}
	return body, len(body) <= 67 && protocol.HasAdvertisementPrefix(body)
}

func validAX25Call(call string) bool {
	base, ssid, hasSSID := strings.Cut(call, "-")
	if len(base) < 1 || len(base) > 6 {
		return false
	}
	for i := 0; i < len(base); i++ {
		if !(base[i] >= 'A' && base[i] <= 'Z' || base[i] >= '0' && base[i] <= '9') {
			return false
		}
	}
	if !hasSSID {
		return true
	}
	value, err := strconv.ParseUint(ssid, 10, 4)
	return err == nil && strconv.FormatUint(value, 10) == ssid
}

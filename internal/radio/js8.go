//go:build js8call

package radio

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"arednlink-hf/internal/jsoncheck"
	"arednlink-hf/internal/protocol"
)

const maxJS8Line = 8192
const endMarker = "\u2662"

func js8Available() bool { return true }

func newJS8Transmitter(cfg Config) (transmitter, error) {
	if _, err := newJS8(cfg); err != nil {
		return nil, err
	}
	if cfg.Destination == "" {
		return nil, &ConfigError{Field: "advert destination"}
	}
	return &js8Transmitter{destination: cfg.Destination}, nil
}

type js8Transmitter struct {
	destination string
}

func (t *js8Transmitter) encode(payload string, requestID uint64) ([]byte, error) {
	if _, err := protocol.Parse(payload); err != nil {
		return nil, err
	}
	request := struct {
		Type   string `json:"type"`
		Value  string `json:"value"`
		Params struct {
			ID       uint64 `json:"_ID"`
			Priority string `json:"PRIORITY"`
		} `json:"params"`
	}{Type: "TX.SEND_MESSAGE", Value: t.destination + " MSG " + payload}
	request.Params.ID = requestID
	request.Params.Priority = "HIGH"
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode JS8Call advert: %w", err)
	}
	return append(encoded, '\n'), nil
}

func newJS8(cfg Config) (decoder, error) {
	if !protocol.ValidCallsign(cfg.Callsign) {
		return nil, &ConfigError{Field: "callsign"}
	}
	if cfg.Destination != "" && !validGroup(cfg.Destination) {
		return nil, &ConfigError{Field: "destination"}
	}
	return &js8Decoder{callsign: cfg.Callsign, destination: cfg.Destination, onReceive: cfg.OnReceive,
		onTransmitRefused: cfg.OnTransmitRefused,
		line:              make([]byte, 0, maxJS8Line)}, nil
}
func validGroup(group string) bool {
	if len(group) < 2 || len(group) > 16 || group[0] != '@' {
		return false
	}
	for i := 1; i < len(group); i++ {
		if !(group[i] >= 'A' && group[i] <= 'Z' || group[i] >= '0' && group[i] <= '9') {
			return false
		}
	}
	return true
}

type js8Decoder struct {
	callsign, destination string
	line                  []byte
	dropping              bool
	onReceive             func(string)
	onTransmitRefused     func(uint64, string)
}

func (d *js8Decoder) feed(data []byte, emit func(Message)) uint64 {
	var rejected uint64
	for _, value := range data {
		rejected += d.accept(value, emit)
	}
	return rejected
}
func (d *js8Decoder) accept(value byte, emit func(Message)) uint64 {
	if value == '\n' {
		var rejected uint64
		if !d.dropping && len(d.line) > 0 {
			if message, ok := d.directed(d.line); ok {
				if message.Payload != "" {
					emit(message)
				}
			} else {
				rejected = 1
			}
		}
		d.line = d.line[:0]
		d.dropping = false
		return rejected
	}
	if d.dropping {
		return 0
	}
	if len(d.line) >= maxJS8Line {
		d.dropping = true
		return 1
	}
	d.line = append(d.line, value)
	return 0
}

type directedRecord struct {
	Type   string       `json:"type"`
	Value  optionalText `json:"value"`
	Params *js8Params   `json:"params"`
}

type js8Params struct {
	From    optionalText `json:"FROM"`
	To      optionalText `json:"TO"`
	Command optionalText `json:"CMD"`
	Extra   optionalText `json:"EXTRA"`
	Text    optionalText `json:"TEXT"`
}

type optionalText struct {
	value   string
	present bool
}

func (s *optionalText) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || data[0] != '"' {
		return errors.New("expected string")
	}
	if err := json.Unmarshal(data, &s.value); err != nil {
		return err
	}
	if !utf8.ValidString(s.value) || strings.ContainsRune(s.value, '\x00') {
		return errors.New("invalid text")
	}
	s.present = true
	return nil
}

func (d *js8Decoder) directed(line []byte) (Message, bool) {
	if len(line) > maxJS8Line || jsoncheck.Validate(line, 16) != nil {
		return Message{}, false
	}
	var record directedRecord
	if err := json.Unmarshal(line, &record); err != nil || record.Params == nil {
		return Message{}, false
	}
	if record.Type == "TX.SEND_MESSAGE" {
		// Fork-specific: this refusal reply shape comes from a reviewed JS8Call
		// fork and has not been confirmed against upstream JS8Call, so the
		// resulting api_refused state is reported as fork-specific in status.
		var result struct {
			Params struct {
				ID      uint64 `json:"_ID"`
				Refused bool   `json:"REFUSED"`
				Reason  string `json:"REASON"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &result); err != nil {
			return Message{}, false
		}
		if result.Params.Refused && d.onTransmitRefused != nil {
			d.onTransmitRefused(result.Params.ID, result.Params.Reason)
		}
		return Message{}, true
	}
	d.observeReceive(record)
	return d.advertisement(record)
}

func (d *js8Decoder) observeReceive(record directedRecord) {
	if d.onReceive == nil {
		return
	}
	var call string
	switch record.Type {
	case "RX.ACTIVITY":
		if strings.TrimSpace(record.Value.value) == "" {
			return
		}
		if prefix, _, found := strings.Cut(record.Value.value, ": "); found && protocol.ValidCallsign(prefix) {
			call = prefix
		}
	case "RX.DIRECTED":
		call = record.Params.From.value
		if !protocol.ValidCallsign(call) || !record.receiveText() {
			return
		}
	default:
		return
	}
	if call != d.callsign {
		d.onReceive(call)
	}
}

func (r directedRecord) receiveText() bool {
	if r.Value.present && r.Params.Text.present && r.Value.value != r.Params.Text.value {
		return false
	}
	return strings.TrimSpace(r.Value.value) != "" || strings.TrimSpace(r.Params.Text.value) != ""
}

func (d *js8Decoder) advertisement(record directedRecord) (Message, bool) {
	if record.Type != "RX.DIRECTED" {
		return Message{}, false
	}
	p := record.Params
	if !protocol.ValidCallsign(p.From.value) || p.To.value != d.callsign && (d.destination == "" || p.To.value != d.destination) {
		return Message{}, false
	}
	if !p.Command.present || !p.Extra.present || p.Extra.value != "" {
		return Message{}, false
	}
	if record.Value.present && record.Value.value != p.Text.value {
		return Message{}, false
	}
	prefix, ok := directedPrefix(p.From.value, p.To.value, p.Command.value)
	if !ok {
		return Message{}, false
	}
	payload, ok := completePayload(p.Text.value, prefix)
	if !ok {
		return Message{}, false
	}
	return Message{Source: p.From.value, Transport: "js8call", Payload: payload}, true
}

func directedPrefix(from, to, command string) (string, bool) {
	prefix := from + ": " + to
	if strings.Trim(command, " ") != "" {
		if strings.Trim(command, " ") != "MSG" || !strings.HasPrefix(command, " ") {
			return "", false
		}
		prefix += command
	}
	return prefix + " ", true
}

// completePayload accepts one full prefixed RX.DIRECTED record ending in the
// default EOM marker exactly once. Partial records and alternate formats fail.
func completePayload(text, prefix string) (string, bool) {
	text = strings.TrimRight(text, " ")
	if strings.Count(text, endMarker) != 1 || !strings.HasSuffix(text, " "+endMarker) || !strings.HasPrefix(text, prefix) {
		return "", false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(text, prefix), " "+endMarker)
	if len(body) > 67 || !protocol.HasAdvertisementPrefix(body) {
		return "", false
	}
	return body, true
}

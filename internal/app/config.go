package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"arednlink-hf/internal/core"
	"arednlink-hf/internal/jsoncheck"
	"arednlink-hf/internal/protocol"
	"arednlink-hf/internal/radio"
	"arednlink-hf/internal/resourceguard"
)

const maxConfigBytes = 64 << 10

// Config contains operator settings. Durations use Go's duration notation.
type Config struct {
	ResourceGuard       resourceguard.Config `json:"resource_guard"`
	ControlSocket       string               `json:"control_socket"`
	LogLevel            string               `json:"log_level"`
	MaxPeers            int                  `json:"max_peers"`
	PeerTable           PeerTable            `json:"peer_table"`
	EnableProbes        bool                 `json:"enable_probes"`
	ProbeTimeout        string               `json:"probe_timeout"`
	ProbeInterval       string               `json:"probe_interval"`
	ProbeGlobalInterval string               `json:"probe_global_interval"`
	ProbePrefixes       []string             `json:"probe_prefixes"`
	ProbePorts          []uint16             `json:"probe_ports"`
	Radio               RadioConfig          `json:"radio"`
	Radios              []RadioConfig        `json:"radios,omitempty"`
	// DeprecatedKeys lists accepted-and-ignored legacy keys found while decoding.
	DeprecatedKeys []string `json:"-"`
}

// PeerTable selects how the bounded peer store behaves when it is full.
// FullPolicy is "replace" (evict one entry: heard once before heard repeatedly,
// least recently heard first) or "refuse" (decline new callsigns).
type PeerTable struct {
	FullPolicy string `json:"full_policy"`
}

// deprecatedKeys are removed top-level settings that old files may still carry.
var deprecatedKeys = []string{"status_listen"}

// RadioConfig selects one adapter included in this build and optional beacon settings.
type RadioConfig struct {
	Kind        string       `json:"kind"`
	Endpoint    string       `json:"endpoint"`
	Callsign    string       `json:"callsign"`
	Destination string       `json:"destination"`
	AllowRemote bool         `json:"allow_remote"`
	Beacon      BeaconConfig `json:"advert"`
}

// BeaconConfig explicitly opts in to a periodic endpoint advertisement.
type BeaconConfig struct {
	Enabled         bool     `json:"enabled"`
	IP              string   `json:"ip"`
	Services        []string `json:"services"`
	Endpoint        string   `json:"endpoint"`
	ValidityMinutes uint16   `json:"validity_minutes"`
	Interval        string   `json:"interval"`
	Jitter          string   `json:"jitter"`
}

// Defaults disables radio reception, beacon transmission, and active probing.
func Defaults() Config {
	return Config{ControlSocket: "/tmp/run/arednlink-hf/control.sock", LogLevel: "info", MaxPeers: 64,
		PeerTable:    PeerTable{FullPolicy: core.FullPolicyReplace},
		ProbeTimeout: "3s", ProbeInterval: "5m", ProbeGlobalInterval: "10s",
		Radio: RadioConfig{Kind: "none"}, ResourceGuard: resourceguard.Defaults()}
}

// LoadConfig reads a bounded strict JSON configuration from an explicit path.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path) // The operator supplies a local configuration path.
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	cfg, decodeErr := DecodeConfig(f)
	closeErr := f.Close()
	if decodeErr != nil {
		return Config{}, decodeErr
	}
	if closeErr != nil {
		return Config{}, fmt.Errorf("close configuration: %w", closeErr)
	}
	return cfg, nil
}

// DecodeConfig rejects unknown keys, duplicate keys and oversized documents.
func DecodeConfig(r io.Reader) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > maxConfigBytes {
		return Config{}, errors.New("configuration exceeds 64 kib")
	}
	if err := jsoncheck.Validate(data, 8); err != nil {
		return Config{}, fmt.Errorf("configuration json: %w", err)
	}
	data, err = canonicalAdvertKeys(data)
	if err != nil {
		return Config{}, err
	}
	data, ignored, err := stripDeprecatedKeys(data)
	if err != nil {
		return Config{}, err
	}
	cfg := Defaults()
	cfg.DeprecatedKeys = ignored
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("configuration must contain one json object")
	}
	return cfg, cfg.Validate()
}

// Validate checks exposure and build capabilities before any listener starts.
func (c Config) Validate() error {
	if !filepath.IsAbs(c.ControlSocket) || filepath.Clean(c.ControlSocket) != c.ControlSocket || strings.ContainsRune(c.ControlSocket, 0) {
		return errors.New("control_socket must be a clean absolute path")
	}
	if err := c.ResourceGuard.Validate(); err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return errors.New("invalid log_level")
	}
	if c.MaxPeers < 1 || c.MaxPeers > 1024 {
		return errors.New("max_peers must be between 1 and 1024")
	}
	if _, err := c.coreConfig(); err != nil {
		return err
	}
	if len(c.Radios) > 0 {
		if len(c.Radios) > 2 {
			return errors.New("radios supports at most two adapters")
		}
		if c.Radio.Kind != "none" || c.Radio.Endpoint != "" || c.Radio.Callsign != "" || c.Radio.Destination != "" || c.Radio.AllowRemote || c.Radio.Beacon.hasSettings() {
			return errors.New("radio and radios cannot both be active")
		}
		seen := make(map[string]bool, 2)
		for _, adapter := range c.Radios {
			if adapter.Kind != "kiss" && adapter.Kind != "js8call" || seen[adapter.Kind] {
				return errors.New("radios require distinct kiss and js8call kinds")
			}
			seen[adapter.Kind] = true
			if err := adapter.validate(); err != nil {
				return err
			}
		}
		return nil
	}
	if c.Radio.Kind == "none" {
		if c.Radio.Endpoint != "" || c.Radio.Callsign != "" || c.Radio.Destination != "" || c.Radio.AllowRemote || c.Radio.Beacon.hasSettings() {
			return errors.New("radio settings require an enabled adapter")
		}
		return nil
	}
	return c.Radio.validate()
}

func (c RadioConfig) validate() error {
	for _, kind := range radio.Supported() {
		if c.Kind == kind {
			beacon, err := c.beaconConfig()
			if err != nil {
				return err
			}
			return radio.Validate(radio.Config{Kind: c.Kind, Endpoint: c.Endpoint,
				Callsign: c.Callsign, Destination: c.Destination, AllowRemote: c.AllowRemote,
				Beacon: beacon})
		}
	}
	return errors.New("radio adapter unavailable in this build")
}

// canonicalAdvertKeys accepts the historical spelling on each adapter only once.
func canonicalAdvertKeys(data []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	for _, field := range []string{"radio", "radios"} {
		raw, ok := root[field]
		if !ok {
			continue
		}
		if field == "radio" {
			normalized, err := canonicalAdapter(raw)
			if err != nil {
				return nil, err
			}
			root[field] = normalized
		} else {
			var entries []json.RawMessage
			if err := json.Unmarshal(raw, &entries); err != nil {
				return nil, err
			}
			for i, entry := range entries {
				normalized, err := canonicalAdapter(entry)
				if err != nil {
					return nil, err
				}
				entries[i] = normalized
			}
			marshaled, err := json.Marshal(entries)
			if err != nil {
				return nil, err
			}
			root[field] = marshaled
		}
	}
	return json.Marshal(root)
}

func canonicalAdapter(raw json.RawMessage) (json.RawMessage, error) {
	var adapter map[string]json.RawMessage
	if err := json.Unmarshal(raw, &adapter); err != nil {
		return nil, err
	}
	var legacy json.RawMessage
	var legacyKey string
	hasAdvert := false
	for key, value := range adapter {
		if strings.EqualFold(key, "beacon") {
			legacy, legacyKey = value, key
		}
		if strings.EqualFold(key, "advert") {
			hasAdvert = true
		}
	}
	if legacyKey != "" && hasAdvert {
		return nil, errors.New("advert and beacon cannot both be set on one adapter")
	}
	if legacyKey != "" {
		adapter["advert"] = legacy
		delete(adapter, legacyKey)
	}
	return json.Marshal(adapter)
}

func (c RadioConfig) beaconConfig() (radio.BeaconConfig, error) {
	b := c.Beacon
	if !b.hasSettings() {
		return radio.BeaconConfig{}, nil
	}
	if b.IP != "" && b.Endpoint != "" {
		return radio.BeaconConfig{}, errors.New("radio advert cannot mix ip and endpoint")
	}
	if b.IP == "" && b.Services != nil {
		return radio.BeaconConfig{}, errors.New("radio advert services require ip")
	}
	if b.Enabled && b.IP == "" && b.Endpoint == "" {
		return radio.BeaconConfig{}, errors.New("radio advert requires ip or endpoint")
	}
	var address netip.Addr
	var endpoint netip.AddrPort
	var err error
	if b.IP != "" {
		address, err = netip.ParseAddr(b.IP)
		if err != nil || address.Zone() != "" || address.Is4In6() || !address.IsGlobalUnicast() {
			return radio.BeaconConfig{}, errors.New("invalid radio advert ip")
		}
	} else if b.Endpoint != "" {
		endpoint, err = netip.ParseAddrPort(b.Endpoint)
		if err != nil {
			return radio.BeaconConfig{}, errors.New("invalid radio advert endpoint")
		}
	}
	services, err := protocol.ServiceMask(b.Services)
	if err != nil {
		return radio.BeaconConfig{}, fmt.Errorf("invalid radio advert services: %w", err)
	}
	var interval time.Duration
	if b.Interval != "" {
		interval, err = time.ParseDuration(b.Interval)
	}
	if err != nil || b.Interval != "" && interval != 30*time.Minute && interval != 60*time.Minute || b.Enabled && b.Interval == "" {
		return radio.BeaconConfig{}, errors.New("radio advert interval must be 30m or 60m")
	}
	var jitter time.Duration
	if b.Jitter != "" {
		jitter, err = time.ParseDuration(b.Jitter)
	}
	if err != nil || b.Jitter != "" && (jitter <= 0 || jitter > 5*time.Minute) || b.Enabled && b.Jitter == "" {
		return radio.BeaconConfig{}, errors.New("radio advert jitter must be positive and at most 5m")
	}
	if b.ValidityMinutes > 180 || b.Enabled && b.ValidityMinutes == 0 {
		return radio.BeaconConfig{}, errors.New("radio advert validity must be 1..180 minutes")
	}
	if address.IsValid() || endpoint.IsValid() {
		validationMinutes := b.ValidityMinutes
		if validationMinutes == 0 {
			validationMinutes = 1
		}
		if _, err := protocol.EncodeCompact(protocol.Advertisement{Endpoint: endpoint, Address: address, Services: services, ServicesPresent: address.IsValid(),
			ValidFor: time.Duration(validationMinutes) * time.Minute}); err != nil {
			return radio.BeaconConfig{}, errors.New("invalid radio advert address or endpoint")
		}
	}
	return radio.BeaconConfig{Enabled: b.Enabled, Endpoint: endpoint, Address: address, Services: services, ValidityMinutes: uint8(b.ValidityMinutes), Interval: interval, Jitter: jitter}, nil
}

func (b BeaconConfig) hasSettings() bool {
	return b.Enabled || b.IP != "" || b.Services != nil || b.Endpoint != "" || b.ValidityMinutes != 0 || b.Interval != "" || b.Jitter != ""
}

// stripDeprecatedKeys removes accepted-and-ignored legacy top-level keys so the
// strict decoder does not reject old files, and returns the keys it removed.
func stripDeprecatedKeys(data []byte) ([]byte, []string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, err
	}
	var found []string
	for key := range root {
		for _, legacy := range deprecatedKeys {
			if strings.EqualFold(key, legacy) {
				found = append(found, legacy)
				delete(root, key)
			}
		}
	}
	sort.Strings(found)
	out, err := json.Marshal(root)
	return out, found, err
}

func (c Config) coreConfig() (core.Config, error) {
	if c.PeerTable.FullPolicy != core.FullPolicyReplace && c.PeerTable.FullPolicy != core.FullPolicyRefuse {
		return core.Config{}, errors.New(`peer_table.full_policy must be "replace" or "refuse"`)
	}
	result := core.Config{MaxPeers: c.MaxPeers, FullPolicy: c.PeerTable.FullPolicy, EnableProbes: c.EnableProbes,
		ProbePorts: append([]uint16(nil), c.ProbePorts...)}
	var err error
	for _, duration := range []struct {
		text   string
		target *time.Duration
	}{{c.ProbeTimeout, &result.ProbeTimeout}, {c.ProbeInterval, &result.ProbeInterval},
		{c.ProbeGlobalInterval, &result.ProbeGlobalInterval}} {
		*duration.target, err = time.ParseDuration(duration.text)
		if err != nil || *duration.target <= 0 {
			return core.Config{}, errors.New("probe durations must be positive duration strings")
		}
	}
	for _, text := range c.ProbePrefixes {
		prefix, err := netip.ParsePrefix(text)
		if err != nil || prefix != prefix.Masked() || strings.Contains(text, "%") {
			return core.Config{}, errors.New("invalid or noncanonical probe_prefixes entry")
		}
		result.ProbePrefixes = append(result.ProbePrefixes, prefix)
	}
	return result, core.ValidateConfig(result)
}

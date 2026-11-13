package app

import (
	"arednlink-hf/internal/radio"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPreparedAdvertCanonicalRoundTrip(t *testing.T) {
	supported := radio.Supported()
	if len(supported) == 0 {
		t.Skip("radio adapter omitted by build tags")
	}
	kind := supported[0]
	adapter := fmt.Sprintf(`{"kind":%q,"endpoint":"127.0.0.1:8001","callsign":"N0CALL","destination":"AREDNH","advert":{"enabled":false,"ip":"198.51.100.27","services":["signal_proxy","simplex_smp"],"validity_minutes":90,"interval":"30m","jitter":"1m"}}`, kind)
	if kind == "js8call" {
		adapter = strings.Replace(adapter, `"destination":"AREDNH"`, `"destination":"@AREDN"`, 1)
	}
	cfg, err := DecodeConfig(strings.NewReader(`{"radios":[` + adapter + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Radios[0].Beacon.Enabled {
		t.Fatal("prepared advert was enabled")
	}
	canonical, err := json.Marshal(cfg)
	if err != nil || !strings.Contains(string(canonical), `"ip":"198.51.100.27"`) || strings.Contains(string(canonical), `"beacon"`) {
		t.Fatalf("bad canonical form: %s, %v", canonical, err)
	}
	again, err := DecodeConfig(strings.NewReader(string(canonical)))
	if err != nil || again.Radios[0].Beacon.Services[1] != "simplex_smp" || again.Radios[0].Beacon.Enabled {
		t.Fatalf("round trip lost prepared advert: %+v, %v", again.Radios, err)
	}
	legacy := strings.ReplaceAll(string(canonical), `"advert":`, `"beacon":`)
	fromLegacy, err := DecodeConfig(strings.NewReader(legacy))
	if err != nil || fromLegacy.Radios[0].Beacon.IP != "198.51.100.27" {
		t.Fatalf("legacy alias failed: %+v, %v", fromLegacy.Radios, err)
	}
}

func TestAdvertAliasesAndDualAdapterValidation(t *testing.T) {
	inputs := []string{`{"radio":{"kind":"none","beacon":{"enabled":false}}}`}
	if len(radio.Supported()) == 2 {
		inputs = append(inputs, `{"radios":[{"kind":"kiss","endpoint":"127.0.0.1:8001","callsign":"N0CALL","destination":"AREDNH","advert":{"enabled":false}},{"kind":"js8call","endpoint":"127.0.0.1:8002","callsign":"N0CALL","destination":"@AREDN","beacon":{"enabled":false}}]}`)
	}
	for _, input := range inputs {
		cfg, err := DecodeConfig(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := json.Marshal(cfg)
		if err != nil || strings.Contains(string(canonical), `"beacon"`) {
			t.Fatalf("noncanonical config: %s, %v", canonical, err)
		}
	}
	for _, input := range []string{
		`{"radio":{"kind":"kiss","endpoint":"127.0.0.1:8001","callsign":"N0CALL","destination":"AREDNH"},"radios":[{"kind":"js8call","endpoint":"127.0.0.1:8002","callsign":"N0CALL","destination":"@AREDN"}]}`,
		`{"radios":[{"kind":"kiss","endpoint":"127.0.0.1:8001","callsign":"N0CALL","destination":"AREDNH"},{"kind":"kiss","endpoint":"127.0.0.1:8002","callsign":"N0CALL","destination":"AREDNH"}]}`,
		`{"radios":[{"kind":"kiss","endpoint":"127.0.0.1:8001","callsign":"N0CALL","destination":"AREDNH","advert":{},"beacon":{}}]}`,
		`{"radios":[{"kind":"kiss","endpoint":"127.0.0.1:8001","callsign":"N0CALL","destination":"AREDNH","ADVERT":{},"BEACON":{}}]}`,
		`{"radio":{"kind":"none","advert":{},"beacon":{}}}`,
		`{"radio":{"kind":"none","beacon":{"enabled":false,"unknown":true}}}`,
		`{"radios":[{"kind":"kiss","endpoint":"127.0.0.1:8001","callsign":"N0CALL","destination":"AREDNH","beacon":{"enabled":false,"enabled":true}}]}`,
		`{"radios":[{"kind":"kiss","endpoint":"127.0.0.1:8001","callsign":"N0CALL","destination":"AREDNH"},{"kind":"js8call","endpoint":"127.0.0.1:8002","callsign":"N0CALL","destination":"@AREDN"},{"kind":"kiss","endpoint":"127.0.0.1:8003","callsign":"N0CALL","destination":"AREDNH"}]}`,
	} {
		if _, err := DecodeConfig(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestDecodeConfigSafeDefaults(t *testing.T) {
	cfg, err := DecodeConfig(strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlSocket != "/tmp/run/arednlink-hf/control.sock" || cfg.ResourceGuard.Enabled || cfg.EnableProbes || cfg.Radio.Kind != "none" || cfg.Radio.Beacon.Enabled || cfg.MaxPeers != 64 || cfg.PeerTable.FullPolicy != "replace" {
		t.Fatalf("unsafe defaults: %+v", cfg)
	}
	engine, err := cfg.coreConfig()
	if err != nil || engine.ProbeGlobalInterval != 10*time.Second {
		t.Fatalf("global budget default: %+v, %v", engine, err)
	}
}

func TestBeaconConfigRequiresExplicitBounds(t *testing.T) {
	if _, err := (RadioConfig{}).beaconConfig(); err != nil {
		t.Fatalf("disabled default rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		cfg  BeaconConfig
	}{
		{"missing endpoint", BeaconConfig{Enabled: true, ValidityMinutes: 90, Interval: "30m", Jitter: "1m"}},
		{"invalid endpoint", BeaconConfig{Enabled: true, Endpoint: "example.org:1234", ValidityMinutes: 90, Interval: "30m", Jitter: "1m"}},
		{"private endpoint", BeaconConfig{Enabled: true, Endpoint: "127.0.0.1:1234", ValidityMinutes: 90, Interval: "30m", Jitter: "1m"}},
		{"ttl", BeaconConfig{Enabled: true, Endpoint: "198.51.100.27:49173", ValidityMinutes: 181, Interval: "30m", Jitter: "1m"}},
		{"interval", BeaconConfig{Enabled: true, Endpoint: "198.51.100.27:49173", ValidityMinutes: 90, Interval: "5m", Jitter: "1m"}},
		{"jitter", BeaconConfig{Enabled: true, Endpoint: "198.51.100.27:49173", ValidityMinutes: 90, Interval: "30m", Jitter: "6m"}},
		{"mixed ip endpoint", BeaconConfig{IP: "198.51.100.27", Endpoint: "198.51.100.27:49173"}},
		{"unknown service while disabled", BeaconConfig{IP: "198.51.100.27", Services: []string{"unknown"}}},
		{"duplicate service while disabled", BeaconConfig{IP: "198.51.100.27", Services: []string{"signal_proxy", "signal_proxy"}}},
		{"bad ip while disabled", BeaconConfig{IP: "example.org"}},
		{"service without ip", BeaconConfig{Services: []string{"signal_proxy"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := (RadioConfig{Beacon: tc.cfg}).beaconConfig(); err == nil {
				t.Fatal("accepted invalid beacon configuration")
			}
		})
	}
	configured := RadioConfig{Beacon: BeaconConfig{Enabled: true, Endpoint: "198.51.100.27:49173",
		ValidityMinutes: 90, Interval: "30m", Jitter: "5m"}}
	got, err := configured.beaconConfig()
	if err != nil || got.Interval != 30*time.Minute || got.Jitter != 5*time.Minute || got.ValidityMinutes != 90 {
		t.Fatalf("valid config = %+v, %v", got, err)
	}
	prepared := RadioConfig{Beacon: BeaconConfig{IP: "198.51.100.27", Services: []string{"signal_proxy", "simplex_smp"}, ValidityMinutes: 90, Interval: "30m", Jitter: "5m"}}
	parsed, err := prepared.beaconConfig()
	if err != nil || parsed.Enabled || parsed.Address.String() != "198.51.100.27" || parsed.Services != 5 {
		t.Fatalf("prepared advert invalid: %+v, %v", parsed, err)
	}
}

func TestDecodeConfigRejectsAmbiguousOrUnsafeInput(t *testing.T) {
	cases := map[string]string{
		"unknown":                   `{"typo":true}`,
		"duplicate":                 `{"enable_probes":false,"enable_probes":true}`,
		"nested duplicate":          `{"radio":{"kind":"none","kind":"none"}}`,
		"escaped duplicate":         `{"max_peers":64,"max_\u0070eers":32}`,
		"case alias duplicate":      `{"enable_probes":false,"ENABLE_PROBES":true}`,
		"trailing object":           `{} {}`,
		"null":                      `null`,
		"array":                     `[]`,
		"too large":                 `{"log_level":"` + strings.Repeat("x", maxConfigBytes) + `"}`,
		"enabled without policy":    `{"enable_probes":true}`,
		"only prefix":               `{"enable_probes":true,"probe_prefixes":["200::/7"]}`,
		"only port":                 `{"enable_probes":true,"probe_ports":[12345]}`,
		"zero port":                 `{"probe_ports":[0]}`,
		"bad prefix":                `{"probe_prefixes":["not-an-address"]}`,
		"host bits":                 `{"probe_prefixes":["200::1/7"]}`,
		"fast budget":               `{"probe_global_interval":"1ms"}`,
		"zero timeout":              `{"probe_timeout":"0s"}`,
		"unavailable adapter":       `{"radio":{"kind":"unavailable"}}`,
		"disabled adapter endpoint": `{"radio":{"kind":"none","endpoint":"127.0.0.1:8001"}}`,
		"disabled beacon endpoint":  `{"radio":{"kind":"none","beacon":{"enabled":false,"endpoint":"198.51.100.27:49173"}}}`,
		"invalid log level":         `{"log_level":"everything"}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeConfig(strings.NewReader(input)); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestDeprecatedStatusListenIsIgnoredWithWarning(t *testing.T) {
	for _, value := range []string{`"127.0.0.1:8765"`, `"0.0.0.0:1"`} {
		cfg, err := DecodeConfig(strings.NewReader(`{"status_listen":` + value + `}`))
		if err != nil {
			t.Fatalf("old file with status_listen %s rejected: %v", value, err)
		}
		if len(cfg.DeprecatedKeys) != 1 || cfg.DeprecatedKeys[0] != "status_listen" {
			t.Fatalf("deprecated keys = %v", cfg.DeprecatedKeys)
		}
	}
	cfg, err := DecodeConfig(strings.NewReader(`{}`))
	if err != nil || len(cfg.DeprecatedKeys) != 0 {
		t.Fatalf("clean file reported deprecated keys: %v, %v", cfg.DeprecatedKeys, err)
	}
	if _, err := DecodeConfig(strings.NewReader(`{"status_listen":"a","status_listen":"b"}`)); err == nil {
		t.Fatal("duplicate deprecated key accepted")
	}
	data, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(data), "status_listen") {
		t.Fatalf("serialized configuration kept status_listen: %s, %v", data, err)
	}
}

func TestPeerTableFullPolicyValidation(t *testing.T) {
	cfg, err := DecodeConfig(strings.NewReader(`{}`))
	if err != nil || cfg.PeerTable.FullPolicy != "replace" {
		t.Fatalf("default policy = %q, %v", cfg.PeerTable.FullPolicy, err)
	}
	for _, policy := range []string{"replace", "refuse"} {
		cfg, err := DecodeConfig(strings.NewReader(`{"peer_table":{"full_policy":"` + policy + `"}}`))
		if err != nil {
			t.Fatalf("%s rejected: %v", policy, err)
		}
		engine, err := cfg.coreConfig()
		if err != nil || engine.FullPolicy != policy {
			t.Fatalf("core policy = %q, %v", engine.FullPolicy, err)
		}
	}
	cfg, err = DecodeConfig(strings.NewReader(`{"peer_table":{}}`))
	if err != nil || cfg.PeerTable.FullPolicy != "replace" {
		t.Fatalf("empty peer_table = %q, %v", cfg.PeerTable.FullPolicy, err)
	}
	for _, bad := range []string{`{"peer_table":{"full_policy":"drop"}}`, `{"peer_table":{"full_policy":""}}`,
		`{"peer_table":{"full_policy":"Replace"}}`, `{"peer_table":{"full_policy":3}}`, `{"peer_table":{"other":1}}`} {
		if _, err := DecodeConfig(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestProbePolicyExplicitOptIn(t *testing.T) {
	cfg, err := DecodeConfig(strings.NewReader(`{"enable_probes":true,"probe_prefixes":["200::/7"],"probe_ports":[12345],"probe_timeout":"10s","probe_interval":"10s","probe_global_interval":"10s"}`))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := cfg.coreConfig()
	if err != nil || !engine.EnableProbes || len(engine.ProbePrefixes) != 1 || len(engine.ProbePorts) != 1 || engine.ProbeTimeout != 10*time.Second || engine.ProbeInterval != 10*time.Second || engine.ProbeGlobalInterval != 10*time.Second {
		t.Fatalf("lost explicit policy or equal durations: %+v, %v", engine, err)
	}
}

func TestResourceGuardConfiguration(t *testing.T) {
	cfg, err := DecodeConfig(strings.NewReader(`{"resource_guard":{"enabled":true,"max_rss_kib":24576,"min_available_kib":32768,"interval":"5s"}}`))
	if err != nil || !cfg.ResourceGuard.Enabled {
		t.Fatalf("guard config rejected %v", err)
	}
	for _, draft := range []string{`{"resource_guard":{"interval":"1ms"}}`, `{"resource_guard":{"max_rss_kib":0}}`, `{"resource_guard":{"unknown":true}}`, `{"resource_guard":{"max_rss_kib":18446744073709551616}}`} {
		if _, err := DecodeConfig(strings.NewReader(draft)); err == nil {
			t.Fatal("invalid guard config accepted", draft)
		}
	}
}

func TestControlSocketPathConfiguration(t *testing.T) {
	for _, path := range []string{"/tmp/run/arednlink-hf/control.sock", "/run/private/control.sock"} {
		cfg, err := DecodeConfig(strings.NewReader(`{"control_socket":"` + path + `"}`))
		if err != nil || cfg.ControlSocket != path {
			t.Fatalf("control_socket = %q, %v", cfg.ControlSocket, err)
		}
	}
	for _, path := range []string{"relative/control.sock", "/tmp/run/../control.sock", "/tmp/run//control.sock", ""} {
		if _, err := DecodeConfig(strings.NewReader(`{"control_socket":"` + path + `"}`)); err == nil {
			t.Fatalf("accepted unsafe control_socket %q", path)
		}
	}
}

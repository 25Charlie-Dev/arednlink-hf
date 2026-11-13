package resourceguard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseKiB(t *testing.T) {
	for _, test := range []struct {
		input string
		valid bool
		value uint64
	}{
		{"Name:\ttest\nVmRSS:\t24576 kB\n", true, 24576},
		{"VmRSS: 0 kB\n", true, 0},
		{"VmRSS: 18446744073709551616 kB", false, 0},
		{"VmRSS: -1 kB", false, 0},
		{"VmRSS: 10 MB", false, 0},
		{"VmRSS: 10 KiB", false, 0},
		{"VmRSS: 10 kB extra", false, 0},
		{"VmRSS: 10 kB\nVmRSS: 11 kB", false, 0},
		{"VmSize: 20 kB", false, 0},
	} {
		value, err := parseKiB([]byte(test.input), "VmRSS:")
		if (err == nil) != test.valid || value != test.value {
			t.Fatalf("parse %q = %d %v", test.input, value, err)
		}
	}
}

func TestConfigBounds(t *testing.T) {
	for _, interval := range []string{"0s", "999ms", "61s", "invalid"} {
		cfg := Defaults()
		cfg.Interval = interval
		if cfg.Validate() == nil {
			t.Fatal("accepted interval", interval)
		}
	}
	for _, quantity := range []uint64{0, 1023, 1048577, ^uint64(0)} {
		cfg := Defaults()
		cfg.MaxRSSKiB = quantity
		if cfg.Validate() == nil {
			t.Fatal("accepted rss threshold", quantity)
		}
		cfg = Defaults()
		cfg.MinAvailableKiB = quantity
		if cfg.Validate() == nil {
			t.Fatal("accepted available threshold", quantity)
		}
	}
}

func TestThresholdsAndReadFailure(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, test := range []struct {
		name   string
		values measurement
		err    error
		state  string
	}{
		{"equal boundaries", measurement{24576, 32768}, nil, "healthy"},
		{"rss breach", measurement{24577, 32768}, nil, "stopping"},
		{"available breach", measurement{1000, 32767}, nil, "stopping"},
		{"read failure", measurement{}, errors.New("missing proc fixture"), "degraded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Enabled = true
			monitor, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			monitor.read = func(context.Context) (measurement, error) { return test.values, test.err }
			err = monitor.check(context.Background(), logger)
			snapshot := monitor.Snapshot()
			if snapshot.State != test.state || (err == nil) != (test.state == "healthy") {
				t.Fatalf("state %s error %v", snapshot.State, err)
			}
			if test.err != nil && (snapshot.RSSKiB != nil || snapshot.AvailableKiB != nil) {
				t.Fatal("read failure fabricated measurements")
			}
			if snapshot.RSSKiB != nil {
				*snapshot.RSSKiB = 999
				if *monitor.Snapshot().RSSKiB == 999 {
					t.Fatal("snapshot aliases shared state")
				}
			}
		})
	}
}

func TestRunCancellation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := Defaults()
		cfg.Enabled = enabled
		monitor, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		checked := make(chan struct{}, 1)
		monitor.read = func(context.Context) (measurement, error) {
			checked <- struct{}{}
			return measurement{1000, 100000}, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- monitor.Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
		if enabled {
			select {
			case <-checked:
			case <-time.After(time.Second):
				t.Fatal("initial sample absent")
			}
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("guard leaked after cancellation")
		}
	}
}

func TestBoundedProcFile(t *testing.T) {
	data, err := readBounded("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseKiB(data, "VmRSS:"); err != nil {
		t.Fatal(err)
	}
	if _, err := readBounded("/proc/absent-resource-guard-fixture"); err == nil {
		t.Fatal("missing proc reported success")
	}
	if _, err := parseKiB([]byte(strings.Repeat("x", 100)), "MemAvailable:"); err == nil {
		t.Fatal("missing field accepted")
	}
}

func TestProcDocumentLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 16385)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBounded(path); err == nil {
		t.Fatal("oversized proc document accepted")
	}
}

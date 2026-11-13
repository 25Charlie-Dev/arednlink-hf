// Package resourceguard monitors controller memory without imposing kernel limits.
package resourceguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config sets controller RSS and system available-memory thresholds in KiB.
type Config struct {
	Enabled         bool   `json:"enabled"`
	MaxRSSKiB       uint64 `json:"max_rss_kib"`
	MinAvailableKiB uint64 `json:"min_available_kib"`
	Interval        string `json:"interval"`
}

// Defaults leaves the guard disabled with conservative alpha thresholds.
func Defaults() Config { return Config{MaxRSSKiB: 24576, MinAvailableKiB: 32768, Interval: "5s"} }

// Validate bounds polling and threshold values even when disabled.
func (c Config) Validate() error {
	interval, err := time.ParseDuration(c.Interval)
	if err != nil || interval < time.Second || interval > time.Minute {
		return errors.New("resource_guard interval must be between 1s and 1m")
	}
	if c.MaxRSSKiB < 1024 || c.MaxRSSKiB > 1048576 || c.MinAvailableKiB < 1024 || c.MinAvailableKiB > 1048576 {
		return errors.New("resource_guard thresholds must be between 1024 and 1048576 kib")
	}
	return nil
}

// Snapshot reports only observed values; null measurements were not obtained.
type Snapshot struct {
	State           string  `json:"state"`
	Reason          string  `json:"reason,omitempty"`
	RSSKiB          *uint64 `json:"rss_kib"`
	AvailableKiB    *uint64 `json:"available_kib"`
	MaxRSSKiB       uint64  `json:"max_rss_kib"`
	MinAvailableKiB uint64  `json:"min_available_kib"`
}

type measurement struct{ rss, available uint64 }

// Monitor owns one polling timer; it measures this controller, not child processes.
type Monitor struct {
	mu   sync.RWMutex
	cfg  Config
	view Snapshot
	read func(context.Context) (measurement, error)
}

// New creates a validated guard without starting any background work.
func New(cfg Config) (*Monitor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	state := "disabled"
	if cfg.Enabled {
		state = "starting"
	}
	return &Monitor{cfg: cfg, view: Snapshot{State: state, MaxRSSKiB: cfg.MaxRSSKiB, MinAvailableKiB: cfg.MinAvailableKiB}, read: readProc}, nil
}

// Snapshot returns a copy that cannot mutate the monitor's state.
func (m *Monitor) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	view := m.view
	if view.RSSKiB != nil {
		value := *view.RSSKiB
		view.RSSKiB = &value
	}
	if view.AvailableKiB != nil {
		value := *view.AvailableKiB
		view.AvailableKiB = &value
	}
	return view
}

// Run stops with an error on unavailable measurements or a threshold breach.
func (m *Monitor) Run(ctx context.Context, logger *slog.Logger) error {
	if !m.cfg.Enabled {
		<-ctx.Done()
		return nil
	}
	interval, err := time.ParseDuration(m.cfg.Interval)
	if err != nil {
		return err
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if err := m.check(ctx, logger); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}

func (m *Monitor) check(ctx context.Context, logger *slog.Logger) error {
	values, err := m.read(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.view.State = "degraded"
		m.view.Reason = "resource measurements unavailable"
		m.view.RSSKiB = nil
		m.view.AvailableKiB = nil
		logger.Error("resource guard stopping controller", "reason", m.view.Reason, "error", err)
		return errors.New("resource guard measurements unavailable")
	}
	m.view.RSSKiB = &values.rss
	m.view.AvailableKiB = &values.available
	reason := ""
	if values.rss > m.cfg.MaxRSSKiB {
		reason = "controller rss threshold exceeded"
	}
	if values.available < m.cfg.MinAvailableKiB {
		reason = "system available memory below threshold"
	}
	if reason != "" {
		m.view.State = "stopping"
		m.view.Reason = reason
		logger.Error("resource guard stopping controller", "reason", reason, "rss_kib", values.rss, "available_kib", values.available, "max_rss_kib", m.cfg.MaxRSSKiB, "min_available_kib", m.cfg.MinAvailableKiB)
		return fmt.Errorf("resource guard: %s", reason)
	}
	m.view.State = "healthy"
	m.view.Reason = ""
	return nil
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 16385))
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > 16384 {
		return nil, errors.New("proc memory document exceeds limit")
	}
	return data, nil
}

func readProc(ctx context.Context) (measurement, error) {
	if err := ctx.Err(); err != nil {
		return measurement{}, err
	}
	status, err := readBounded("/proc/self/status")
	if err != nil {
		return measurement{}, err
	}
	rss, err := parseKiB(status, "VmRSS:")
	if err != nil {
		return measurement{}, err
	}
	meminfo, err := readBounded("/proc/meminfo")
	if err != nil {
		return measurement{}, err
	}
	available, err := parseKiB(meminfo, "MemAvailable:")
	if err != nil {
		return measurement{}, err
	}
	return measurement{rss: rss, available: available}, nil
}

func parseKiB(data []byte, key string) (uint64, error) {
	var value uint64
	found := false
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		fields := strings.Fields(string(line))
		if len(fields) == 0 || fields[0] != key {
			continue
		}
		if found || len(fields) != 3 || fields[2] != "kB" {
			return 0, errors.New("invalid or duplicate proc memory field")
		}
		parsed, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, errors.New("invalid proc memory quantity")
		}
		value = parsed
		found = true
	}
	if !found {
		return 0, errors.New("required proc memory field missing")
	}
	return value, nil
}

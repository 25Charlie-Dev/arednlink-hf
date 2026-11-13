// Package radio receives advertisements and can write only an explicitly configured beacon.
// Source fields are unverified claims.
package radio

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"strconv"
	"time"

	"arednlink-hf/internal/protocol"
)

// Message carries the original advertisement body and an unverified source claim.
type Message struct {
	Source    string
	Transport string
	Payload   string
}

// Config selects one radio adapter, a literal TCP endpoint, and an optional periodic beacon.
// AllowRemote explicitly permits a controlled unicast endpoint beyond loopback.
// OnState, when set, runs synchronously and must not block.
// OnReceive observes fresh JS8Call reception synchronously and must not block;
// an empty callsign identifies received activity without a source claim.
type Config struct {
	Kind        string
	Endpoint    string
	Callsign    string
	Destination string
	AllowRemote bool
	Beacon      BeaconConfig
	OnState     func(string)
	OnReceive   func(callsign string)
	// OnTransmit reports scheduled transmission progress and must not block.
	OnTransmit        func(string)
	OnTransmitRequest func(requestID uint64)
	OnTransmitRefused func(requestID uint64, reason string)
}

// BeaconConfig enables bounded periodic endpoint advertisements when explicitly set.
type BeaconConfig struct {
	Enabled         bool
	Endpoint        netip.AddrPort
	Address         netip.Addr
	Services        uint16
	ValidityMinutes uint8
	Interval        time.Duration
	Jitter          time.Duration
}

// UnavailableError identifies an adapter omitted from the build.
type UnavailableError struct{ Kind string }

func (e *UnavailableError) Error() string {
	return "requested radio adapter is unavailable in this build"
}

// ConfigError identifies an invalid configuration before any connection attempt.
type ConfigError struct{ Field string }

func (e *ConfigError) Error() string { return "invalid radio configuration: " + e.Field }

const maxBeaconFrameBytes = 256

// minAdapterTransmitInterval is the floor between two transmit attempts on one
// adapter; each adapter owns its own schedule, so the floor is not process-wide.
const minAdapterTransmitInterval = 30 * time.Minute

// stableSession is how long a connection must last before reconnect backoff resets.
const stableSession = 90 * time.Second

// Keepalive timing detects a half-open modem socket after about
// keepAliveIdle + keepAliveCount*keepAliveInterval. A quiet but healthy link
// is never treated as dead: silence on the air is normal.
const (
	keepAliveIdle     = 30 * time.Second
	keepAliveInterval = 10 * time.Second
	keepAliveCount    = 3
)

type decoder interface {
	feed([]byte, func(Message)) uint64
}

type transmitter interface {
	encode(string, uint64) ([]byte, error)
}

// Supported returns the adapter names included by build tags.
func Supported() []string {
	result := make([]string, 0, 2)
	if kissAvailable() {
		result = append(result, "kiss")
	}
	if js8Available() {
		result = append(result, "js8call")
	}
	return result
}

// Run owns reconnects and the live socket until cancellation. Handler failures
// are counted and do not tear down the transport; handlers must honor context.
func Run(ctx context.Context, cfg Config, handler func(context.Context, Message) error, logger *slog.Logger) error {
	if handler == nil {
		return &ConfigError{Field: "handler"}
	}
	if err := Validate(cfg); err != nil {
		return err
	}
	endpoint, err := validatedEndpoint(cfg)
	if err != nil {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}
	schedule, err := scheduleFor(cfg, time.Now())
	if err != nil {
		return err
	}
	reports := eventLog{logger: logger.With("transport", cfg.Kind)}
	return supervise(ctx, cfg, endpoint, handler, &reports, schedule)
}

// scheduleFor returns nil unless the beacon is explicitly enabled; a nil
// schedule means the adapter never writes to the modem.
func scheduleFor(cfg Config, now time.Time) (*beaconSchedule, error) {
	if !cfg.Beacon.Enabled {
		return nil, nil
	}
	writer, err := makeTransmitter(cfg)
	if err != nil {
		return nil, err
	}
	schedule, err := newBeaconSchedule(cfg.Beacon, writer, now)
	if err != nil {
		return nil, err
	}
	transmitState(cfg, "scheduled")
	return schedule, nil
}

// Validate checks adapter availability and all settings without opening a socket.
func Validate(cfg Config) error {
	if _, err := makeDecoder(cfg); err != nil {
		return err
	}
	_, err := validatedEndpoint(cfg)
	if err != nil {
		return err
	}
	if !cfg.Beacon.Enabled {
		return nil
	}
	if cfg.Callsign == "" || cfg.Destination == "" || cfg.Beacon.Endpoint.IsValid() == cfg.Beacon.Address.IsValid() || cfg.Beacon.ValidityMinutes < 1 || cfg.Beacon.ValidityMinutes > 180 ||
		(cfg.Beacon.Interval != 30*time.Minute && cfg.Beacon.Interval != 60*time.Minute) || cfg.Beacon.Jitter <= 0 || cfg.Beacon.Jitter > 5*time.Minute {
		return &ConfigError{Field: "advert"}
	}
	advertisement := protocol.Advertisement{Endpoint: cfg.Beacon.Endpoint, Address: cfg.Beacon.Address, Services: cfg.Beacon.Services,
		ServicesPresent: cfg.Beacon.Address.IsValid(), ValidFor: time.Duration(cfg.Beacon.ValidityMinutes) * time.Minute}
	if _, err := protocol.EncodeCompact(advertisement); err != nil {
		return err
	}
	if _, err := makeTransmitter(cfg); err != nil {
		return err
	}
	return nil
}

func newBeaconSchedule(cfg BeaconConfig, writer transmitter, now time.Time) (*beaconSchedule, error) {
	jitter, err := randomDuration(cfg.Jitter)
	if err != nil {
		return nil, fmt.Errorf("initial advert jitter: %w", err)
	}
	return &beaconSchedule{config: cfg, writer: writer, next: now.Add(cfg.Interval + jitter)}, nil
}

type beaconSchedule struct {
	config      BeaconConfig
	writer      transmitter
	next        time.Time
	lastAttempt time.Time
	requestID   uint64
}

func (s *beaconSchedule) due(now time.Time) (bool, error) {
	if !s.lastAttempt.IsZero() {
		minimum := s.lastAttempt.Add(minAdapterTransmitInterval)
		if s.next.Before(minimum) {
			s.next = minimum
		}
	}
	if now.Before(s.next) {
		return false, nil
	}
	jitter, err := randomDuration(s.config.Jitter)
	if err != nil {
		return false, fmt.Errorf("next advert jitter: %w", err)
	}
	s.lastAttempt = now
	s.next = now.Add(s.config.Interval + jitter)
	return true, nil
}

func randomDuration(maximum time.Duration) (time.Duration, error) {
	if maximum <= 0 {
		return 0, nil
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(maximum)+1))
	if err != nil {
		return 0, err
	}
	return time.Duration(value.Int64()), nil
}

func makeDecoder(cfg Config) (decoder, error) {
	switch cfg.Kind {
	case "kiss":
		return newKISS(cfg)
	case "js8call":
		return newJS8(cfg)
	default:
		return nil, &ConfigError{Field: "kind"}
	}
}

func makeTransmitter(cfg Config) (transmitter, error) {
	switch cfg.Kind {
	case "kiss":
		return newKISSTransmitter(cfg)
	case "js8call":
		return newJS8Transmitter(cfg)
	default:
		return nil, &ConfigError{Field: "kind"}
	}
}

func validatedEndpoint(cfg Config) (string, error) {
	endpoint, err := netip.ParseAddrPort(cfg.Endpoint)
	if err != nil || endpoint.Port() == 0 {
		return "", &ConfigError{Field: "endpoint"}
	}
	addr := endpoint.Addr()
	if addr.Zone() != "" || addr.Is4In6() || !addr.IsGlobalUnicast() && !addr.IsLoopback() {
		return "", &ConfigError{Field: "endpoint"}
	}
	if addr.Is4() && addr.As4() == [4]byte{255, 255, 255, 255} {
		return "", &ConfigError{Field: "endpoint"}
	}
	if !addr.IsLoopback() && !cfg.AllowRemote {
		return "", &ConfigError{Field: "allow_remote"}
	}
	return net.JoinHostPort(addr.String(), strconv.Itoa(int(endpoint.Port()))), nil
}

func supervise(ctx context.Context, cfg Config, endpoint string, handler func(context.Context, Message) error, reports *eventLog, schedule *beaconSchedule) error {
	defer state(cfg, "stopped")
	delay := 2 * time.Second
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAliveConfig: net.KeepAliveConfig{
		Enable: true, Idle: keepAliveIdle, Interval: keepAliveInterval, Count: keepAliveCount}}
	for ctx.Err() == nil {
		state(cfg, "connecting")
		conn, err := dialer.DialContext(ctx, "tcp", endpoint)
		if err == nil {
			state(cfg, "connected")
			started := time.Now()
			reports.note(ctx, slog.LevelInfo, "radio.connection", "connected")
			err = receive(ctx, conn, cfg, handler, reports, schedule)
			if time.Since(started) >= stableSession {
				delay = 2 * time.Second
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state(cfg, "backoff")
		reports.note(ctx, slog.LevelWarn, "radio.connection", "disconnected")
		if err = backoff(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, 60*time.Second)
	}
	return ctx.Err()
}

func receive(ctx context.Context, conn net.Conn, cfg Config, handler func(context.Context, Message) error, reports *eventLog, schedule *beaconSchedule) error {
	parser, err := makeDecoder(cfg)
	if err != nil {
		// The connection is not yet owned by receiveWith; close it here.
		if closeErr := conn.Close(); closeErr != nil {
			reports.note(ctx, slog.LevelDebug, "radio.connection", "close_failed")
		}
		return err
	}
	return receiveWith(ctx, conn, parser, cfg, handler, reports, schedule)
}

// receiveWith reads until cancellation or a transport error. A silent link is
// healthy: the only read deadline is the next beacon due time, and half-open
// sockets are detected by TCP keepalive on the dialer.
func receiveWith(ctx context.Context, conn net.Conn, parser decoder, cfg Config, handler func(context.Context, Message) error, reports *eventLog, schedule *beaconSchedule) error {
	done := make(chan struct{})
	joined := make(chan struct{})
	go closeOnCancel(ctx, conn, done, joined)
	defer func() {
		close(done)
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			reports.note(ctx, slog.LevelDebug, "radio.connection", "close_failed")
		}
		<-joined
	}()
	emit := func(message Message) {
		if ctx.Err() != nil {
			return
		}
		if err := handler(ctx, message); err != nil {
			reports.note(ctx, slog.LevelWarn, "radio.handler", "rejected")
		}
	}
	buffer := make([]byte, 1024)
	for ctx.Err() == nil {
		// The zero time clears any deadline: block until data, close or a beacon is due.
		var deadline time.Time
		beaconDeadline := false
		if schedule != nil {
			deadline = schedule.next
			beaconDeadline = true
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return fmt.Errorf("read deadline: %w", err)
		}
		count, err := conn.Read(buffer)
		if count > 0 {
			if rejected := parser.feed(buffer[:count], emit); rejected > 0 {
				reports.rejected(ctx, rejected)
			}
		}
		if err != nil {
			var networkError net.Error
			if beaconDeadline && errors.As(err, &networkError) && networkError.Timeout() {
				if txErr := transmitBeacon(ctx, conn, cfg, schedule); txErr != nil {
					return txErr
				}
				continue
			}
			return fmt.Errorf("receive: %w", err)
		}
		if count == 0 {
			return io.ErrNoProgress
		}
		if txErr := transmitBeacon(ctx, conn, cfg, schedule); txErr != nil {
			return txErr
		}
	}
	return ctx.Err()
}

func transmitBeacon(ctx context.Context, conn net.Conn, cfg Config, schedule *beaconSchedule) error {
	if schedule == nil {
		return nil
	}
	due, err := schedule.due(time.Now())
	if err != nil || !due {
		return err
	}
	transmitState(cfg, "queued")
	advertisement := protocol.Advertisement{Endpoint: cfg.Beacon.Endpoint, Address: cfg.Beacon.Address, Services: cfg.Beacon.Services,
		ServicesPresent: cfg.Beacon.Address.IsValid(),
		ValidFor:        time.Duration(cfg.Beacon.ValidityMinutes) * time.Minute}
	payload, err := protocol.EncodeCompact(advertisement)
	if err != nil {
		return err
	}
	schedule.requestID++
	if cfg.OnTransmitRequest != nil {
		cfg.OnTransmitRequest(schedule.requestID)
	}
	frame, err := schedule.writer.encode(payload, schedule.requestID)
	if err != nil {
		return err
	}
	if len(frame) == 0 || len(frame) > maxBeaconFrameBytes {
		return &ConfigError{Field: "advert frame size"}
	}
	transmitState(cfg, "writing")
	if err := writeFrame(ctx, conn, frame); err != nil {
		transmitState(cfg, "write_failed")
		return fmt.Errorf("write advert: %w", err)
	}
	transmitState(cfg, "written")
	return nil
}

func writeFrame(ctx context.Context, conn net.Conn, frame []byte) error {
	if len(frame) == 0 || len(frame) > maxBeaconFrameBytes {
		return &ConfigError{Field: "advert frame size"}
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	for offset := 0; offset < len(frame); {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := conn.Write(frame[offset:])
		if count > 0 {
			offset += count
		}
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear write deadline: %w", err)
	}
	return nil
}

func transmitState(cfg Config, state string) {
	if cfg.OnTransmit != nil {
		cfg.OnTransmit(state)
	}
}

func closeOnCancel(ctx context.Context, conn net.Conn, done <-chan struct{}, joined chan<- struct{}) {
	defer close(joined)
	select {
	case <-ctx.Done():
		// Discarded: the socket Close error on cancellation; the blocked read
		// is released either way and the receive loop reports its own result.
		_ = conn.Close()
	case <-done:
	}
}

func backoff(ctx context.Context, base time.Duration) error {
	jitter, err := rand.Int(rand.Reader, big.NewInt(400001))
	if err != nil {
		return fmt.Errorf("reconnect jitter: %w", err)
	}
	delay := time.Duration(int64(base) * (800000 + jitter.Int64()) / 1000000)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type eventLog struct {
	logger *slog.Logger
	next   [4]time.Time
	counts [4]uint64
}

func (l *eventLog) note(ctx context.Context, level slog.Level, event, reason string) {
	slot := 0
	if reason == "connected" {
		slot = 1
	}
	if event == "radio.handler" {
		slot = 2
	}
	l.record(ctx, slot, level, event, reason, 1)
}
func (l *eventLog) rejected(ctx context.Context, count uint64) {
	l.record(ctx, 3, slog.LevelWarn, "radio.input", "invalid_or_filtered", count)
}
func (l *eventLog) record(ctx context.Context, slot int, level slog.Level, event, reason string, count uint64) {
	l.counts[slot] += count
	now := time.Now()
	if now.Before(l.next[slot]) {
		return
	}
	l.next[slot] = now.Add(time.Minute)
	l.logger.Log(ctx, level, event, "reason", reason, "count", l.counts[slot])
}

func state(cfg Config, value string) {
	if cfg.OnState != nil {
		cfg.OnState(value)
	}
}

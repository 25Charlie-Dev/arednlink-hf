package radio

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type noopDecoder struct{}

func (noopDecoder) feed([]byte, func(Message)) uint64 { return 0 }

type fixedFrame struct{}

func (fixedFrame) encode(string, uint64) ([]byte, error) { return []byte("FRAME"), nil }

// deadlineConn blocks reads until closed and records every read deadline set.
type deadlineConn struct {
	net.Conn
	mu        sync.Mutex
	deadlines []time.Time
	closed    chan struct{}
	once      sync.Once
}

func newDeadlineConn() *deadlineConn { return &deadlineConn{closed: make(chan struct{})} }

func (c *deadlineConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}
func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadlines = append(c.deadlines, t)
	return nil
}
func (c *deadlineConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func quietReports() *eventLog {
	return &eventLog{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func beaconTestConfig() Config {
	return Config{Kind: "kiss", Callsign: "N0CALL", Destination: "AREDNH", Beacon: BeaconConfig{
		Enabled: true, Endpoint: netip.MustParseAddrPort("198.51.100.27:49173"),
		ValidityMinutes: 90, Interval: 40 * time.Millisecond}}
}

// A silent link must stay connected: with no beacon schedule no read deadline
// is ever armed, so no timeout can end the session no matter how long it idles.
func TestQuietLinkIsNotRedialed(t *testing.T) {
	conn := newDeadlineConn()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- receiveWith(ctx, conn, noopDecoder{}, Config{}, func(context.Context, Message) error { return nil }, quietReports(), nil)
	}()
	select {
	case err := <-done:
		t.Fatalf("quiet link ended the session: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	conn.mu.Lock()
	for _, deadline := range conn.deadlines {
		if !deadline.IsZero() {
			t.Errorf("idle read deadline armed: %v", deadline)
		}
	}
	armed := len(conn.deadlines)
	conn.mu.Unlock()
	if armed == 0 {
		t.Error("read deadline was never cleared")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("unexpected result %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("receive did not stop on cancellation")
	}
}

func TestScheduleOnlyWhenAdvertEnabled(t *testing.T) {
	cfg := beaconTestConfig()
	cfg.Beacon.Enabled = false
	schedule, err := scheduleFor(cfg, time.Now())
	if err != nil || schedule != nil {
		t.Fatalf("disabled advert produced schedule %v, %v", schedule, err)
	}
}

// countModemWrites runs receiveWith against a loopback modem and returns the
// number of frames the modem received within the observation window.
func countModemWrites(t *testing.T, schedule *beaconSchedule, cfg Config, window time.Duration) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	modem, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer modem.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- receiveWith(ctx, client, noopDecoder{}, cfg, func(context.Context, Message) error { return nil }, quietReports(), schedule)
	}()
	if err := modem.SetReadDeadline(time.Now().Add(window)); err != nil {
		t.Fatal(err)
	}
	total := 0
	buffer := make([]byte, 64)
	for {
		count, err := modem.Read(buffer)
		total += count
		if err != nil {
			break
		}
	}
	cancel()
	<-done
	if total%len("FRAME") != 0 {
		t.Fatalf("partial frame: %d bytes", total)
	}
	return total / len("FRAME")
}

// The would-be due time is 40 ms after the schedule is built; the window runs
// several times past it. The later due times are held back by the 30 minute floor.
func TestAdvertDisabledWritesNothingPastDueTime(t *testing.T) {
	cfg := beaconTestConfig()
	cfg.Beacon.Enabled = false
	schedule, err := scheduleFor(cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if writes := countModemWrites(t, schedule, cfg, 400*time.Millisecond); writes != 0 {
		t.Fatalf("disabled advert wrote %d frames", writes)
	}
}

func TestAdvertEnabledWritesExactlyOnePastDueTime(t *testing.T) {
	cfg := beaconTestConfig()
	schedule, err := newBeaconSchedule(cfg.Beacon, fixedFrame{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if writes := countModemWrites(t, schedule, cfg, 400*time.Millisecond); writes != 1 {
		t.Fatalf("enabled advert wrote %d frames, want exactly 1", writes)
	}
}

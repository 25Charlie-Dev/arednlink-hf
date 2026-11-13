package radio

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func TestEndpointPolicy(t *testing.T) {
	for _, tt := range []struct {
		endpoint      string
		remote, valid bool
	}{
		{"127.0.0.1:8001", false, true}, {"[::1]:2442", false, true},
		{"localhost:2442", false, false}, {"127.0.0.1:0", false, false},
		{"192.168.1.2:8001", false, false}, {"192.168.1.2:8001", true, true},
		{"[::ffff:127.0.0.1]:8001", true, false}, {"[fe80::1%eth0]:8001", true, false},
		{"0.0.0.0:8001", true, false}, {"224.0.0.1:8001", true, false}, {"255.255.255.255:8001", true, false},
	} {
		t.Run(tt.endpoint, func(t *testing.T) {
			_, err := validatedEndpoint(Config{Endpoint: tt.endpoint, AllowRemote: tt.remote})
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v error=%v", tt.valid, err)
			}
		})
	}
}
func TestUnavailableBuildAdapter(t *testing.T) {
	for _, tt := range []struct {
		kind      string
		available bool
	}{{"kiss", kissAvailable()}, {"js8call", js8Available()}} {
		if tt.available {
			continue
		}
		err := Run(t.Context(), Config{Kind: tt.kind}, func(context.Context, Message) error { return nil }, nil)
		var missing *UnavailableError
		if !errors.As(err, &missing) {
			t.Fatalf("kind %s: expected unavailable, got %v", tt.kind, err)
		}
	}
}
func TestCancelClosesReadAndJoinsWatcher(t *testing.T) {
	kind := "kiss"
	cfg := Config{Kind: kind, Callsign: "N0CALL"}
	if !kissAvailable() {
		cfg.Kind = "js8call"
	}
	if !kissAvailable() && !js8Available() {
		t.Skip("no decoder included")
	}
	ctx, cancel := context.WithCancel(t.Context())
	reader, peer := net.Pipe()
	defer peer.Close()
	done := make(chan error, 1)
	reports := eventLog{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go func() {
		done <- receive(ctx, reader, cfg, func(context.Context, Message) error { return nil }, &reports, nil)
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("read did not exit on cancellation")
	}
}
func TestBackoffCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := backoff(ctx, 60*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestBeaconScheduleUsesThirtyMinuteGlobalFloor(t *testing.T) {
	now := time.Now()
	schedule, err := newBeaconSchedule(BeaconConfig{Interval: 30 * time.Minute}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if due, err := schedule.due(now.Add(29 * time.Minute)); err != nil || due {
		t.Fatalf("early beacon due=%v err=%v", due, err)
	}
	first := now.Add(30 * time.Minute)
	if due, err := schedule.due(first); err != nil || !due {
		t.Fatalf("first beacon due=%v err=%v", due, err)
	}
	if due, err := schedule.due(first.Add(29 * time.Minute)); err != nil || due {
		t.Fatalf("global floor due=%v err=%v", due, err)
	}
	if due, err := schedule.due(first.Add(30 * time.Minute)); err != nil || !due {
		t.Fatalf("next beacon due=%v err=%v", due, err)
	}
}

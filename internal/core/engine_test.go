package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"arednlink-hf/internal/protocol"
)

const fixture = "AL1 198.51.100.27:49173 1"

func probeConfig() Config {
	return Config{
		EnableProbes:  true,
		ProbePrefixes: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")},
		ProbePorts:    []uint16{49173},
	}
}

func makeEngine(t *testing.T, cfg Config, dial dialContext, now func() time.Time) *Engine {
	t.Helper()
	e, err := newEngine(cfg, nil, dial, now)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func successfulDial(_ context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	if err := server.Close(); err != nil {
		return nil, err
	}
	return client, nil
}

func TestConfigurationPolicyAndCopies(t *testing.T) {
	bad := []Config{
		{MaxPeers: -1}, {MaxPeers: 4097}, {ProbeTimeout: -time.Second},
		{ProbeTimeout: 31 * time.Second}, {ProbeGlobalInterval: time.Second},
		{ProbeInterval: 9 * time.Second}, {EnableProbes: true},
		{FullPolicy: "drop"}, {ProbePorts: []uint16{0}}, {ProbePrefixes: []netip.Prefix{{}}},
		{ProbePrefixes: []netip.Prefix{netip.MustParsePrefix("::ffff:198.51.100.0/120")}},
	}
	for i, cfg := range bad {
		if _, err := New(cfg, nil); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("configuration %d: error = %v", i, err)
		}
	}
	cfg := probeConfig()
	e := makeEngine(t, cfg, successfulDial, time.Now)
	cfg.ProbePorts[0] = 80
	cfg.ProbePrefixes[0] = netip.MustParsePrefix("203.0.113.0/24")
	if err := e.Observe(context.Background(), "N0CALL", "kiss", fixture); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot()[0].Probe; got != ProbeACK {
		t.Fatalf("copied policy result = %q", got)
	}
}

func TestMetadataCapacityExpiryAndSnapshot(t *testing.T) {
	now := time.Now()
	e := makeEngine(t, Config{MaxPeers: 2, FullPolicy: FullPolicyRefuse}, successfulDial, func() time.Time { return now })
	for _, metadata := range [][2]string{{"bad", "kiss"}, {"N0CALL", "aprs"}} {
		if err := e.Observe(context.Background(), metadata[0], metadata[1], fixture); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("metadata error = %v", err)
		}
	}
	if err := e.Observe(context.Background(), "N0CALL", "kiss", "invalid"); err == nil {
		t.Fatal("malformed claim accepted")
	}
	for _, source := range []string{"Z1ABC", "A1ABC"} {
		if err := e.Observe(context.Background(), source, "kiss", fixture); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Observe(context.Background(), "B1ABC", "kiss", fixture); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity error = %v", err)
	}
	if got := e.PeerTable(); got.Refused != 1 || got.Evicted != 0 || got.Policy != FullPolicyRefuse {
		t.Fatalf("refuse counters = %+v", got)
	}
	peers := e.Snapshot()
	if len(peers) != 2 || peers[0].Callsign != "A1ABC" || peers[0].Probe != ProbeNotChecked {
		t.Fatalf("snapshot = %+v", peers)
	}
	peers[0].Endpoint = "modified"
	if e.Snapshot()[0].Endpoint == "modified" {
		t.Fatal("snapshot modified internal state")
	}
	now = now.Add(30 * time.Second)
	if err := e.Observe(context.Background(), "A1ABC", "kiss", fixture); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	if got := e.Snapshot(); len(got) != 1 || got[0].Callsign != "A1ABC" {
		t.Fatalf("expiry did not preserve refreshed peer: %+v", got)
	}
	now = now.Add(30 * time.Second)
	if len(e.Snapshot()) != 0 {
		t.Fatal("peer remained at exact expiry")
	}
	if err := e.Observe(context.Background(), "B1ABC", "kiss", fixture); err != nil {
		t.Fatalf("expired capacity not reclaimed: %v", err)
	}
}

func TestServiceMetadataPreservedAndLegacyClearsIt(t *testing.T) {
	e := makeEngine(t, Config{MaxPeers: 2}, successfulDial, time.Now)
	base, err := protocol.Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	base.Services, base.ServicesPresent = 0x400b, true
	al3, err := protocol.EncodeCompact(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Observe(context.Background(), "N0CALL", "kiss", al3); err != nil {
		t.Fatal(err)
	}
	peer := e.Snapshot()[0]
	wantNames := []string{"signal_proxy", "briar_contact", "yggdrasil_connectivity"}
	if peer.Services == nil || *peer.Services != 0x400b || !reflect.DeepEqual(peer.ServiceNames, wantNames) || peer.UnassignedServiceBits != 0x4000 {
		t.Fatalf("service metadata = %+v", peer)
	}
	*peer.Services = 0
	peer.ServiceNames[0] = "changed"
	if got := e.Snapshot()[0]; *got.Services != 0x400b || got.ServiceNames[0] != "signal_proxy" {
		t.Fatalf("snapshot mutation changed stored metadata: %+v", got)
	}
	base.Services = 0
	al3, err = protocol.EncodeCompact(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Observe(context.Background(), "N0CALL", "kiss", al3); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot()[0]; got.Services == nil || *got.Services != 0 || len(got.ServiceNames) != 0 {
		t.Fatalf("explicit zero bitmap became absent: %+v", got)
	}
	if err := e.Observe(context.Background(), "N0CALL", "kiss", fixture); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot()[0]; got.Services != nil || len(got.ServiceNames) != 0 || got.UnassignedServiceBits != 0 {
		t.Fatalf("legacy advertisement fabricated service state: %+v", got)
	}
}

func TestProbePolicy(t *testing.T) {
	cases := []struct {
		name, payload, state, reason string
		cfg                          Config
	}{
		{"disabled", fixture, ProbeNotChecked, "disabled", Config{}},
		{"prefix", "AL1 203.0.113.1:49173 1", ProbeSkippedPolicy, "destination_not_allowed", probeConfig()},
		{"port", "AL1 198.51.100.27:80 1", ProbeSkippedPolicy, "destination_not_allowed", probeConfig()},
		{"private denied", "AL1 10.0.0.1:49173 1", ProbeSkippedPolicy, "destination_not_allowed", probeConfig()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := makeEngine(t, tc.cfg, func(context.Context, string, string) (net.Conn, error) {
				t.Error("policy performed a dial")
				return nil, errors.New("unexpected dial")
			}, time.Now)
			if err := e.Observe(context.Background(), "N0CALL", "kiss", tc.payload); err != nil {
				t.Fatal(err)
			}
			got := e.Snapshot()[0]
			if got.Probe != tc.state || got.ProbeReason != tc.reason {
				t.Fatalf("result = %+v", got)
			}
		})
	}
	cfg := probeConfig()
	cfg.ProbePrefixes = []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}
	e := makeEngine(t, cfg, successfulDial, time.Now)
	if err := e.Observe(context.Background(), "N0CALL", "kiss", "AL1 10.0.0.1:49173 1"); err != nil || e.Snapshot()[0].Probe != ProbeACK {
		t.Fatalf("explicit private policy: %v, %+v", err, e.Snapshot())
	}
}

func TestProbeFailureAndCooldownBudgets(t *testing.T) {
	now := time.Now()
	calls := 0
	refused := syscall.ECONNREFUSED
	e := makeEngine(t, probeConfig(), func(context.Context, string, string) (net.Conn, error) {
		calls++
		return nil, refused
	}, func() time.Time { return now })
	if err := e.Observe(context.Background(), "N0CALL", "kiss", fixture); !errors.Is(err, ErrProbeFailed) || !errors.Is(err, refused) {
		t.Fatalf("probe failure error = %v", err)
	}
	if got := e.Snapshot()[0]; got.Probe != ProbeNACK || got.ProbeReason != "refused" {
		t.Fatalf("failure status = %+v", got)
	}
	for _, item := range []struct{ source, transport, payload string }{
		{"N0CALL", "kiss", fixture},
		{"N1CALL", "js8call", fixture},
		{"N0CALL", "kiss", "AL1 198.51.100.28:49173 1"},
		{"N2CALL", "kiss", "AL1 198.51.100.29:49173 1"},
	} {
		if err := e.Observe(context.Background(), item.source, item.transport, item.payload); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("callsign/transport/endpoint changes bypassed budget: %d dials", calls)
	}
	now = now.Add(10 * time.Second)
	if err := e.Observe(context.Background(), "N2CALL", "kiss", "AL1 198.51.100.29:49173 1"); !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("global interval did not permit next destination: %v", err)
	}
	if calls != 2 {
		t.Fatalf("dials = %d", calls)
	}
	now = now.Add(50 * time.Second)
	if err := e.Observe(context.Background(), "N1CALL", "js8call", fixture); !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("endpoint cooldown did not expire: %v", err)
	}
}

func TestEndpointCacheBoundAndExpiry(t *testing.T) {
	now := time.Now()
	cfg := probeConfig()
	cfg.MaxPeers = 1
	cfg.ProbeInterval = 2 * time.Minute
	e := makeEngine(t, cfg, successfulDial, func() time.Time { return now })
	if err := e.Observe(context.Background(), "N0CALL", "kiss", fixture); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	second := "AL1 198.51.100.28:49173 1"
	if err := e.Observe(context.Background(), "N1CALL", "kiss", second); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot()[0]; got.ProbeReason != "endpoint_budget_full" {
		t.Fatalf("bounded cache result = %+v", got)
	}
	now = now.Add(time.Minute)
	if err := e.Observe(context.Background(), "N1CALL", "kiss", second); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot()[0]; got.Probe != ProbeACK {
		t.Fatalf("expired cache budget result = %+v", got)
	}
}

func TestConcurrentProbeDoesNotHoldStoreLockOrOverwriteNewEndpoint(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e := makeEngine(t, probeConfig(), func(ctx context.Context, _, _ string) (net.Conn, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return successfulDial(ctx, "tcp", "")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, time.Now)
	var wg sync.WaitGroup
	wg.Add(1)
	firstDone := make(chan error, 1)
	go func() {
		defer wg.Done()
		firstDone <- e.Observe(context.Background(), "N0CALL", "kiss", fixture)
	}()
	defer wg.Wait()
	defer close(release)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("dial did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Observe(ctx, "N0CALL", "kiss", "AL1 198.51.100.28:49173 1"); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot()[0]; got.Endpoint != "198.51.100.28:49173" || got.Probe == ProbeACK {
		t.Fatalf("concurrent endpoint = %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatal("parallel probe started")
	}
	// Release and wait before asserting that the old generation cannot publish ACK.
	release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot()[0]; got.Probe == ProbeACK {
		t.Fatalf("old probe overwrote new endpoint: %+v", got)
	}
}

func TestProbeTimeoutAndCallerCancellation(t *testing.T) {
	cfg := probeConfig()
	cfg.ProbeTimeout = 20 * time.Millisecond
	e := makeEngine(t, cfg, func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, time.Now)
	if err := e.Observe(context.Background(), "N0CALL", "kiss", fixture); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("deadline error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Observe(ctx, "N1CALL", "kiss", fixture); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if len(e.Snapshot()) != 1 {
		t.Fatal("cancelled observation stored")
	}
}

func TestTCPConnectAndCloseBoundary(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The injected dialer redirects a validated documentation fixture to an isolated test listener.
	e := makeEngine(t, probeConfig(), func(ctx context.Context, network, endpoint string) (net.Conn, error) {
		if network != "tcp" || endpoint != "198.51.100.27:49173" {
			return nil, fmt.Errorf("unexpected dial target")
		}
		return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
	}, time.Now)
	if err := e.Observe(ctx, "N0CALL", "kiss", fixture); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if _, err := conn.Read(data[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("probe did not close without application data: %v", err)
	}
	if got := e.Snapshot()[0]; got.Probe != ProbeACK || got.ProbeReason != "tcp_connect_only" {
		t.Fatalf("tcp result = %+v", got)
	}
}

func TestProbeErrorReasons(t *testing.T) {
	cases := []struct {
		cause  error
		reason string
	}{
		{context.DeadlineExceeded, "timeout"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "refused"},
		{syscall.ENETUNREACH, "unreachable"},
		{syscall.EHOSTUNREACH, "unreachable"},
		{errors.New("other"), "other_tcp_error"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			e := makeEngine(t, probeConfig(), func(context.Context, string, string) (net.Conn, error) {
				return nil, tc.cause
			}, time.Now)
			if err := e.Observe(context.Background(), "N0CALL", "kiss", fixture); !errors.Is(err, ErrProbeFailed) || !errors.Is(err, tc.cause) {
				t.Fatalf("probe error = %v", err)
			}
			if got := e.Snapshot()[0]; got.Probe != ProbeNACK || got.ProbeReason != tc.reason {
				t.Fatalf("probe result = %+v", got)
			}
		})
	}
}

func TestCancellationDuringProbeIsNotNACK(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := makeEngine(t, probeConfig(), func(ctx context.Context, _, _ string) (net.Conn, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}, time.Now)
	if err := e.Observe(ctx, "N0CALL", "kiss", fixture); !errors.Is(err, context.Canceled) || errors.Is(err, ErrProbeFailed) {
		t.Fatalf("cancelled probe error = %v", err)
	}
	if got := e.Snapshot()[0]; got.Probe != ProbeNotChecked || got.ProbeReason != "cancelled" {
		t.Fatalf("cancelled probe result = %+v", got)
	}
}

func TestExpiredProbeCannotPublishResult(t *testing.T) {
	base := time.Now()
	var elapsed atomic.Int64
	clock := func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	e := makeEngine(t, probeConfig(), func(ctx context.Context, _, _ string) (net.Conn, error) {
		elapsed.Store(int64(time.Minute))
		return successfulDial(ctx, "tcp", "")
	}, clock)
	if err := e.Observe(context.Background(), "N0CALL", "kiss", fixture); err != nil {
		t.Fatal(err)
	}
	if len(e.Snapshot()) != 0 {
		t.Fatal("completed probe revived expired advertisement")
	}
}

func TestIPv6Policy(t *testing.T) {
	cfg := probeConfig()
	cfg.ProbePrefixes = []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}
	dials := 0
	e := makeEngine(t, cfg, func(ctx context.Context, network, endpoint string) (net.Conn, error) {
		dials++
		if network != "tcp" || endpoint != "[2001:db8::1]:49173" {
			t.Fatalf("IPv6 dial = %q %q", network, endpoint)
		}
		return successfulDial(ctx, network, endpoint)
	}, time.Now)
	if err := e.Observe(context.Background(), "N0CALL", "js8call", "AL1 [2001:0DB8::1]:49173 1"); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot()[0]; got.Probe != ProbeACK || got.Endpoint != "[2001:db8::1]:49173" {
		t.Fatalf("IPv6 result = %+v", got)
	}
	if err := e.Observe(context.Background(), "N1CALL", "kiss", fixture); err != nil {
		t.Fatal(err)
	}
	if dials != 1 {
		t.Fatal("IPv6 policy permitted IPv4")
	}
}

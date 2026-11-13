package core

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

const longFixture = "AL1 198.51.100.27:49173 180"

func peerNames(e *Engine) map[string]bool {
	names := make(map[string]bool)
	for _, peer := range e.Snapshot() {
		names[peer.Callsign] = true
	}
	return names
}

func observe(t *testing.T, e *Engine, source string) {
	t.Helper()
	if err := e.Observe(context.Background(), source, "kiss", longFixture); err != nil {
		t.Fatalf("observe %s: %v", source, err)
	}
}

func TestReplacePolicyIsDefault(t *testing.T) {
	e := makeEngine(t, Config{MaxPeers: 1}, successfulDial, time.Now)
	if got := e.PeerTable().Policy; got != FullPolicyReplace {
		t.Fatalf("default policy = %q", got)
	}
}

func TestReplaceEvictsHeardOnceBeforeRepeatHeard(t *testing.T) {
	now := time.Now()
	e := makeEngine(t, Config{MaxPeers: 3}, successfulDial, func() time.Time { return now })
	// A1ABC is the oldest but heard twice; B1ABC and C1ABC are heard once.
	observe(t, e, "A1ABC")
	now = now.Add(time.Second)
	observe(t, e, "B1ABC")
	now = now.Add(time.Second)
	observe(t, e, "C1ABC")
	now = now.Add(time.Second)
	observe(t, e, "A1ABC")
	now = now.Add(time.Second)
	observe(t, e, "D1ABC")
	got := peerNames(e)
	if got["B1ABC"] || !got["A1ABC"] || !got["C1ABC"] || !got["D1ABC"] || len(got) != 3 {
		t.Fatalf("after first eviction: %v", got)
	}
	// B1ABC is gone; C1ABC and D1ABC are once-heard, C1ABC is older.
	now = now.Add(time.Second)
	observe(t, e, "E1ABC")
	got = peerNames(e)
	if got["C1ABC"] || !got["A1ABC"] || !got["D1ABC"] || !got["E1ABC"] || len(got) != 3 {
		t.Fatalf("after second eviction: %v", got)
	}
	if stats := e.PeerTable(); stats.Evicted != 2 || stats.Refused != 0 || stats.Policy != FullPolicyReplace || stats.MaxPeers != 3 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestReplaceEvictsLeastRecentlyHeardWithinRepeatTier(t *testing.T) {
	now := time.Now()
	e := makeEngine(t, Config{MaxPeers: 2}, successfulDial, func() time.Time { return now })
	for _, source := range []string{"A1ABC", "B1ABC", "B1ABC", "A1ABC"} {
		observe(t, e, source)
		now = now.Add(time.Second)
	}
	// Both heard twice; B1ABC was heard least recently.
	observe(t, e, "C1ABC")
	got := peerNames(e)
	if got["B1ABC"] || !got["A1ABC"] || !got["C1ABC"] {
		t.Fatalf("repeat tier eviction: %v", got)
	}
}

func TestReplaceTreatsTransportsAsSeparateRecords(t *testing.T) {
	now := time.Now()
	e := makeEngine(t, Config{MaxPeers: 1}, successfulDial, func() time.Time { return now })
	observe(t, e, "A1ABC")
	now = now.Add(time.Second)
	if err := e.Observe(context.Background(), "A1ABC", "js8call", longFixture); err != nil {
		t.Fatal(err)
	}
	peers := e.Snapshot()
	if len(peers) != 1 || peers[0].Transport != "js8call" {
		t.Fatalf("peers = %+v", peers)
	}
}

func TestRefreshOfExistingRecordNeverEvicts(t *testing.T) {
	e := makeEngine(t, Config{MaxPeers: 2}, successfulDial, time.Now)
	observe(t, e, "A1ABC")
	observe(t, e, "B1ABC")
	observe(t, e, "A1ABC")
	if stats := e.PeerTable(); stats.Evicted != 0 || len(e.Snapshot()) != 2 {
		t.Fatalf("refresh evicted: %+v", stats)
	}
}

func TestRefusePolicyKeepsExistingRecords(t *testing.T) {
	e := makeEngine(t, Config{MaxPeers: 2, FullPolicy: FullPolicyRefuse}, successfulDial, time.Now)
	observe(t, e, "A1ABC")
	observe(t, e, "B1ABC")
	if err := e.Observe(context.Background(), "C1ABC", "kiss", longFixture); !errors.Is(err, ErrCapacity) {
		t.Fatalf("error = %v", err)
	}
	got := peerNames(e)
	if !got["A1ABC"] || !got["B1ABC"] || got["C1ABC"] {
		t.Fatalf("refuse changed the table: %v", got)
	}
	if stats := e.PeerTable(); stats.Refused != 1 || stats.Evicted != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestEvictionKeepsEndpointCooldownBounded(t *testing.T) {
	now := time.Now()
	cfg := probeConfig()
	cfg.MaxPeers = 2
	cfg.ProbeInterval = time.Hour
	e := makeEngine(t, cfg, successfulDial, func() time.Time { return now })
	for i, source := range []string{"A1ABC", "B1ABC", "C1ABC", "D1ABC", "E1ABC"} {
		endpoint := netip.AddrPortFrom(netip.AddrFrom4([4]byte{198, 51, 100, byte(10 + i)}), 49173)
		if err := e.Observe(context.Background(), source, "kiss", "AL1 "+endpoint.String()+" 180"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(11 * time.Second)
	}
	e.mu.Lock()
	endpoints, peers := len(e.endpoints), len(e.peers)
	e.mu.Unlock()
	if peers != 2 || endpoints > cfg.MaxPeers {
		t.Fatalf("peers=%d endpoints=%d exceed bound %d", peers, endpoints, cfg.MaxPeers)
	}
}

func TestProbeRunsOnOwnedWorkerNotInObserve(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	e := makeEngine(t, probeConfig(), func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		select {
		case <-release:
			return successfulDial(ctx, "tcp", "")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, time.Now)
	e.probes = make(chan probeJob, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	returned := make(chan error, 1)
	go func() { returned <- e.Observe(context.Background(), "N0CALL", "kiss", fixture) }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Observe blocked on the probe dial")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not dial")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for e.Snapshot()[0].Probe != ProbeACK {
		if time.Now().After(deadline) {
			t.Fatalf("probe result not published: %+v", e.Snapshot()[0])
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker was not joined on shutdown")
	}
}

func TestFullProbeQueueIsCountedAndReleased(t *testing.T) {
	e := makeEngine(t, probeConfig(), successfulDial, time.Now)
	e.probes = make(chan probeJob, 1)
	// No worker runs: the first job fills the queue and the second must be dropped.
	if err := e.Observe(context.Background(), "N0CALL", "kiss", fixture); err != nil {
		t.Fatal(err)
	}
	e.enqueueProbe(probeJob{key: peerKey{"N0CALL", "kiss"}, generation: 0, endpoint: netip.MustParseAddrPort("198.51.100.27:49173")})
	if got := e.PeerTable().ProbeDropped; got != 1 {
		t.Fatalf("dropped = %d", got)
	}
	e.mu.Lock()
	busy := e.probeBusy
	e.mu.Unlock()
	if busy {
		t.Fatal("dropping a job left the probe budget reserved")
	}
}

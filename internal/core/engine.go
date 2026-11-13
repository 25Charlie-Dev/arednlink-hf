// Package core stores bounded, unverified radio claims and optional TCP observations.
package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"sync"
	"syscall"
	"time"

	"arednlink-hf/internal/protocol"
)

const (
	// ProbeNotChecked means no TCP observation was made.
	ProbeNotChecked = "NOT_CHECKED"
	// ProbeACK means TCP connected; it does not establish application readiness or identity.
	ProbeACK = "ACK"
	// ProbeNACK means a TCP connection attempt failed.
	ProbeNACK = "NACK"
	// ProbeSkippedPolicy means operator destination policy denied a probe.
	ProbeSkippedPolicy = "SKIPPED_POLICY"
)

var (
	// ErrInvalidConfig identifies rejected controller configuration.
	ErrInvalidConfig = errors.New("invalid controller configuration")
	// ErrInvalidMetadata identifies unsupported transport or malformed station metadata.
	ErrInvalidMetadata = errors.New("invalid observation metadata")
	// ErrCapacity means the bounded peer store has no free record and the
	// refuse policy declined the new claim.
	ErrCapacity = errors.New("peer capacity reached")
	// ErrProbeFailed identifies a failed TCP connection observation.
	ErrProbeFailed = errors.New("tcp probe failed")
)

const (
	// FullPolicyReplace evicts one record when the store is full: records heard
	// once go before records heard repeatedly, least recently heard first within a tier.
	FullPolicyReplace = "replace"
	// FullPolicyRefuse declines new callsign/transport claims while the store is full.
	FullPolicyRefuse = "refuse"
)

// Config bounds storage and explicitly authorizes TCP probe destinations.
// FullPolicy is FullPolicyReplace when empty.
type Config struct {
	MaxPeers            int
	FullPolicy          string
	EnableProbes        bool
	ProbeTimeout        time.Duration
	ProbeInterval       time.Duration
	ProbeGlobalInterval time.Duration
	ProbePrefixes       []netip.Prefix
	ProbePorts          []uint16
}

// Peer is a copied observation; Callsign is an unverified radio source claim.
type Peer struct {
	Callsign              string    `json:"callsign"`
	Transport             string    `json:"transport"`
	Endpoint              string    `json:"endpoint"`
	IP                    string    `json:"ip,omitempty"`
	LastSeen              time.Time `json:"last_seen"`
	ExpiresAt             time.Time `json:"expires_at"`
	Probe                 string    `json:"probe"`
	ProbeReason           string    `json:"probe_reason"`
	Services              *uint16   `json:"services,omitempty"`
	ServiceNames          []string  `json:"service_names,omitempty"`
	UnassignedServiceBits uint16    `json:"unassigned_service_bits,omitempty"`
}

type peerKey struct{ source, transport string }
type record struct {
	peer       Peer
	heard      uint64
	generation uint64
	lastProbe  time.Time
	lastLog    time.Time
}

type dialContext func(context.Context, string, string) (net.Conn, error)

// Engine owns the peer store and serializes bounded TCP observations.
// One Engine owns the process probe budget.
type Engine struct {
	mu         sync.Mutex
	cfg        Config
	logger     *slog.Logger
	peers      map[peerKey]*record
	endpoints  map[netip.AddrPort]time.Time
	probeBusy  bool
	nextProbe  time.Time
	generation uint64
	dial       dialContext
	now        func() time.Time
	refused    uint64
	evicted    uint64
	dropped    uint64
	// probes is non-nil only for an engine built by New; its worker is Run.
	// Engines built without it probe inline, which keeps unit tests synchronous.
	probes chan probeJob
}

type probeJob struct {
	key        peerKey
	generation uint64
	endpoint   netip.AddrPort
}

// PeerTableStats reports how the bounded peer store has handled being full.
// Refused counts declined claims under the refuse policy, Evicted counts
// replaced records under the replace policy, and ProbeDropped counts probes
// discarded because the worker queue was full.
type PeerTableStats struct {
	Policy       string `json:"policy"`
	MaxPeers     int    `json:"max_peers"`
	Refused      uint64 `json:"refused"`
	Evicted      uint64 `json:"evicted"`
	ProbeDropped uint64 `json:"probe_dropped"`
}

// New constructs a controller with probes disabled unless explicitly authorized.
// Probes run on one worker owned by Run, behind a queue of one; Observe never dials.
func New(cfg Config, logger *slog.Logger) (*Engine, error) {
	e, err := newEngine(cfg, logger, (&net.Dialer{}).DialContext, time.Now)
	if err != nil {
		return nil, err
	}
	// The global probe budget admits one probe at a time, so a queue of one
	// holds the only job that can exist; a full queue is counted and dropped.
	e.probes = make(chan probeJob, 1)
	return e, nil
}

// Run is the probe worker for an engine built by New. It returns nil after
// ctx is cancelled, discarding any queued probe. It must be started once.
func (e *Engine) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			e.discardQueued()
			return nil
		case job := <-e.probes:
			// The probe logs its own outcome; the returned error only mirrors it.
			if err := e.probe(ctx, job.key, job.generation, job.endpoint); err != nil {
				e.logger.Debug("probe worker result", "event", "probe_worker", "error", err)
			}
		}
	}
}

func (e *Engine) discardQueued() {
	for {
		select {
		case job := <-e.probes:
			e.finishProbe(job.key, job.generation, job.endpoint, ProbeNotChecked, "cancelled")
		default:
			return
		}
	}
}

func (e *Engine) enqueueProbe(job probeJob) {
	select {
	case e.probes <- job:
	default:
		e.mu.Lock()
		e.dropped++
		e.mu.Unlock()
		e.finishProbe(job.key, job.generation, job.endpoint, ProbeNotChecked, "queue_full")
	}
}

// PeerTable returns capacity-handling counters for the status snapshot.
func (e *Engine) PeerTable() PeerTableStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return PeerTableStats{Policy: e.cfg.FullPolicy, MaxPeers: e.cfg.MaxPeers,
		Refused: e.refused, Evicted: e.evicted, ProbeDropped: e.dropped}
}

func newEngine(cfg Config, logger *slog.Logger, dial dialContext, now func() time.Time) (*Engine, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Engine{
		cfg: cfg, logger: logger, dial: dial, now: now,
		peers:     make(map[peerKey]*record, cfg.MaxPeers),
		endpoints: make(map[netip.AddrPort]time.Time, cfg.MaxPeers),
	}, nil
}

// Observe validates a claim, refreshes advertisement validity, and optionally probes TCP.
func (e *Engine) Observe(ctx context.Context, source, transport, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !protocol.ValidCallsign(source) || (transport != "kiss" && transport != "js8call") {
		return ErrInvalidMetadata
	}
	ad, err := protocol.Parse(payload)
	if err != nil {
		return err
	}
	key := peerKey{source, transport}
	generation, probe, err := e.observeClaim(key, ad)
	if err != nil || !probe {
		return err
	}
	if e.probes != nil {
		e.enqueueProbe(probeJob{key: key, generation: generation, endpoint: ad.Endpoint})
		return nil
	}
	return e.probe(ctx, key, generation, ad.Endpoint)
}

func (e *Engine) observeClaim(key peerKey, ad protocol.Advertisement) (uint64, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.prune(now)
	r, exists := e.peers[key]
	if !exists && len(e.peers) >= e.cfg.MaxPeers {
		if e.cfg.FullPolicy == FullPolicyRefuse {
			e.refused++
			return 0, false, ErrCapacity
		}
		e.evictOne()
	}
	if !exists {
		r = &record{peer: Peer{Callsign: key.source, Transport: key.transport}}
		e.peers[key] = r
	}
	r.heard++
	endpoint, ip := "", ""
	if ad.Address.IsValid() {
		ip = ad.Address.String()
	} else {
		endpoint = ad.Endpoint.String()
	}
	if r.peer.Endpoint != endpoint || r.peer.IP != ip {
		e.generation++
		r.generation = e.generation
		r.peer.Endpoint, r.peer.IP = endpoint, ip
		r.lastProbe = time.Time{}
		r.peer.Probe, r.peer.ProbeReason = ProbeNotChecked, "not_attempted"
	}
	r.peer.LastSeen, r.peer.ExpiresAt = now, now.Add(ad.ValidFor)
	if ad.ServicesPresent {
		services := ad.Services
		r.peer.Services = &services
		r.peer.ServiceNames = protocol.ServiceNames(services)
		r.peer.UnassignedServiceBits = protocol.UnassignedServiceBits(services)
	} else {
		r.peer.Services = nil
		r.peer.ServiceNames = nil
		r.peer.UnassignedServiceBits = 0
	}
	if r.lastLog.IsZero() || now.Sub(r.lastLog) >= 10*time.Second {
		e.logger.Debug("validated radio claim", "event", "claim_observed", "callsign", key.source,
			"transport", key.transport, "endpoint", r.peer.Endpoint, "identity", "unverified")
		r.lastLog = now
	}
	if ad.Address.IsValid() {
		r.peer.Probe, r.peer.ProbeReason = ProbeNotChecked, "ip_only"
		return r.generation, false, nil
	}
	probe := e.reserveProbe(r, ad.Endpoint, now)
	return r.generation, probe, nil
}

// evictOne removes the record heard exactly once with the oldest LastSeen, or,
// when every record was heard repeatedly, the one with the oldest LastSeen.
// Ties break on callsign then transport so the choice is deterministic.
// The caller holds e.mu and the store is non-empty.
//
// The per-endpoint probe cooldown is deliberately kept: it is keyed by
// destination, not by record, expires on its own and is bounded by MaxPeers,
// and dropping it on eviction would let churning claims defeat probe pacing.
func (e *Engine) evictOne() {
	var victim peerKey
	var best *record
	for key, r := range e.peers {
		if best == nil || evictBefore(key, r, victim, best) {
			victim, best = key, r
		}
	}
	if best == nil {
		return
	}
	delete(e.peers, victim)
	e.evicted++
	e.logger.Debug("evicted peer record", "event", "peer_evicted", "callsign", victim.source,
		"transport", victim.transport, "heard", best.heard)
}

// evictBefore reports whether candidate a should be evicted ahead of b.
func evictBefore(ak peerKey, a *record, bk peerKey, b *record) bool {
	aOnce, bOnce := a.heard <= 1, b.heard <= 1
	if aOnce != bOnce {
		return aOnce
	}
	if !a.peer.LastSeen.Equal(b.peer.LastSeen) {
		return a.peer.LastSeen.Before(b.peer.LastSeen)
	}
	if ak.source != bk.source {
		return ak.source < bk.source
	}
	return ak.transport < bk.transport
}

func (e *Engine) reserveProbe(r *record, endpoint netip.AddrPort, now time.Time) bool {
	if !e.cfg.EnableProbes {
		r.peer.Probe, r.peer.ProbeReason = ProbeNotChecked, "disabled"
		return false
	}
	if !e.allowed(endpoint) {
		r.peer.Probe, r.peer.ProbeReason = ProbeSkippedPolicy, "destination_not_allowed"
		return false
	}
	reason := e.budgetReason(r, endpoint, now)
	if reason != "" {
		if r.peer.Probe != ProbeACK && r.peer.Probe != ProbeNACK {
			r.peer.Probe, r.peer.ProbeReason = ProbeNotChecked, reason
		}
		return false
	}
	e.probeBusy = true
	e.nextProbe = now.Add(e.cfg.ProbeGlobalInterval)
	e.endpoints[endpoint] = now.Add(e.cfg.ProbeInterval)
	r.lastProbe = now
	r.peer.Probe, r.peer.ProbeReason = ProbeNotChecked, "in_progress"
	return true
}

func (e *Engine) budgetReason(r *record, endpoint netip.AddrPort, now time.Time) string {
	if !r.lastProbe.IsZero() && now.Sub(r.lastProbe) < e.cfg.ProbeInterval {
		return "peer_cooldown"
	}
	if until, exists := e.endpoints[endpoint]; exists && now.Before(until) {
		return "endpoint_cooldown"
	}
	if e.probeBusy {
		return "global_busy"
	}
	if now.Before(e.nextProbe) {
		return "global_cooldown"
	}
	if len(e.endpoints) >= e.cfg.MaxPeers {
		return "endpoint_budget_full"
	}
	return ""
}

func (e *Engine) probe(ctx context.Context, key peerKey, generation uint64, endpoint netip.AddrPort) error {
	probeCtx, cancel := context.WithTimeout(ctx, e.cfg.ProbeTimeout)
	defer cancel()
	conn, err := e.dial(probeCtx, "tcp", endpoint.String())
	if err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			e.logger.Debug("probe socket close failed", "event", "probe_close", "error", closeErr)
		}
	}
	state, reason, resultErr := probeOutcome(ctx, err)
	e.finishProbe(key, generation, endpoint, state, reason)
	e.logger.Info("tcp probe observation", "event", "probe_result", "callsign", key.source,
		"transport", key.transport, "endpoint", endpoint.String(), "probe", state, "reason", reason)
	return resultErr
}

func probeOutcome(ctx context.Context, err error) (string, string, error) {
	if callerErr := ctx.Err(); callerErr != nil {
		return ProbeNotChecked, "cancelled", callerErr
	}
	if err == nil {
		return ProbeACK, "tcp_connect_only", nil
	}
	reason := "other_tcp_error"
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		reason = "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		reason = "refused"
	case errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH):
		reason = "unreachable"
	}
	return ProbeNACK, reason, fmt.Errorf("%w: %w", ErrProbeFailed, err)
}

func (e *Engine) finishProbe(key peerKey, generation uint64, endpoint netip.AddrPort, state, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.probeBusy = false
	r, exists := e.peers[key]
	if exists && r.generation == generation && r.peer.Endpoint == endpoint.String() && e.now().Before(r.peer.ExpiresAt) {
		r.peer.Probe, r.peer.ProbeReason = state, reason
	}
}

// Snapshot returns an immutable, sorted copy of currently unexpired observations.
func (e *Engine) Snapshot() []Peer {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.prune(e.now())
	peers := make([]Peer, 0, len(e.peers))
	for _, r := range e.peers {
		peer := r.peer
		if r.peer.Services != nil {
			services := *r.peer.Services
			peer.Services = &services
		}
		peer.ServiceNames = append([]string(nil), r.peer.ServiceNames...)
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Callsign != peers[j].Callsign {
			return peers[i].Callsign < peers[j].Callsign
		}
		return peers[i].Transport < peers[j].Transport
	})
	return peers
}

func (e *Engine) prune(now time.Time) {
	for key, r := range e.peers {
		if !now.Before(r.peer.ExpiresAt) {
			delete(e.peers, key)
		}
	}
	for endpoint, until := range e.endpoints {
		if !now.Before(until) {
			delete(e.endpoints, endpoint)
		}
	}
}

func (e *Engine) allowed(endpoint netip.AddrPort) bool {
	portAllowed := false
	for _, port := range e.cfg.ProbePorts {
		if port == endpoint.Port() {
			portAllowed = true
			break
		}
	}
	if !portAllowed {
		return false
	}
	for _, prefix := range e.cfg.ProbePrefixes {
		if prefix.Contains(endpoint.Addr()) {
			return true
		}
	}
	return false
}

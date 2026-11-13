// Package status provides HTTP-free radio snapshots.
package status

import (
	"arednlink-hf/internal/protocol"
	"arednlink-hf/internal/resourceguard"
	"sync"
	"time"
)

// RadioStatus records adapter lifecycle without equating TCP with Yggdrasil.
type RadioStatus struct {
	adapters      []*RadioStatus
	resourceGuard *resourceguard.Monitor
	mu            sync.RWMutex
	kind          string
	state         string
	txEnabled     bool
	txState       string
	lastQueued    time.Time
	lastWritten   time.Time
	pendingTXID   uint64
	apiRefusal    string
	now           func() time.Time
	lastHeard     time.Time
	recent        [5]receivedStation
	count         int
}

// SetAdapters attaches independently owned adapter states in configured order.
func (s *RadioStatus) SetAdapters(adapters ...*RadioStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapters = append([]*RadioStatus(nil), adapters...)
}

// NewRadioStatus starts an enabled adapter in a degraded connecting state.
func NewRadioStatus(kind string, transmitEnabled ...bool) *RadioStatus {
	enabled := len(transmitEnabled) > 0 && transmitEnabled[0]
	return newRadioStatus(kind, time.Now, enabled)
}

func newRadioStatus(kind string, now func() time.Time, transmitEnabled ...bool) *RadioStatus {
	state := "connecting"
	if kind == "none" {
		state = "disabled"
	}
	enabled := len(transmitEnabled) > 0 && transmitEnabled[0]
	txState := "disabled"
	if enabled {
		txState = "scheduled"
	}
	return &RadioStatus{kind: kind, state: state, txEnabled: enabled, txState: txState, now: now}
}

// Set updates the last adapter lifecycle state.
func (s *RadioStatus) Set(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

func (s *RadioStatus) SetTransmit(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txState = state
	if state == "queued" {
		s.lastQueued = s.now()
	}
	if state == "written" {
		s.lastWritten = s.now()
	}
}

func (s *RadioStatus) SetTransmitRequest(requestID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingTXID = requestID
	s.apiRefusal = ""
}

func (s *RadioStatus) SetTransmitRefused(requestID uint64, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if requestID != s.pendingTXID {
		return
	}
	// Fork-specific: only a reviewed JS8Call fork is known to send this reply.
	s.txState = "api_refused"
	if len(reason) > 80 {
		reason = reason[:80]
	}
	s.apiRefusal = reason
}

// ObserveReceive records fresh activity and up to five unique source claims.
// An empty callsign updates reception time without adding a station.
func (s *RadioStatus) ObserveReceive(callsign string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kind != "js8call" || callsign != "" && !protocol.ValidCallsign(callsign) {
		return
	}
	s.lastHeard = s.now()
	if callsign == "" {
		return
	}
	index := s.count
	for i := 0; i < s.count; i++ {
		if s.recent[i].callsign == callsign {
			index = i
			break
		}
	}
	if index == s.count && s.count < len(s.recent) {
		s.count++
	}
	index = min(index, len(s.recent)-1)
	copy(s.recent[1:index+1], s.recent[:index])
	s.recent[0] = receivedStation{callsign: callsign, lastHeard: s.lastHeard}
}

// SetResourceGuard attaches the owned controller resource monitor.
func (s *RadioStatus) SetResourceGuard(guard *resourceguard.Monitor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resourceGuard = guard
}

func (s *RadioStatus) Snapshot() RadioView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	view := RadioView{Kind: s.kind, State: s.state, Healthy: s.state == "disabled" || s.state == "connected"}
	if len(s.adapters) > 0 {
		view.Adapters = make([]RadioView, 0, len(s.adapters))
		view.Healthy = true
		for _, adapter := range s.adapters {
			item := adapter.Snapshot()
			view.Adapters = append(view.Adapters, item)
			if !item.Healthy {
				view.Healthy = false
			}
		}
		if view.Healthy {
			view.State = "connected"
		} else {
			view.State = "degraded"
		}
	}
	if s.txEnabled {
		view.TX = s.transmitSnapshot()
	}
	if s.resourceGuard != nil {
		guard := s.resourceGuard.Snapshot()
		view.ResourceGuard = &guard
		if guard.State != "healthy" && guard.State != "disabled" {
			view.Healthy = false
			if len(s.adapters) > 0 {
				view.State = "degraded"
			}
		}
	}
	if s.kind == "js8call" {
		view.RX = s.receiveSnapshot()
	}
	return view
}

func (s *RadioStatus) transmitSnapshot() *transmitView {
	view := &transmitView{State: s.txState, AirStatus: "unknown"}
	if s.apiRefusal != "" {
		refusal := s.apiRefusal
		view.APIRefusal = &refusal
	}
	if s.txState == "api_refused" {
		view.APIRefusalSource = refusalSourceFork
	}
	if !s.lastQueued.IsZero() {
		stamp := s.lastQueued.UTC().Format(time.RFC3339)
		view.LastQueued = &stamp
	}
	if !s.lastWritten.IsZero() {
		stamp := s.lastWritten.UTC().Format(time.RFC3339)
		view.LastWritten = &stamp
	}
	return view
}

func (s *RadioStatus) receiveSnapshot() *receiveView {
	view := &receiveView{RecentCallsigns: make([]stationView, 0, s.count)}
	if !s.lastHeard.IsZero() {
		stamp := s.lastHeard.UTC().Format(time.RFC3339)
		age := max(int64(0), int64(s.now().Sub(s.lastHeard)/time.Second))
		view.LastHeard, view.AgeSeconds = &stamp, &age
	}
	for i := 0; i < s.count; i++ {
		station := s.recent[i]
		view.RecentCallsigns = append(view.RecentCallsigns, stationView{
			Callsign: station.callsign, LastHeard: station.lastHeard.UTC().Format(time.RFC3339)})
	}
	return view
}

type receivedStation struct {
	callsign  string
	lastHeard time.Time
}

type receiveView struct {
	LastHeard       *string       `json:"last_heard"`
	AgeSeconds      *int64        `json:"age_seconds"`
	RecentCallsigns []stationView `json:"recent_callsigns"`
}

type stationView struct {
	Callsign  string `json:"callsign"`
	LastHeard string `json:"last_heard"`
}

// refusalSourceFork labels api_refused as observed only on a JS8Call fork's
// reply shape, not a documented upstream JS8Call behavior.
const refusalSourceFork = "js8call_fork_specific"

type transmitView struct {
	State            string  `json:"state"`
	LastQueued       *string `json:"last_queued,omitempty"`
	LastWritten      *string `json:"last_written,omitempty"`
	APIRefusal       *string `json:"api_refusal,omitempty"`
	APIRefusalSource string  `json:"api_refusal_source,omitempty"`
	AirStatus        string  `json:"air_status"`
}

type RadioView struct {
	Adapters      []RadioView             `json:"adapters,omitempty"`
	ResourceGuard *resourceguard.Snapshot `json:"resource_guard,omitempty"`
	Kind          string                  `json:"kind"`
	State         string                  `json:"state"`
	Healthy       bool                    `json:"healthy"`
	RX            *receiveView            `json:"rx,omitempty"`
	TX            *transmitView           `json:"tx,omitempty"`
}

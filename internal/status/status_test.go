package status

import (
	"arednlink-hf/internal/resourceguard"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAggregateHealthCopiesAdaptersAndGuard(t *testing.T) {
	kiss := NewRadioStatus("kiss")
	js8 := NewRadioStatus("js8call")
	parent := NewRadioStatus("multiple")
	parent.SetAdapters(kiss, js8)
	kiss.Set("connected")
	js8.Set("connected")
	js8.ObserveReceive("W1AW")
	view := parent.Snapshot()
	if !view.Healthy || view.State != "connected" || len(view.Adapters) != 2 {
		t.Fatalf("aggregate lost healthy adapters: %+v", view)
	}
	view.Adapters[0].Kind = "mutated"
	view.Adapters[1].RX.RecentCallsigns[0].Callsign = "mutated"
	view.Adapters[1].State = "mutated"
	view = parent.Snapshot()
	if view.Adapters[0].Kind != "kiss" || view.Adapters[1].RX.RecentCallsigns[0].Callsign != "W1AW" || view.Adapters[1].State != "connected" {
		t.Fatalf("snapshot aliases adapter state: %+v", view)
	}
	kiss.Set("backoff")
	view = parent.Snapshot()
	if view.Healthy || view.State != "degraded" || view.Adapters[1].State != "connected" {
		t.Fatalf("one failure hid healthy adapter: %+v", view)
	}
	kiss.Set("connected")
	guardCfg := resourceguard.Defaults()
	guardCfg.Enabled = true
	guard, err := resourceguard.New(guardCfg)
	if err != nil {
		t.Fatal(err)
	}
	parent.SetResourceGuard(guard)
	view = parent.Snapshot()
	if view.Healthy || view.State != "degraded" || view.ResourceGuard == nil || view.ResourceGuard.State != "starting" {
		t.Fatalf("guard health was hidden: %+v", view)
	}
}

func TestReceiveNeverHeardAndUnsupported(t *testing.T) {
	for _, kind := range []string{"js8call", "kiss", "none"} {
		state := NewRadioStatus(kind)
		view := state.Snapshot()
		if kind != "js8call" {
			state.ObserveReceive("W1AW")
			if state.Snapshot().RX != nil {
				t.Fatalf("unsupported history for %s", kind)
			}
			continue
		}
		if view.RX == nil || view.RX.LastHeard != nil || view.RX.AgeSeconds != nil || view.RX.RecentCallsigns == nil || len(view.RX.RecentCallsigns) != 0 {
			t.Fatalf("startup marked heard: %#v", view.RX)
		}
	}
}

func TestTransmitStatusSeparatesQueuedWrittenAndAcceptance(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := newRadioStatus("kiss", func() time.Time { return now }, true)
	if state.Snapshot().TX == nil || state.Snapshot().TX.State != "scheduled" {
		t.Fatal("enabled beacon was not shown as scheduled")
	}
	state.SetTransmitRequest(7)
	state.SetTransmit("queued")
	now = now.Add(time.Second)
	view := state.Snapshot().TX
	if view.LastQueued == nil || view.LastWritten != nil || view.AirStatus != "unknown" {
		t.Fatalf("queued status conflated with delivery: %#v", view)
	}
	state.SetTransmit("written")
	view = state.Snapshot().TX
	if view.LastQueued == nil || view.LastWritten == nil || *view.LastWritten != "2026-01-02T03:04:06Z" || view.AirStatus != "unknown" {
		t.Fatalf("written status claimed acceptance or airtime: %#v", view)
	}
	state.SetTransmitRefused(6, "stale")
	if state.Snapshot().TX.State != "written" {
		t.Fatal("stale API response changed transmission state")
	}
	state.SetTransmitRefused(7, "auto_route_active")
	view = state.Snapshot().TX
	if view.State != "api_refused" || view.APIRefusal == nil || *view.APIRefusal != "auto_route_active" || view.APIRefusalSource != "js8call_fork_specific" || view.AirStatus != "unknown" {
		t.Fatalf("API refusal conflated with acceptance or airtime: %#v", view)
	}
	if NewRadioStatus("kiss").Snapshot().TX != nil {
		t.Fatal("disabled beacon exposed TX state")
	}
}

func TestReceiveHistoryUniqueMRUBoundedAndCopied(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("offset", 3600))
	state := newRadioStatus("js8call", func() time.Time { return now })
	for _, call := range []string{"W1AW", "W2AW", "W3AW", "W4AW", "W5AW", "W2AW", "W6AW"} {
		state.ObserveReceive(call)
		now = now.Add(time.Second)
	}
	view := state.Snapshot()
	var calls []string
	for _, station := range view.RX.RecentCallsigns {
		calls = append(calls, station.Callsign)
	}
	if strings.Join(calls, ",") != "W6AW,W2AW,W5AW,W4AW,W3AW" || *view.RX.LastHeard != "2026-01-02T02:04:11Z" || *view.RX.AgeSeconds != 1 {
		t.Fatalf("unexpected MRU: %#v, age=%d", view.RX, *view.RX.AgeSeconds)
	}
	view.RX.RecentCallsigns[0].Callsign = "MUTATED"
	*view.RX.LastHeard = "MUTATED"
	*view.RX.AgeSeconds = -100
	now = now.Add(10 * time.Second)
	state.ObserveReceive("")
	view = state.Snapshot()
	if view.RX.RecentCallsigns[0].Callsign != "W6AW" || view.RX.RecentCallsigns[0].LastHeard != "2026-01-02T02:04:11Z" || *view.RX.LastHeard != "2026-01-02T02:04:22Z" || *view.RX.AgeSeconds != 0 {
		t.Fatalf("snapshot alias or anonymous update changed station: %#v", view.RX)
	}
	now = now.Add(-time.Hour)
	if *state.Snapshot().RX.AgeSeconds != 0 {
		t.Fatal("negative age")
	}
}

func TestReceiveConcurrentSnapshotsAndUpdates(t *testing.T) {
	state := NewRadioStatus("js8call")
	var joined sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		joined.Add(1)
		go func() {
			defer joined.Done()
			for i := 0; i < 1000; i++ {
				state.ObserveReceive("W1AW")
				state.ObserveReceive("")
				state.Set("connected")
				view := state.Snapshot()
				view.RX.RecentCallsigns[0].Callsign = "COPY"
			}
		}()
	}
	joined.Wait()
	if state.Snapshot().RX.RecentCallsigns[0].Callsign != "W1AW" {
		t.Fatal("snapshot leaked internal history")
	}
}

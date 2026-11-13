package core

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"arednlink-hf/internal/protocol"
)

func TestIPOnlyNeverProbesOrInventsPort(t *testing.T) {
	for _, ip := range []string{"198.51.100.27", "2001:db8::1"} {
		t.Run(ip, func(t *testing.T) {
			cfg := probeConfig()
			cfg.ProbePrefixes = append(cfg.ProbePrefixes, netip.MustParsePrefix("2001:db8::/32"))
			dials := 0
			e := makeEngine(t, cfg, func(ctx context.Context, network, address string) (net.Conn, error) {
				dials++
				return successfulDial(ctx, network, address)
			}, time.Now)
			ad := protocol.Advertisement{Address: netip.MustParseAddr(ip), ValidFor: time.Minute, Services: protocol.ServiceSignalProxy}
			wire, err := protocol.EncodeCompact(ad)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Observe(context.Background(), "N0CALL", "kiss", wire); err != nil {
				t.Fatal(err)
			}
			peer := e.Snapshot()[0]
			if dials != 0 || peer.IP != ip || peer.Endpoint != "" || peer.Probe != ProbeNotChecked || peer.ProbeReason != "ip_only" || peer.Services == nil || *peer.Services != protocol.ServiceSignalProxy {
				t.Fatalf("peer = %+v, dials = %d", peer, dials)
			}
			if err := e.Observe(context.Background(), "N0CALL", "kiss", fixture); err != nil {
				t.Fatal(err)
			}
			peer = e.Snapshot()[0]
			if peer.IP != "" || peer.Endpoint != "198.51.100.27:49173" || dials != 1 {
				t.Fatalf("legacy transition = %+v, dials = %d", peer, dials)
			}
		})
	}
}

package main

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
	"time"

	"arednlink-hf/internal/protocol"
)

func TestCodecAL4PreservesIPOnlyClaims(t *testing.T) {
	for _, ip := range []string{"198.51.100.27", "2001:db8::1"} {
		wire, err := protocol.EncodeCompact(protocol.Advertisement{Address: netip.MustParseAddr(ip), ValidFor: 90 * time.Minute, Services: protocol.ServiceMatrixFederation})
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"-encode", "-decode"} {
			var stdout, stderr bytes.Buffer
			if err := run([]string{flag, wire}, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(stdout.String()); got != wire {
				t.Fatalf("%s %s = %q", flag, ip, got)
			}
		}
	}
}

func TestCodecFlagsRunWithoutStartingDaemon(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-encode", "AL1 198.51.100.27:49173 90"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "AL2 PFFBXVIDLIKO" {
		t.Fatalf("encoded output = %q", got)
	}
	stdout.Reset()
	if err := run([]string{"-decode", "AL2 PFFBXVIDLIKO"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "AL1 198.51.100.27:49173 90" {
		t.Fatalf("decoded output = %q", got)
	}
	stdout.Reset()
	if err := run([]string{"-encode", "AL3 PFFBXVIDLIKO 00V"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "AL3 PFFBXVIDLIKO 00V" {
		t.Fatalf("encoded AL3 output lost service flags: %q", got)
	}
	stdout.Reset()
	if err := run([]string{"-decode", "AL3 PFFBXVIDLIKO 00V"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "AL3 PFFBXVIDLIKO 00V" {
		t.Fatalf("decoded AL3 output lost service flags: %q", got)
	}
}

func TestCodecFlagsRequireExclusiveNonemptyInput(t *testing.T) {
	for _, args := range [][]string{
		{"-encode", ""}, {"-decode", ""}, {"-encode", "AL1 198.51.100.27:49173 90", "-decode", "AL2 PFFBXVIDLIKO"},
		{"-encode", "AL1 198.51.100.27:49173 90", "-version"}, {"-decode", "AL2 PFFBXVIDLIKO", "-config="},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err == nil {
			t.Errorf("run(%q) succeeded", args)
		}
		if stdout.Len() != 0 {
			t.Errorf("run(%q) wrote output %q", args, stdout.String())
		}
	}
}

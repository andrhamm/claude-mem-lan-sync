package discover

import (
	"net/netip"
	"strings"
	"testing"
)

// The hub id is the routing partition a client sends in X-User-Id. Broadcasting
// it to the whole segment would leave only the key between a neighbour and every
// memory on the hub.
func TestTXTRecordsCarryNoIdentifyingData(t *testing.T) {
	cfg := Config{Enabled: true, InstanceName: "cmemlan-abc123", Port: 8787}
	recs := txtRecords(cfg)

	if recs["v"] != "1" {
		t.Errorf("version record = %q, want 1", recs["v"])
	}
	for k, v := range recs {
		if strings.Contains(strings.ToLower(k), "hub") || strings.Contains(strings.ToLower(k), "id") {
			t.Errorf("TXT record %q=%q looks like it identifies the hub", k, v)
		}
	}
	if len(recs) != 1 {
		t.Errorf("TXT carries %d records; keep the advertisement minimal: %v", len(recs), recs)
	}

	for _, s := range TXTStrings(cfg) {
		if !strings.Contains(s, "=") {
			t.Errorf("TXT string %q is not in k=v form", s)
		}
	}
}

// Announcing a loopback-bound hub is noise, and worse, a misleading signal that
// this machine hosts something joinable.
func TestShouldAdvertise(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"disabled", Config{Enabled: false, BindAddr: netip.MustParseAddr("192.168.1.5")}, false},
		{"loopback v4", Config{Enabled: true, BindAddr: netip.MustParseAddr("127.0.0.1")}, false},
		{"loopback v6", Config{Enabled: true, BindAddr: netip.MustParseAddr("::1")}, false},
		{"lan", Config{Enabled: true, BindAddr: netip.MustParseAddr("192.168.1.5")}, true},
		{"tailscale", Config{Enabled: true, BindAddr: netip.MustParseAddr("100.80.1.1")}, true},
		{"invalid addr", Config{Enabled: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldAdvertise(tc.cfg); got != tc.want {
				t.Errorf("ShouldAdvertise = %v, want %v", got, tc.want)
			}
		})
	}
}

// The default label must not be the hostname, which usually contains a name.
func TestRandomInstanceName(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		name, err := RandomInstanceName()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(name, "cmemlan-") {
			t.Fatalf("name %q lacks the expected prefix", name)
		}
		if seen[name] {
			t.Fatalf("duplicate label %q", name)
		}
		seen[name] = true
	}
}

func TestResolveNameHonoursOverride(t *testing.T) {
	got, err := ResolveName(Config{InstanceName: "my-hub"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "my-hub" {
		t.Fatalf("ResolveName = %q", got)
	}

	got, err = ResolveName(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || !strings.HasPrefix(got, "cmemlan-") {
		t.Fatalf("ResolveName fallback = %q", got)
	}
}

// A disabled advertisement must be a no-op rather than an error, so callers can
// pass the flag through without branching.
func TestAdvertiseDisabledIsNoOp(t *testing.T) {
	if err := Advertise(t.Context(), Config{Enabled: false}); err != nil {
		t.Fatalf("disabled Advertise returned %v", err)
	}
	if err := Advertise(t.Context(), Config{
		Enabled:  true,
		BindAddr: netip.MustParseAddr("127.0.0.1"),
		Port:     8787,
	}); err != nil {
		t.Fatalf("loopback Advertise returned %v", err)
	}
}

func TestFoundAddr(t *testing.T) {
	f := Found{Name: "cmemlan-abc", Host: "192.168.1.5", Port: 8787}
	if got := f.Addr(); got != "http://192.168.1.5:8787" {
		t.Fatalf("Addr = %q", got)
	}
}

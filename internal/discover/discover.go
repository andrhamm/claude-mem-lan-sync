// Package discover advertises and finds hubs on the local link.
//
// mDNS is multicast, so it does not cross Tailscale or route between subnets.
// Same-LAN discovery works; anything else needs an explicit hostname. That
// limitation is documented rather than worked around.
package discover

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net/netip"
	"strconv"
	"time"
)

// ServiceType is the DNS-SD service type for a cmemlan hub.
const ServiceType = "_cmemlan._tcp"

// Config describes an advertisement.
type Config struct {
	Enabled bool
	// InstanceName defaults to a random label rather than the hostname, which
	// would otherwise tell every machine on the network whose laptop this is.
	InstanceName string
	Port         int
	BindAddr     netip.Addr
}

// Found is a discovered hub.
type Found struct {
	Name string
	Host string
	Port int
}

// Addr renders a URL for connect.
func (f Found) Addr() string {
	return fmt.Sprintf("http://%s:%d", f.Host, f.Port)
}

// txtRecords builds the advertised TXT map.
//
// The hub id is deliberately absent. It is the routing partition id that a
// client sends in X-User-Id, so broadcasting it to the whole segment would mean
// an attacker needs only the key. Nothing here identifies the machine or its
// owner either.
func txtRecords(cfg Config) map[string]string {
	return map[string]string{
		"v": "1",
	}
}

// ShouldAdvertise reports whether an advertisement makes sense.
//
// A loopback-bound hub is unreachable from anywhere else, so announcing it would
// be pure noise — and a misleading signal that this machine hosts something
// joinable.
func ShouldAdvertise(cfg Config) bool {
	if !cfg.Enabled {
		return false
	}
	if !cfg.BindAddr.IsValid() {
		return false
	}
	return !cfg.BindAddr.IsLoopback()
}

// RandomInstanceName returns a short, non-identifying label.
func RandomInstanceName() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 8)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return "cmemlan-" + string(out), nil
}

// ResolveName picks the instance name to advertise.
func ResolveName(cfg Config) (string, error) {
	if cfg.InstanceName != "" {
		return cfg.InstanceName, nil
	}
	return RandomInstanceName()
}

// TXTStrings renders the TXT map in the "k=v" form the wire format uses.
func TXTStrings(cfg Config) []string {
	recs := txtRecords(cfg)
	out := make([]string, 0, len(recs))
	for k, v := range recs {
		out = append(out, k+"="+v)
	}
	return out
}

// PortString is a helper for building service entries.
func PortString(p int) string { return strconv.Itoa(p) }

// BrowseTimeout is how long connect waits for answers.
const BrowseTimeout = 3 * time.Second

// ctxWithTimeout is a small helper so callers do not repeat this.
func ctxWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = BrowseTimeout
	}
	return context.WithTimeout(parent, d)
}

package discover

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"time"

	"github.com/brutella/dnssd"
)

// Advertise announces this hub on the local link until ctx is cancelled.
//
// It returns nil without doing anything when advertising is disabled or the hub
// is bound to loopback.
func Advertise(ctx context.Context, cfg Config) error {
	if !ShouldAdvertise(cfg) {
		return nil
	}

	name, err := ResolveName(cfg)
	if err != nil {
		return err
	}

	service, err := dnssd.NewService(dnssd.Config{
		Name: name,
		Type: ServiceType,
		Port: cfg.Port,
		Text: txtRecords(cfg),
	})
	if err != nil {
		return fmt.Errorf("discover: building the service entry: %w", err)
	}

	responder, err := dnssd.NewResponder()
	if err != nil {
		return fmt.Errorf("discover: creating the mDNS responder: %w", err)
	}
	if _, err := responder.Add(service); err != nil {
		return fmt.Errorf("discover: registering the service: %w", err)
	}

	// Respond returns when ctx is cancelled, sending a goodbye packet.
	return responder.Respond(ctx)
}

// Browse looks for hubs on the local link.
//
// Multicast behaviour on Windows is unreliable enough that `connect` requires an
// explicit URL there rather than pretending discovery works.
func Browse(ctx context.Context, timeout time.Duration) ([]Found, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("discover: mDNS discovery is not supported on Windows; pass the hub URL explicitly")
	}

	ctx, cancel := ctxWithTimeout(ctx, timeout)
	defer cancel()

	seen := map[string]Found{}
	add := func(e dnssd.BrowseEntry) {
		host := e.Host
		if len(e.IPs) > 0 {
			host = pickAddress(e.IPs)
		}
		if host == "" {
			return
		}
		key := fmt.Sprintf("%s:%d", host, e.Port)
		seen[key] = Found{Name: e.Name, Host: host, Port: e.Port}
	}

	err := dnssd.LookupType(ctx, ServiceType+".local.", add, func(dnssd.BrowseEntry) {})
	if err != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("discover: browsing: %w", err)
	}

	out := make([]Found, 0, len(seen))
	for _, f := range seen {
		out = append(out, f)
	}
	return out, nil
}

// pickAddress prefers IPv4, which is what a LAN user will recognise.
func pickAddress(ips []net.IP) string {
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	if len(ips) > 0 {
		return ips[0].String()
	}
	return ""
}

package hub

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// DefaultAllowedCIDRs are the ranges a LAN hub should ever serve.
//
// RFC1918, CGNAT (which is where Tailscale lives), link-local, and loopback.
var DefaultAllowedCIDRs = []string{
	"127.0.0.0/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"100.64.0.0/10", // CGNAT / Tailscale
	"169.254.0.0/16", "fe80::/10",
	"fc00::/7", // unique local addresses
}

// ErrPublicBind is returned when a bind address would expose the hub.
var ErrPublicBind = errors.New(
	"refusing to bind a public or wildcard address: this hub serves plaintext memory over HTTP")

// ClassifyBind validates a bind address.
//
// String matching on "0.0.0.0" is not enough: "::", "[::]", and a bare ":8787"
// are all wildcards too, and a hostname can resolve to a globally routable
// SLAAC address that home routers firewall but cafe networks do not. Everything
// is classified through net/netip instead.
func ClassifyBind(bind string, allowPublic bool) (netip.AddrPort, error) {
	if bind == "" {
		return netip.AddrPort{}, errors.New("empty bind address")
	}

	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("bind address %q must be host:port: %w", bind, err)
	}

	// A bare ":8787" binds the dual-stack wildcard.
	if strings.TrimSpace(host) == "" {
		if !allowPublic {
			return netip.AddrPort{}, fmt.Errorf("%w: %q binds every interface", ErrPublicBind, bind)
		}
		host = "0.0.0.0"
	}

	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf(
			"bind host %q must be an IP address (use --bind 127.0.0.1:8787 or a specific interface address)", host)
	}

	ap, err := netip.ParseAddrPort(net.JoinHostPort(addr.String(), port))
	if err != nil {
		return netip.AddrPort{}, err
	}

	if allowPublic {
		return ap, nil
	}
	if addr.IsUnspecified() {
		return netip.AddrPort{}, fmt.Errorf("%w: %q is a wildcard address", ErrPublicBind, bind)
	}
	if isLocalish(addr) {
		return ap, nil
	}
	return netip.AddrPort{}, fmt.Errorf("%w: %q is globally routable", ErrPublicBind, bind)
}

func isLocalish(a netip.Addr) bool {
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() {
		return true
	}
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	ula := netip.MustParsePrefix("fc00::/7")
	return cgnat.Contains(a) || ula.Contains(a)
}

// ParseCIDRList parses a comma-separated prefix list; empty yields the defaults.
func ParseCIDRList(s string) ([]netip.Prefix, error) {
	if strings.TrimSpace(s) == "" {
		s = strings.Join(DefaultAllowedCIDRs, ",")
	}
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", part, err)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, errors.New("no usable CIDR ranges")
	}
	return out, nil
}

// AllowCIDR builds a predicate over remote addresses.
func AllowCIDR(prefixes []netip.Prefix) func(net.Addr) bool {
	return func(a net.Addr) bool {
		addr, ok := addrOf(a)
		if !ok {
			return false
		}
		addr = addr.Unmap()
		for _, p := range prefixes {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}
}

func addrOf(a net.Addr) (netip.Addr, bool) {
	switch v := a.(type) {
	case *net.TCPAddr:
		ip, ok := netip.AddrFromSlice(v.IP)
		return ip, ok
	default:
		host, _, err := net.SplitHostPort(a.String())
		if err != nil {
			return netip.Addr{}, false
		}
		ip, err := netip.ParseAddr(host)
		return ip, err == nil
	}
}

// FilterListener closes disallowed connections at accept time.
//
// This, not the bind address, is what protects a laptop that moves between
// networks: the address it listens on is stable, the machines around it are not.
// Rejecting before any read also keeps a stranger from reaching the HTTP parser.
type filteredListener struct {
	net.Listener
	allow  func(net.Addr) bool
	onDrop func(net.Addr)
}

// FilterListener wraps l so only allowed peers are served.
func FilterListener(l net.Listener, allow func(net.Addr) bool, onDrop func(net.Addr)) net.Listener {
	return &filteredListener{Listener: l, allow: allow, onDrop: onDrop}
}

func (f *filteredListener) Accept() (net.Conn, error) {
	for {
		c, err := f.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if f.allow == nil || f.allow(c.RemoteAddr()) {
			return c, nil
		}
		if f.onDrop != nil {
			f.onDrop(c.RemoteAddr())
		}
		_ = c.Close()
	}
}

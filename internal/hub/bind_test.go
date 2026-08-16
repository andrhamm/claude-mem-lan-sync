package hub

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

// Every wildcard spelling must be refused, not just the literal "0.0.0.0".
func TestClassifyBindRefusesWildcards(t *testing.T) {
	for _, bind := range []string{
		"0.0.0.0:8787",
		"[::]:8787",
		":8787",
		"[::0]:8787",
	} {
		if _, err := ClassifyBind(bind, false); !errors.Is(err, ErrPublicBind) {
			t.Errorf("ClassifyBind(%q) = %v, want it refused", bind, err)
		}
	}
}

func TestClassifyBindAcceptsLocalAddresses(t *testing.T) {
	for _, bind := range []string{
		"127.0.0.1:8787",
		"[::1]:8787",
		"192.168.1.10:8787",
		"10.1.2.3:8787",
		"172.16.5.5:8787",
		"100.101.102.103:8787", // tailscale
		"169.254.1.1:8787",
	} {
		if _, err := ClassifyBind(bind, false); err != nil {
			t.Errorf("ClassifyBind(%q) = %v, want it accepted", bind, err)
		}
	}
}

func TestClassifyBindRefusesGloballyRoutable(t *testing.T) {
	for _, bind := range []string{
		"8.8.8.8:8787",
		"93.184.216.34:8787",
		"[2606:2800:220:1:248:1893:25c8:1946]:8787",
	} {
		if _, err := ClassifyBind(bind, false); !errors.Is(err, ErrPublicBind) {
			t.Errorf("ClassifyBind(%q) = %v, want it refused", bind, err)
		}
	}
}

func TestClassifyBindAllowsPublicWithOverride(t *testing.T) {
	ap, err := ClassifyBind("0.0.0.0:8787", true)
	if err != nil {
		t.Fatalf("override rejected: %v", err)
	}
	if ap.Port() != 8787 {
		t.Fatalf("port = %d", ap.Port())
	}
}

func TestClassifyBindRejectsNonsense(t *testing.T) {
	for _, bind := range []string{"", "8787", "not-an-address:8787", "127.0.0.1"} {
		if _, err := ClassifyBind(bind, true); err == nil {
			t.Errorf("ClassifyBind(%q) accepted nonsense", bind)
		}
	}
}

func TestParseCIDRListDefaults(t *testing.T) {
	prefixes, err := ParseCIDRList("")
	if err != nil {
		t.Fatal(err)
	}
	allow := AllowCIDR(prefixes)

	for _, ip := range []string{"127.0.0.1", "192.168.1.5", "10.0.0.9", "100.80.1.1"} {
		if !allow(&net.TCPAddr{IP: net.ParseIP(ip), Port: 1234}) {
			t.Errorf("default ranges rejected %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "203.0.113.7"} {
		if allow(&net.TCPAddr{IP: net.ParseIP(ip), Port: 1234}) {
			t.Errorf("default ranges accepted public address %s", ip)
		}
	}
}

func TestParseCIDRListCustom(t *testing.T) {
	prefixes, err := ParseCIDRList("192.168.5.0/24")
	if err != nil {
		t.Fatal(err)
	}
	allow := AllowCIDR(prefixes)

	if !allow(&net.TCPAddr{IP: net.ParseIP("192.168.5.20"), Port: 1}) {
		t.Error("in-range address rejected")
	}
	if allow(&net.TCPAddr{IP: net.ParseIP("192.168.6.20"), Port: 1}) {
		t.Error("out-of-range address accepted")
	}
	if _, err := ParseCIDRList("not-a-cidr"); err == nil {
		t.Error("accepted an invalid CIDR")
	}
}

// The listener is what actually protects a laptop that changes networks: the
// bind address stays put, the neighbours do not.
func TestFilterListenerClosesDisallowedPeers(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.Close() }()

	dropped := make(chan string, 1)
	// Allow nothing, so the loopback dialer below is refused.
	l := FilterListener(base, AllowCIDR([]netip.Prefix{netip.MustParsePrefix("192.168.99.0/24")}),
		func(a net.Addr) { dropped <- a.String() })

	go func() {
		conn, err := l.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	c, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	select {
	case <-dropped:
	case <-timeoutAfter():
		t.Fatal("connection from a disallowed range was not dropped")
	}
}

func TestFilterListenerAcceptsAllowedPeers(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.Close() }()

	prefixes, err := ParseCIDRList("")
	if err != nil {
		t.Fatal(err)
	}
	l := FilterListener(base, AllowCIDR(prefixes), nil)

	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := l.Accept()
		if err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	c, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	select {
	case <-accepted:
	case <-timeoutAfter():
		t.Fatal("loopback connection was not accepted")
	}
}

func timeoutAfter() <-chan time.Time { return time.After(3 * time.Second) }

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/andrhamm/claude-mem-lan-sync/internal/discover"
	"github.com/andrhamm/claude-mem-lan-sync/internal/hub"
	"github.com/andrhamm/claude-mem-lan-sync/internal/logging"
	"github.com/andrhamm/claude-mem-lan-sync/internal/pair"
	"github.com/andrhamm/claude-mem-lan-sync/internal/paths"
	"github.com/andrhamm/claude-mem-lan-sync/internal/store"
)

// DefaultBind keeps a fresh install off the network until the user says
// otherwise.
const DefaultBind = "127.0.0.1:8787"

type serveFlags struct {
	bind        string
	allowCIDR   string
	dataDir     string
	allowPublic bool
	logLevel    string
	logFormat   string
	minFree     int64
	noMDNS      bool
	adverName   string
	printUnit   bool
	install     bool
	uninstall   bool
}

func runServe(args []string, env Env) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	var f serveFlags
	fs.StringVar(&f.bind, "bind", DefaultBind, "address to listen on (host:port)")
	fs.StringVar(&f.allowCIDR, "allow-cidr", "", "comma-separated peer ranges (default: private, CGNAT, link-local, loopback)")
	fs.StringVar(&f.dataDir, "data-dir", "", "hub data directory")
	fs.BoolVar(&f.allowPublic, "insecure-public-bind", false, "allow a wildcard or globally routable bind address")
	fs.StringVar(&f.logLevel, "log-level", "info", "debug|info|warn|error")
	fs.StringVar(&f.logFormat, "log-format", "text", "text|json")
	fs.Int64Var(&f.minFree, "min-free-bytes", 1<<30, "refuse writes below this much free disk space")
	fs.BoolVar(&f.noMDNS, "no-mdns", false, "do not advertise over mDNS")
	fs.StringVar(&f.adverName, "advertise-name", "", "mDNS instance name (default: a random label)")
	fs.BoolVar(&f.printUnit, "print-unit", false, "print a service unit and exit")
	fs.BoolVar(&f.install, "install-service", false, "install and start a user service")
	fs.BoolVar(&f.uninstall, "uninstall-service", false, "stop and remove the user service")

	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	log := logging.New(f.logLevel, f.logFormat, env.Stderr)

	dataDir, err := paths.DataDir(cmp(f.dataDir, env.DataDir))
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	if f.printUnit || f.install || f.uninstall {
		return runService(f, dataDir, env)
	}

	addr, err := hub.ClassifyBind(f.bind, f.allowPublic)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		if errors.Is(err, hub.ErrPublicBind) {
			fmt.Fprintf(env.Stderr,
				"\nBind a specific LAN or Tailscale address instead, for example:\n"+
					"  cmemlan serve --bind 192.168.1.10:8787\n\n"+
					"If you genuinely intend to expose this hub, pass --insecure-public-bind.\n")
		}
		return 1
	}

	// The container image cannot expand an environment variable in its exec-form
	// entrypoint, so the requirement lives here instead: inside a container the
	// bind guard is necessarily overridden, and the peer filter is what remains.
	allowCIDR := f.allowCIDR
	if allowCIDR == "" {
		allowCIDR = os.Getenv("CMEMLAN_ALLOW_CIDR")
	}
	if allowCIDR == "" && os.Getenv("CMEMLAN_REQUIRE_ALLOW_CIDR") == "1" {
		fmt.Fprintln(env.Stderr,
			"cmemlan: CMEMLAN_ALLOW_CIDR is required in this environment\n\n"+
				"This container binds every interface, so the peer allowlist is the only thing\n"+
				"restricting who can reach your memory. Set it to your LAN, for example:\n"+
				"  -e CMEMLAN_ALLOW_CIDR=192.168.1.0/24")
		return 1
	}

	prefixes, err := hub.ParseCIDRList(allowCIDR)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	release, err := Lockfile(dataDir)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	defer func() { _ = release() }()

	st, err := store.Open(filepath.Join(dataDir, "hub.db"), log)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	keys, err := pair.LoadOrCreate(dataDir)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	srv := hub.New(st, hub.Options{
		UserID:       st.UserID(),
		Auth:         hub.StaticToken{UserID: st.UserID(), Token: keys.PSK},
		Logger:       log,
		MinFreeBytes: f.minFree,
		FreeBytes:    func() (int64, error) { return freeBytes(dataDir) },
	})
	// The pairing window lives in a file so `cmemlan pair` — a separate process —
	// can open one against a hub that is already running.
	srv.SetPairExchanger(pair.FileWindow{Dir: dataDir})

	base, err := net.Listen("tcp", addr.String())
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: listening on %s: %v\n", addr, err)
		return 1
	}
	ln := hub.FilterListener(base, hub.AllowCIDR(prefixes), func(a net.Addr) {
		log.Warn("refused a connection from outside the allowed ranges", "peer", a.String())
	})

	httpSrv := hub.NewHTTPServer(addr.String(), srv.Handler())

	fmt.Fprintf(env.Stdout, "hub listening on %s\n", addr)
	fmt.Fprintf(env.Stdout, "hub id      %s\n", st.UserID())
	fmt.Fprintf(env.Stdout, "fingerprint %s\n", keys.Fingerprint())
	fmt.Fprintf(env.Stdout, "\nPair another machine with:\n  cmemlan pair\n")

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Advertising is skipped automatically for a loopback bind, where it would be
	// noise. Failures are logged, never fatal: discovery is a convenience and the
	// hub works fine with an explicit URL.
	mdnsCfg := discover.Config{
		Enabled:      !f.noMDNS,
		InstanceName: f.adverName,
		Port:         int(addr.Port()),
		BindAddr:     addr.Addr(),
	}
	if discover.ShouldAdvertise(mdnsCfg) {
		go func() {
			if err := discover.Advertise(ctx, mdnsCfg); err != nil {
				log.Warn("mDNS advertising stopped", "error", err)
			}
		}()
		fmt.Fprintf(env.Stdout, "advertising over mDNS as %s\n", discover.ServiceType)
	}

	select {
	case err := <-errCh:
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	case <-ctx.Done():
	}

	// Shutdown order matters: stop accepting, then close the write handle, which
	// checkpoints and truncates the write-ahead log.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown did not complete", "error", err)
	}
	log.Info("hub stopped")
	return 0
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

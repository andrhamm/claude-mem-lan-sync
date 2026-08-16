package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var errNoDiskCheck = errors.New("cmemlan: free space checks are unsupported on this platform")

// serviceUnit renders a systemd user unit.
//
// The data directory is written in as a resolved absolute path rather than left
// to environment inheritance: a unit that resolves a different directory than a
// manual `serve` gives the user two empty databases and a hub that looks like it
// lost every memory.
//
// The hardening directives matter because `serve` needs neither claude-mem's
// database nor any settings file — confining it to its own directory bounds the
// damage from a compromised dependency or a bug in the hub itself.
func serviceUnit(exe, dataDir, bind, allowCIDR string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[Unit]
Description=cmemlan — self-hosted LAN sync hub for claude-mem
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s serve --data-dir %s --bind %s`, exe, dataDir, bind)
	if allowCIDR != "" {
		fmt.Fprintf(&b, ` --allow-cidr %s`, allowCIDR)
	}
	fmt.Fprintf(&b, `
Restart=on-failure
RestartSec=5

# The hub only ever needs its own directory.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s
CapabilityBoundingSet=
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallFilter=@system-service
SystemCallArchitectures=native
MemoryMax=512M

[Install]
WantedBy=default.target
`, dataDir)
	return b.String()
}

// launchdPlist renders a macOS user agent.
func launchdPlist(exe, dataDir, bind string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.andrhamm.cmemlan</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string><string>serve</string>
    <string>--data-dir</string><string>%s</string>
    <string>--bind</string><string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>%s/cmemlan.log</string>
</dict>
</plist>
`, exe, dataDir, bind, dataDir)
}

func runService(f serveFlags, dataDir string, env Env) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: locating this binary: %v\n", err)
		return 1
	}

	switch runtime.GOOS {
	case "linux":
		unit := serviceUnit(exe, dataDir, f.bind, f.allowCIDR)
		if f.printUnit {
			fmt.Fprint(env.Stdout, unit)
			return 0
		}
		path := filepath.Join(userConfigDir(), "systemd", "user", "cmemlan.service")
		if f.uninstall {
			_ = exec.Command("systemctl", "--user", "disable", "--now", "cmemlan.service").Run()
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
				return 1
			}
			fmt.Fprintln(env.Stdout, "service removed")
			return 0
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		// Without lingering the unit stops at logout, and the symptom is exactly
		// the failure this project exists to avoid: the hub is down when the
		// other machine tries to sync.
		if out, err := exec.Command("loginctl", "enable-linger").CombinedOutput(); err != nil {
			fmt.Fprintf(env.Stderr,
				"cmemlan: could not enable lingering (%v: %s)\n"+
					"The hub will stop when you log out. Run: loginctl enable-linger\n", err, strings.TrimSpace(string(out)))
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", "cmemlan.service").CombinedOutput(); err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: starting the service: %v: %s\n", err, out)
			return 1
		}
		fmt.Fprintf(env.Stdout, "service installed and started (%s)\n", path)
		return 0

	case "darwin":
		plist := launchdPlist(exe, dataDir, f.bind)
		if f.printUnit {
			fmt.Fprint(env.Stdout, plist)
			return 0
		}
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		path := filepath.Join(home, "Library", "LaunchAgents", "dev.andrhamm.cmemlan.plist")
		if f.uninstall {
			_ = exec.Command("launchctl", "bootout", "gui/"+fmt.Sprint(os.Getuid())+"/dev.andrhamm.cmemlan").Run()
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
				return 1
			}
			fmt.Fprintln(env.Stdout, "service removed")
			return 0
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		if out, err := exec.Command("launchctl", "bootstrap", "gui/"+fmt.Sprint(os.Getuid()), path).CombinedOutput(); err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: bootstrapping the agent: %v: %s\n", err, out)
			return 1
		}
		fmt.Fprintf(env.Stdout, "agent installed and started (%s)\n", path)
		return 0

	default:
		fmt.Fprintf(env.Stderr, "cmemlan: service installation is not supported on %s\n", runtime.GOOS)
		return 1
	}
}

func userConfigDir() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config"
	}
	return filepath.Join(home, ".config")
}

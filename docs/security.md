# Security

## Threat model

A home network or a tailnet, where every machine is one you own. That is the design point, and the
defaults enforce it rather than assume it.

What this does **not** defend against: a hostile machine already inside your LAN with the key, an
attacker with read access to the hub's filesystem, or anything on the public internet. Do not run it
there.

## What is exposed

Op bodies are claude-mem observations and full prompt text — plausibly the most sensitive data on the
machine. They are **plaintext**:

- at rest in the hub's SQLite database
- in transit over HTTP on your LAN

"Private" here means private to your network, not encrypted. End-to-end encryption is not possible
without patching claude-mem, which sends plaintext to whatever hub it is configured with.

## Bind policy

`serve` binds `127.0.0.1:8787` by default and refuses to bind anything public without an explicit
override. Refusal is based on `net/netip` classification, not string matching, so all of these are
caught:

```
0.0.0.0:8787   [::]:8787   :8787   <hostname resolving to a public address>
```

`--allow-cidr` (default: RFC1918, CGNAT, link-local, loopback) is enforced **at accept time** —
connections from outside those ranges are closed before a byte is read. This is what protects a
laptop that moves between networks: its bind address stays put, its neighbours do not.

## Containers

The published image must bind the wildcard to be reachable at all, so the guard is effectively
overridden inside a container. Two consequences:

- Publish to loopback explicitly: `-p 127.0.0.1:8787:8787`.
- **Docker's port publishing installs DNAT rules that bypass ufw and firewalld.** A bare
  `-p 8787:8787` exposes the hub on every interface even if you believe you have firewalled the host.

The image refuses to start without `CMEMLAN_ALLOW_CIDR`, so the peer filter is always in force.

## Authentication

- A 32-byte pre-shared key, generated on first run, stored `0600` in the data directory.
- Every protocol request is compared in constant time.
- `X-User-Id` must match the hub's own id; a mismatch is `403` and **never** creates a new partition.
  Auto-creating one would let a typo produce a silently divergent, apparently healthy sync.
- Devices are recorded and can be revoked; revocation denies access on every route, not just listings.

### Pairing

`pair` opens a five-minute window and prints a nine-digit code plus a fingerprint. The code is
single-use, only a hash of it is stored, the attempt counter is persisted so restarting the hub
cannot reset it, and **five wrong guesses destroy the window**. A short code is only safe with a hard
cap like that — a LAN attacker can try thousands per second.

### The fingerprint matters

The key authenticates a device **to** the hub. Nothing in the protocol authenticates the hub **to** a
device. A machine on your network can advertise over mDNS, accept your pairing attempt, and relay to
the real hub while reading everything.

Comparing the fingerprint printed by `pair` against the one `connect` reports is the only check that
catches this, which is why `connect` requires `--fingerprint` and refuses before it touches the
network. `--yes` skips it; do not use it on a network you do not control.

## Server hardening

- Explicit timeouts: `ReadHeaderTimeout` 5s, `ReadTimeout` 60s, `WriteTimeout` 120s, `IdleTimeout`
  60s, `MaxHeaderBytes` 16KB. Go's zero-value server has none of these, and a slowloris is reachable
  before authentication.
- Request bodies capped with `MaxBytesReader` before any read; `Content-Encoding` refused outright.
- Bounded in-flight requests; excess returns `503`.
- `Origin` on a protocol route is refused — the real client never sends one, so it means a browser is
  talking to us. Combined with optional `Host` validation this closes DNS-rebinding paths.
- No `Server` header, and `/healthz` returns a bare `ok` with no version, id, or counts.
- Panic recovery, so one malformed request cannot take down the process that owns the sequencer.
- A free-space floor returns `507` rather than filling the disk the user's live memory database
  shares.

## Data handling

- Error bodies contain a fixed reason enum and nothing else. The client copies the first 200 bytes of
  an error into its own log **on another machine**, so echoing request content would copy memory
  across devices.
- Op bodies, pairing codes, and the key are never logged.
- The hub database and its `-wal`/`-shm` files are `0600`; the data directory is `0700`. SQLite
  creates them world-readable under a default umask, so this is set explicitly.
- Fixtures captured from real traffic are gitignored, and the scrub step regenerates digests before
  anything is committed.

## Service hardening

`--install-service` generates a systemd unit that confines the hub to its own directory:
`ProtectSystem=strict`, `ReadWritePaths=<data dir>`, `ProtectHome=read-only`, `NoNewPrivileges`,
`PrivateTmp`, `RestrictAddressFamilies`, `SystemCallFilter=@system-service`, an empty
`CapabilityBoundingSet`, and a memory cap.

`serve` needs neither claude-mem's database nor any settings file, so this bounds the blast radius of
both a compromised dependency and any bug in the hub itself.

## Remote access

Use Tailscale. Tailnet traffic is WireGuard-encrypted, so plain HTTP over it is fine, and CGNAT space
is already in the default allow list.

**Do not put self-signed TLS in front of the hub.** The client uses Node's `fetch`, which rejects it
without `NODE_EXTRA_CA_CERTS`, and the failure surfaces as an unexplained sync outage. If you want a
real certificate, `tailscale serve` will issue one.

## Reporting a vulnerability

Open a GitHub issue for anything that is not itself sensitive. For something that is, use GitHub's
private vulnerability reporting on this repository.

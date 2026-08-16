# claude-mem-lan-sync

Sync [claude-mem](https://github.com/thedotmack/claude-mem) memory between your own machines, over
your own network. No accounts, no third-party service, nothing leaves your LAN.

claude-mem gives Claude Code persistent memory. Its multi-device story is a hosted service that
uploads observation narratives and full prompt text to cmem.ai. The hub URL is configurable, and the
hub itself is just an ordered log — so `cmemlan` implements a compatible one that runs on a machine
you own.

```
   laptop ──┐                        ┌── desktop
            ├──► cmemlan hub ◄───────┤
   mac mini ┘     (your LAN)         └── work machine
```

## Quickstart

On the machine that will host the hub:

```bash
cmemlan serve --bind 192.168.1.10:8787 --install-service
cmemlan pair
```

`pair` prints a code and a fingerprint. On every other machine:

```bash
cmemlan connect http://192.168.1.10:8787 --code 431-982-507 --fingerprint eC4X-RHBe-QChQ
```

That is the whole setup. Then, on each machine, optionally upload the memories you already have:

```bash
cmemlan backfill --dry-run   # see what would upload
cmemlan backfill
```

Check on it any time:

```bash
cmemlan status
cmemlan doctor
```

## Install

```bash
# release binary
curl -fsSL https://raw.githubusercontent.com/andrhamm/claude-mem-lan-sync/main/install.sh -o install.sh
less install.sh          # read it before running it
sh install.sh

# or from source
go install github.com/andrhamm/claude-mem-lan-sync/cmd/cmemlan@latest

# or via container
docker run -v cmemlan:/data -p 127.0.0.1:8787:8787 \
  -e CMEMLAN_ALLOW_CIDR=192.168.1.0/24 ghcr.io/andrhamm/claude-mem-lan-sync
```

macOS binaries are unsigned and unnotarized, so Gatekeeper quarantines them on download. `install.sh`
clears the quarantine attribute; if you download manually, run
`xattr -d com.apple.quarantine ./cmemlan`.

## As a Claude Code plugin

The repository is also a plugin marketplace, so an agent can set this up for you:

```
/plugin marketplace add andrhamm/claude-mem-lan-sync
/plugin install claude-mem-lan-sync@andrhamm
```

## How it works

claude-mem keeps its memory in SQLite and treats the database as its own outbox: rows that were
written locally and never uploaded are queued by definition. The worker drains that queue to a hub
and pulls other devices' operations back, applying them in strict sequence order.

The hub stores an append-only log of opaque operations. It assigns each one a gapless sequence
number, hands pages back on request, and never looks inside a body. That is the entire job — which is
why a hub that gets the boring parts exactly right is sufficient.

Two properties do all the work:

- **Sequences are gapless.** The client refuses a page whose first entry is not exactly its cursor
  plus one, and a gap stalls that device permanently.
- **Bodies are byte-preserved.** The client re-canonicalises every body it receives and compares it
  to the raw string, so a single re-encoded character breaks it.

`docs/protocol.md` documents the wire format in full — it is otherwise undocumented, and reusable by
anyone else who wants to implement it.

## Commands

| Command | What it does |
|---|---|
| `serve` | Run the hub. `--install-service` sets up systemd or launchd. |
| `pair` | Open a five-minute pairing window, print a code and fingerprint. |
| `connect [url]` | Point this machine's claude-mem at a hub. Discovers one over mDNS if no URL is given. |
| `backfill` | Queue memories that predate sync. `--dry-run` first. `--undo` reverses it. |
| `status` | Local queue, worker state, hub position. |
| `doctor` | Diagnose why sync is not working. |
| `devices` / `revoke` | See and remove paired devices. |
| `rotate-token` | Replace the key. Does not disturb existing memories. |

## Security posture

The threat model is a home network or a tailnet. Read this before exposing it anywhere else.

- **Bodies are plaintext**, at rest on the hub and in transit over HTTP on your LAN. "Private" here
  means private to your network, not encrypted. End-to-end encryption is impossible without patching
  claude-mem, which sends plaintext.
- **Anyone who can read the hub database can read every paired device's memories.**
- `serve` binds loopback by default and refuses wildcard or globally routable addresses unless you
  pass `--insecure-public-bind`. Connections from outside `--allow-cidr` are dropped before a byte is
  read — that is what protects a laptop that moves between networks.
- **Do not run this on a VPS.** For remote access, use Tailscale: its traffic is WireGuard-encrypted,
  so plain HTTP over a tailnet is fine. Do **not** put self-signed TLS in front of it — the client's
  Node `fetch` rejects it without `NODE_EXTRA_CA_CERTS`.
- **Docker publishes ports through DNAT rules that bypass ufw and firewalld.** Use
  `-p 127.0.0.1:8787:8787`, and note the image refuses to start without `CMEMLAN_ALLOW_CIDR`.
- The pre-shared key is a password. `pair` codes are single-use, expire in five minutes, and five bad
  guesses destroy the window.
- The key authenticates a device to the hub, never the hub to a device. Comparing the fingerprint
  during `connect` is the only thing that detects something relaying to your real hub, which is why
  it is required.

## Privacy

cmemlan has no analytics and no crash reporting, and connects to no host other than the hub you
configure. When discovery is enabled it sends and answers mDNS multicast on the local link;
`--no-mdns` disables it. It performs no update checks.

Note that claude-mem itself sends anonymous usage metadata — an install id, counts, versions, not
memory content — to PostHog by default. Set `DO_NOT_TRACK=1` if you would rather it did not. cmemlan
neither enables nor disables that.

Compared with cmem.ai Cloud Sync: that service transmits observation and prompt bodies to a
third-party host; this hub keeps them on your network. No claim is made about what anyone else does
with data they receive.

## Compatibility

Compatibility is derived from the observed behaviour of **claude-mem 13.15.0**, not from a published
specification. `doctor` warns when your installed version differs from the tested one, and
`docs/protocol.md` records what was verified and how.

This project is independent and is not affiliated with claude-mem or cmem.ai.

## Documentation

- [Protocol](docs/protocol.md) — the wire contract
- [Architecture](docs/architecture.md) — how the pieces fit
- [Security](docs/security.md) — threat model and hardening
- [Backfill](docs/backfill.md) — uploading memories that predate sync
- [Testing](docs/testing.md) — including the real-client end-to-end tier
- [Troubleshooting](docs/troubleshooting.md)

## License

MIT

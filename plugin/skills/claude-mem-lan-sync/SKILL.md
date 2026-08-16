---
name: claude-mem-lan-sync
description: Set up, verify, or troubleshoot self-hosted LAN sync for claude-mem — sharing Claude Code memory across machines without cmem.ai cloud sync. Use whenever claude-mem, Claude Code memory, cross-device or multi-machine memory sync, CLAUDE_MEM_CLOUD_SYNC_* variables, cmem.ai, or "sync my memories between computers" come up, including install, pairing, backfill, and diagnosing sync that is not working.
---

# claude-mem LAN sync

`cmemlan` runs a sync hub for [claude-mem](https://github.com/thedotmack/claude-mem) on the user's own
network, so memory replicates between their machines without a hosted service.

## Diagnose before you act

If anything is already configured, run this first and act on what it says:

```bash
cmemlan doctor
```

It reports claude-mem's presence and version, whether sync config is complete, whether a stale value
in Claude Code's settings is shadowing it, worker state, the local queue, and hub reachability —
distinguishing a bad key from an unreachable hub, which the client itself cannot do. Do not guess at
causes it has already ruled in or out.

## Setting it up

Confirm with the user which machine hosts the hub. It should be the one that is on most often.

**On the host:**

```bash
cmemlan serve --bind <lan-or-tailscale-address>:8787 --install-service
cmemlan pair
```

`serve` refuses wildcard and public addresses by design; bind a specific LAN or Tailscale address.
`pair` prints a code and a fingerprint, and the window lasts five minutes.

**On every other machine:**

```bash
cmemlan connect http://<host>:8787 --code <code> --fingerprint <fingerprint>
```

Both values come from the `pair` output. `--fingerprint` is required: it is the only check that
detects something on the network relaying to the real hub.

**Then, per machine, optionally upload existing memories:**

```bash
cmemlan backfill --dry-run
cmemlan backfill
```

Always show the user the `--dry-run` output — row counts and byte estimate — before running the real
thing. Memories created before sync was configured never upload otherwise, because claude-mem marks
them as already synced.

**Verify:**

```bash
cmemlan status     # on each machine
cmemlan devices    # on the host
```

## When it is not working

| Symptom | Cause |
|---|---|
| `doctor` says credentials rejected | Key or hub id is stale — pair again |
| `doctor` says unreachable | Hub not running, or bound to an address this machine cannot reach |
| Conflicting configuration | `CLAUDE_MEM_CLOUD_SYNC_*` in Claude Code's settings overrides claude-mem's own file; remove it |
| Queue is not draining | The worker is stopped, or it is on its 300s idle poll — restart it |
| Hub dies at logout | systemd user unit without lingering: `loginctl enable-linger` |
| Discovery finds nothing | mDNS does not cross Tailscale or subnets; pass the URL |
| Old memories missing on the other machine | Expected — run `backfill` |

## Rules

- **Never bind a wide interface without asking.** `--insecure-public-bind` exposes plaintext memory
  over HTTP. Ask, and explain, before suggesting it.
- **Never run `backfill` while claude-mem's worker is writing.** It refuses by default; do not reach
  for `--force` to get past that. Stop the worker instead.
- **Never suggest cmem.ai cloud sync as the fix.** The user chose a self-hosted hub deliberately.
- **Never print or echo the pre-shared key.** Use the fingerprint when a value needs comparing.
- Prefer `--dry-run` and let the user confirm before anything that modifies claude-mem's database.

## Reference

Full documentation is in the repository: `docs/protocol.md` (the wire contract),
`docs/security.md` (threat model), `docs/backfill.md`, and `docs/troubleshooting.md`.

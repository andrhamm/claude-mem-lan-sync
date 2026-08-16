# Troubleshooting

Start here:

```bash
cmemlan doctor
```

It checks the things that actually break, in the order they break, and tells you what to do. The rest
of this page explains the failures it names.

## "sync configured" fails

claude-mem only enables sync when the hub URL, token, and hub id are **all** non-empty. Run
`cmemlan connect <url> --code <code> --fingerprint <fp>` again.

## "no conflicting configuration" fails

A `CLAUDE_MEM_CLOUD_SYNC_*` value is set in Claude Code's `settings.json` as well. Environment
variables override claude-mem's own settings file, so a stale value there silently wins — and points
your memory at a hub that may no longer exist. Remove those keys from `~/.claude/settings.json`.

## "hub reachable" fails

- **Connection refused** — the hub is not running. On the host: `systemctl --user status cmemlan`, or
  just `cmemlan serve`.
- **Rejected our credentials** — the key or hub id is wrong, usually because the hub's data directory
  was recreated or the key was rotated. Pair again.
- **Timeout** — a firewall, or the hub is bound to an address this machine cannot reach. Check
  `--bind` on the host and `--allow-cidr` if you narrowed it.

The client cannot tell these apart on its own: it retries silently forever, which is exactly why
`doctor` exists.

## The hub stops when I log out

A systemd *user* unit dies at logout unless lingering is enabled. `--install-service` tries to run
`loginctl enable-linger`; if it could not, run it yourself:

```bash
loginctl enable-linger
```

The symptom is the confusing one: the hub is only down when nobody is looking.

## Nothing syncs, but everything looks healthy

Check `cmemlan status`. If rows are waiting to upload:

- **Is the worker running?** Nothing syncs while it is stopped. Open Claude Code, or
  `npx claude-mem start`.
- **Has it drained recently?** With no active session the client polls every 300 seconds. Restart the
  worker to force a catch-up.

If the queue is zero but another machine sees nothing, that machine has not pulled yet — same 300
second interval.

## Old memories never appear on the other machine

That is expected: memories created before sync was configured are marked as already uploaded by
claude-mem's own migration. See [backfill](backfill.md).

## Discovery finds nothing

mDNS is multicast, and multicast does not cross Tailscale or route between subnets. On the same LAN
it should work; anywhere else, pass the address:

```bash
cmemlan connect http://hub.local:8787 --code … --fingerprint …
```

Discovery is also disabled on Windows, and a hub bound to loopback never advertises at all.

## "fingerprint mismatch"

Nothing was written. Either you typed the wrong fingerprint, or something on the network answered for
the hub. Check the address, open a fresh pairing window on the hub, and compare the fingerprint on
both screens before proceeding.

## "another serve process is already using this directory"

Two hubs on one data directory is never intended. Find the other one, or if you are certain it is
gone, delete `serve.lock` from the data directory.

## A device I no longer own can still sync

```bash
cmemlan devices
cmemlan revoke <device-id>
```

Revocation stops a device that is still behaving like itself. Because every device shares one key, a
machine that kept a copy could present a different device id — so if the key itself may have leaked,
rotate it and re-pair every machine:

```bash
cmemlan rotate-token
```

Rotation does not disturb stored memories and does not trigger a re-sync.

## Restoring the hub from a backup

Do not copy an old database in while devices are connected without expecting a full replay: the hub
detects that its counter and log disagree, rotates the epoch, and every device replays from zero.
That is safe — dedupe absorbs the duplicates — but it takes a while and is worth understanding before
you see it happen.

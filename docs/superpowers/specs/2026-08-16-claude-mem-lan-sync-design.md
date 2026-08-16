# claude-mem-lan-sync — Design

**Date:** 2026-08-16
**Status:** Approved design, pending implementation plan
**Module:** `github.com/andrhamm/claude-mem-lan-sync`
**Binary:** `cmemhub` (short to type; the repo name stays descriptive for search)
**License:** MIT

## Problem

[claude-mem](https://github.com/thedotmack/claude-mem) gives Claude Code persistent memory across
sessions, stored locally in SQLite. Its multi-device story is Cloud Sync, which replicates memory
through cmem.ai's hosted hub and requires a paid Pro account. Cloud Sync uploads observation
narratives and full prompt text to a third party.

The client's hub URL is configurable (`CLAUDE_MEM_CLOUD_SYNC_HUB_URL`), and the hub itself is a
simple ordered operation log. This project implements a compatible hub that runs on your own LAN, so
devices sync memory to each other and nothing leaves the network.

## Goals

- Two-way memory sync between a user's own machines, entirely on their network
- One command per machine to set up; zero secrets copied by hand
- Correct enough to trust with real memory: no data loss, no silent corruption
- A public, maintainable repo: docs, tests, CI-built binaries, container image

## Non-goals

- Hosting for other people, multi-tenant operation, or accounts
- Replacing claude-mem's own UI, search, or observation generation
- End-to-end encryption (bodies arrive as plaintext from the client; transport and host security
  are the boundary)
- Patching or forking claude-mem itself
- Internet-exposed deployment (see Security posture)

## Background: how claude-mem sync works

Verified against claude-mem **13.15.0**
(`~/.claude/plugins/cache/thedotmack/claude-mem/13.15.0/scripts/worker-service.cjs`).

Memory lives in `~/.claude-mem/claude-mem.db`. Three tables replicate: `observations`,
`session_summaries`, `user_prompts`. Each carries `synced_at`, `sync_rev`, `origin_device_id`,
`origin_local_id`.

**The database is the queue.** After each write, a debounced flusher (1500 ms, 250 ms fast path)
drains rows matching:

```sql
WHERE synced_at IS NULL AND origin_device_id IS NULL
```

in batches of 200, ordered by id. `origin_device_id IS NULL` means "authored on this machine" — rows
that arrived from the hub keep their origin set and are never pushed back, which prevents echo
loops. On successful push the client stamps `synced_at`.

Pulling is cursor-driven: the client stores a cursor in `sync_state`, requests changes after it, and
applies them with `ON CONFLICT(entity_id, entity_rev) DO NOTHING`. Malformed bodies are quarantined
rather than crashing the worker.

There is no daemon. The worker syncs on write, plus a poll timer: 30 s while a session is active,
300 s idle, suspending after 1 h of inactivity.

### The launch baseline

Schema migration v47 ran a one-time cutoff: it recorded every locally-authored row into
`sync_launch_exclusions(kind, origin_local_id, through_rev)` and set `synced_at = <migration time>`
on all of them. Pre-existing memories are therefore marked synced despite never having been
uploaded. Migration v48 backfills that table if it was missing.

`sync_launch_exclusions` is **not** consulted by the flusher. It **is** consulted when the hub's
epoch changes: the client resets its cursor to 0 and requeues local rows *except* those the
exclusions table covers at or below their current `sync_rev`.

Both migrations are gated on `schema_versions`, so they do not re-run.

## Protocol contract

All three routes take `Authorization: Bearer <token>`, `X-User-Id`, `X-Device-Id`, and optional
`X-Device-Name` (≤80 chars). Responses may carry `X-Sync-Mode`, which the client treats as an
advisory hint.

### Constants

| Name | Value |
|---|---|
| `protocol_version` | `2` |
| `body_schema_version` | `1` |
| `payload_schema_version` | `2` |
| Max op body | 256,000 UTF-8 bytes |
| Max push request | 4,000,000 bytes |
| Client push batch | 200 rows per table |
| Content kinds | `observation`, `summary`, `prompt` |
| Mutation ops | `set_title`, `set_prompt_session`, `remap_project` |
| Decimal strings | `^(?:0|[1-9][0-9]*)$`, ≤ `18446744073709551615` |
| Digest format | base64url SHA-256, `^[A-Za-z0-9_-]{43}$` |

All sequence numbers, revisions, and the epoch cross the wire as **decimal strings**, not JSON
numbers.

### Operation wrapper

Exactly two keys; the client rejects any wrapper with more:

```json
{ "body": "<canonical JSON string>", "operation_sha256": "<base64url SHA-256 of body>" }
```

`body` parses to exactly twelve keys: `body_schema_version`, `deleted`, `deleted_at`, `entity_rev`,
`id`, `kind`, `mutation`, `origin_device_id`, `origin_local_id`, `payload`,
`payload_schema_version`, `payload_sha256`.

Canonical JSON means sorted keys, plain objects only, finite numbers, safe integers only, uint64
values as decimal strings. The client re-canonicalizes received bodies and compares against the raw
string.

**Invariant: the hub stores and returns body bytes verbatim.** It verifies the digest and parses the
body for routing fields, but never re-serializes. Reproducing JavaScript's exact JSON escaping from
Go is not a fight worth having.

Entity ids are `"<kind>:<digest>"` for content and `"mutation:<uuid>"` for mutations.

### `GET /v1/sync/status`

```json
{ "protocol_version": 2, "epoch": "1", "head_seq": "402", "projected_seq": "402" }
```

The client rejects `projected_seq > head_seq`. Projection is a cmem.ai concept with no local
analogue, so we always report `projected_seq == head_seq`.

### `POST /v1/sync/ops`

Request: `{ "protocol_version": 2, "ops": [ <wrapper>, … ] }`

```json
{ "acked": [ { "id": "observation:…", "kind": "observation", "entity_rev": "1",
               "operation_sha256": "…", "seq": "403", "origin_local_id": "91" } ],
  "head_seq": "403", "projected_seq": "403" }
```

The client verifies every ack tuple against what it pushed and rejects extras, mismatches, or
inconsistent `seq` values for the same tuple. A duplicate `(entity_id, entity_rev)` re-acks the
**original** sequence number, making retries after a dropped response free.

### `GET /v1/sync/changes?since=<seq>&limit=<n>`

```json
{ "protocol_version": 2, "epoch": "1", "head_seq": "403", "more": false,
  "ops": [ { "seq": "403", "server_ts": 1755300000000,
             "body": "…", "operation_sha256": "…" } ] }
```

`server_ts` is optional (defaults to 0) and must be a safe integer. `more` must be a boolean.

**The client applies with `requireContiguous: true`:** each `seq` must be exactly the previous plus
one, including across page boundaries. A single gap raises `SyncApply: sequence gap` and stalls that
client. This is the strongest constraint in the system.

## Architecture

One Go binary, subcommands. Server and client roles are independent; the host machine runs both.

### Server components

| Component | Responsibility |
|---|---|
| `internal/hub` | HTTP handlers, routing, header extraction, `/healthz` |
| `internal/proto` | Wire types, validation, digest verification, decimal-string handling |
| `internal/store` | SQLite access, migrations, gapless sequencer, dedupe |
| `internal/discover` | mDNS advertise and browse |
| `internal/pair` | Pairing codes, PSK generation and storage |

### Client components

| Component | Responsibility |
|---|---|
| `internal/clientdb` | Read/write `~/.claude-mem/claude-mem.db` for backfill and status |
| `internal/settings` | Read/write `~/.claude/settings.json` env block; worker restart |
| `internal/cli` | Command dispatch, output formatting |

### Data flow

1. Machine A writes a memory. claude-mem's flusher drains `synced_at IS NULL` rows.
2. `POST /v1/sync/ops` → hub validates, assigns contiguous sequences, acks.
3. Machine A stamps `synced_at` from the acks.
4. Machine B polls `GET /v1/sync/changes?since=<cursor>`, applies, advances its cursor.

The hub holds no per-device sync state. Both directions are driven by the client's own cursor.

## Storage

```sql
CREATE TABLE ops (
  user_id          TEXT    NOT NULL,
  seq              INTEGER NOT NULL,
  entity_id        TEXT    NOT NULL,
  entity_rev       TEXT    NOT NULL,   -- decimal string
  kind             TEXT    NOT NULL,
  origin_device_id TEXT    NOT NULL,
  operation_sha256 TEXT    NOT NULL,
  body             TEXT    NOT NULL,   -- verbatim client bytes
  server_ts        INTEGER NOT NULL,
  PRIMARY KEY (user_id, seq)
);

CREATE UNIQUE INDEX ux_ops_entity ON ops (user_id, entity_id, entity_rev);

CREATE TABLE meta (
  user_id  TEXT PRIMARY KEY,
  epoch    TEXT    NOT NULL,
  head_seq INTEGER NOT NULL
);

CREATE TABLE devices (
  user_id     TEXT    NOT NULL,
  device_id   TEXT    NOT NULL,
  device_name TEXT,
  first_seen  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  PRIMARY KEY (user_id, device_id)
);
```

SQLite in WAL mode with a busy timeout. Driver: `modernc.org/sqlite` (pure Go, keeps
cross-compilation trivial).

### Sequencer

A single writer goroutine serializes all pushes. Each push runs one transaction: read `head_seq`,
assign `head_seq+1 … head_seq+n`, insert, update `head_seq`, commit. A rollback assigns nothing, so
the log has no gaps. `AUTOINCREMENT` is prohibited — it burns values on rollback and would wedge
every client.

Dedupe happens against `ux_ops_entity` inside the same transaction: an op whose `(entity_id,
entity_rev)` already exists consumes no new sequence and is acked with the stored one.

### Epoch

Generated once per user partition at creation and stored in `meta`. It changes only when the log is
wiped, which tells clients their cursors are void and a full replay is needed.

## Validation at ingest

Rejections return `400` with a plain-text reason; the client records these in its dead-letter log.

1. `protocol_version == 2`
2. Wrapper has exactly `body` and `operation_sha256`
3. Digest matches SHA-256 of the raw body bytes, base64url encoded
4. Body parses as JSON and carries exactly the twelve required keys
5. `kind` ∈ {`observation`, `summary`, `prompt`, `mutation`}; mutation ops carry a known mutation op
   and null payload
6. `entity_rev` is a positive decimal string within uint64
7. Body ≤ 256,000 bytes; request ≤ 4,000,000 bytes; ≤ 1,000 ops per request (the client sends at
   most 200 rows per table plus mutations, so this is headroom, and the byte cap governs in practice)

The hub does not emit `X-Sync-Mode`; the header exists for hosted maintenance states with no local
analogue, and the client treats its absence as normal operation.

Canonical-form re-serialization is deliberately **not** checked — the digest already binds the
bytes, and matching JavaScript's escaping rules from Go invites false rejections.

## Discovery

The hub advertises `_cmemhub._tcp.local` over mDNS/DNS-SD with TXT records `v=1`, `hub=<short id>`,
`name=<hostname>`. `connect` with no URL browses for hubs and prompts.

Multicast does not traverse Tailscale or route between subnets. Same-LAN discovery works;
cross-network setups pass the hostname explicitly (`connect http://hub.local:8787`). Documented, not
worked around.

## Auth and pairing

The hub is the identity boundary. No Claude Code credentials are read.

- First `serve` generates a hub id and a 32-byte pre-shared key, stored `0600` in the data directory
- `user_id` = hub id, shared by all paired devices, which partitions the log
- Every protocol request requires `Authorization: Bearer <psk>`, compared in constant time; missing
  or wrong key returns `401`
- `cmemhub pair` opens a five-minute window and prints a single-use code. `connect --code <code>`
  exchanges it for the PSK via `POST /pair` (our endpoint, outside the claude-mem protocol surface)
- `connect --token <psk>` remains available for scripted installs
- Pairing events are logged with device name and surface in `status`

## CLI surface

| Command | Behavior |
|---|---|
| `serve` | Run the hub. Binds loopback by default; `--bind` for a specific interface; refuses `0.0.0.0` without an explicit override flag. `--install-service` writes a systemd user unit or launchd plist. `--record <dir>` captures traffic for fixtures. |
| `pair` | Open a pairing window, print the code. |
| `connect [url]` | Discover or take a URL, pair, write the four env vars into `~/.claude/settings.json`, restart the worker, offer backfill. |
| `backfill` | Requeue locally-authored rows. `--dry-run`, `--project`, `--since`. |
| `status` | Local pending counts, hub head/epoch, paired devices and last-seen. |
| `doctor` | Diagnose claude-mem presence and version, env vars, worker state, hub reachability, pending queue, version-compatibility warning. |

Env vars written by `connect`: `CLAUDE_MEM_CLOUD_SYNC_HUB_URL`, `CLAUDE_MEM_CLOUD_SYNC_TOKEN`,
`CLAUDE_MEM_CLOUD_SYNC_USER_ID`, `CLAUDE_MEM_CLOUD_SYNC_DEVICE_NAME`. All of the first three must be
non-empty for the client to activate sync. `CLAUDE_CONFIG_DIR` is honored when set.

## Backfill

Existing memories predate the launch baseline and are marked synced, so they never upload. Backfill
reverses that:

```sql
UPDATE observations      SET synced_at = NULL WHERE origin_device_id IS NULL;
UPDATE session_summaries SET synced_at = NULL WHERE origin_device_id IS NULL;
UPDATE user_prompts      SET synced_at = NULL WHERE origin_device_id IS NULL;
DELETE FROM sync_launch_exclusions;
```

Safety properties:

- `sync_rev` was created as `TEXT NOT NULL DEFAULT '1'`, so every legacy row already has a valid
  positive revision and none will be quarantined
- `origin_device_id IS NULL` restricts the requeue to locally-authored rows, so no echo loop
- Re-running costs bandwidth only; the hub dedupes on `(entity_id, entity_rev)`
- Clearing `sync_launch_exclusions` keeps the backfill durable across a future epoch change

The command warns when the claude-mem worker is running and offers to stop it first, then reports
per-table counts. `--dry-run` reports without writing.

**Fragility to watch:** backfill depends on the flusher's predicate and on `sync_launch_exclusions`
not becoming a filter in that path. A fixture test against a real database pins this; `doctor` warns
when the installed claude-mem version is outside the tested range.

## Failure modes

| Situation | Behavior |
|---|---|
| Hub down | Client queues locally, retries with backoff. Nothing lost. |
| Hub data wiped | New epoch → clients reset cursors, replay from 0, dedupe absorbs duplicates. |
| Malformed push | `400`, client dead-letters that op and continues. |
| Duplicate push after dropped response | Original sequence re-acked; no duplicate log entry. |
| Two devices edit the same entity | Distinct `entity_rev` values; both reach the log; the client's stale-revision check decides. |
| Clock skew | `server_ts` is informational only; ordering comes from sequence numbers. |
| Sequence exhaustion | uint64; practically unreachable, but the sequencer returns an error rather than wrapping. |

## Testing

1. **Unit** — validation tables, digest verification, decimal-string parsing and comparison,
   rejection cases for each rule above.
2. **Sequencer property tests** — concurrent pushes with induced rollbacks; assert the log is always
   contiguous `1..head` and that dedupe never consumes a sequence.
3. **Golden replay** — `serve --record` captures real request/response pairs from a live claude-mem
   install; fixtures live in `testdata/fixtures/`. Tests replay recorded client bytes and assert our
   responses satisfy the client's validators. This converts protocol inference into evidence.
4. **Fuzz** — Go native fuzzing over the body parser and validator.

A full end-to-end test needs an authenticated Claude account, so CI cannot run one. The substitute is
capture-replay plus a documented two-machine manual checklist in `docs/testing.md`.

## Repository

```
cmd/cmemhub/                main, subcommand dispatch
internal/{proto,store,hub,discover,pair,clientdb,settings,cli}/
testdata/fixtures/          captured client traffic
docs/                       protocol.md, architecture.md, security.md,
                            backfill.md, testing.md, troubleshooting.md
.github/workflows/          ci.yml, release.yml
Dockerfile · install.sh · README.md · CLAUDE.md · LICENSE
```

**CI** (`ci.yml`): `go vet`, golangci-lint, `go test -race`, build matrix, on push and PR.

**Release** (`release.yml`): tag-triggered GoReleaser producing linux amd64/arm64, darwin
amd64/arm64, windows amd64 binaries with checksums, a multi-arch `ghcr.io` image, and generated
release notes. Dependabot covers actions and modules.

**Docs:** `README.md` leads with the per-machine quickstart, then architecture, then the security
posture. `docs/protocol.md` publishes the wire contract above — the most reusable artifact in the
repo, since it is otherwise undocumented.

**`CLAUDE.md`** encodes the invariants:

- Bodies are stored and returned verbatim; never unmarshal-then-marshal
- Sequences are gapless; `AUTOINCREMENT` is prohibited
- Digests are base64url, not hex
- Sequence numbers, revisions, and epochs are decimal strings end to end
- Re-capture fixtures on claude-mem updates; `doctor` warns on untested versions
- Dependency budget: stdlib, SQLite driver, mDNS library; anything else needs justification

## Compatibility policy

Compatibility is derived from observed behavior of claude-mem 13.15.0, not from a published
specification. The README states this plainly, along with a note that the project is independent of
claude-mem and cmem.ai. `doctor` reports the installed claude-mem version and warns when it falls
outside the tested range. `docs/protocol.md` records which version each detail was verified against.

## Security posture

The threat model is a trusted home or tailnet. Stated once, plainly, in the README:

- Bodies are plaintext; anyone who can read the hub database can read your memories
- `serve` binds loopback by default and requires an explicit flag to bind a wide interface
- The PSK is the only access control; treat it as a password
- Nothing is designed for internet exposure. Behind Tailscale is the supported remote story
- No telemetry, no outbound connections other than serving requests

## Deferred

- **WebSocket endpoint** — the client supports one as a latency optimization and drops it with zero
  data loss. Phase 2.
- **Retention and compaction** — the log grows without bound; superseded revisions could be pruned
  once every device has passed them.
- **Metrics** — Prometheus endpoint behind a flag.
- **Discovery beyond the LAN** — mDNS cannot cross Tailscale; a static peer list is the likely
  answer.

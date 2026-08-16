# claude-mem-lan-sync — Design

**Date:** 2026-08-16
**Revision:** 2 — incorporates protocol-fidelity, Go-feasibility, and security reviews
**Status:** Approved design, pending implementation plan
**Module:** `github.com/andrhamm/claude-mem-lan-sync`
**Binary:** `cmemlan`
**License:** MIT

## Problem

[claude-mem](https://github.com/thedotmack/claude-mem) gives Claude Code persistent memory across
sessions, stored locally in SQLite. Its multi-device story is Cloud Sync, which replicates memory
through cmem.ai's hosted hub and requires a paid Pro account. Cloud Sync uploads observation
narratives and full prompt text to a third party.

The client's hub URL is configurable (`CLAUDE_MEM_CLOUD_SYNC_HUB_URL`), and the hub itself is an
ordered operation log. This project implements a compatible hub that runs on your own LAN, so devices
sync memory to each other and nothing leaves the network.

## Goals

- Two-way memory sync between a user's own machines, entirely on their network
- One command per machine to set up; zero secrets copied by hand
- Correct enough to trust with real memory: no data loss, no silently wedged clients
- A public, maintainable repo: docs, tests, CI-built binaries, container image, plugin marketplace

## Non-goals

- Hosting for other people, multi-tenant operation, or accounts
- Replacing claude-mem's own UI, search, or observation generation
- End-to-end encryption (bodies arrive as plaintext from a client we do not control)
- Patching or forking claude-mem itself
- Internet-exposed deployment

## Background: how claude-mem sync works

Verified against claude-mem **13.15.0**
(`~/.claude/plugins/cache/thedotmack/claude-mem/13.15.0/scripts/worker-service.cjs`).
`worker-service.cjs` is the sole protocol client; no other bundle contacts the hub.

Memory lives in `~/.claude-mem/claude-mem.db`. Three tables replicate: `observations`,
`session_summaries`, `user_prompts`, each carrying `synced_at`, `sync_rev`, `origin_device_id`,
`origin_local_id`.

**The database is the queue.** A debounced flusher drains rows matching:

```sql
WHERE synced_at IS NULL AND origin_device_id IS NULL ORDER BY id LIMIT 200
```

`origin_device_id IS NULL` means "authored on this machine" — rows that arrived from the hub keep
their origin set and are never pushed back, preventing echo loops. On a successful push the client
stamps `synced_at`.

Pulling is cursor-driven: the client stores a cursor in `sync_state`, requests changes after it, and
applies them with `ON CONFLICT(entity_id, entity_rev) DO NOTHING`.

Timings: debounce 1500 ms (250 ms only while a WebSocket is live); poll 30 s active, 300 s idle,
suspend after 1 h; `minPullGapMs` 2 s; request timeout 30 s. Without a WebSocket endpoint an active
session polls at 30 s.

### The launch baseline

Migration v47 ran a one-time cutoff: it inserted every `origin_device_id IS NULL` row into
`sync_launch_exclusions(kind, origin_local_id, through_rev)`, stamped `synced_at = <migration time>`
on those rows that still had `synced_at IS NULL`, and **also wiped `sync_outbox`,
`sync_content_outbox`, `sync_dead_letter`, and `sync_state`** (the latter holds both cursor and
epoch). Migration v48 backfills the exclusions table if it was missing. Both are gated on
`schema_versions` and do not re-run.

`sync_launch_exclusions` is not consulted by the flusher. It **is** consulted on epoch change: the
client resets its cursor to 0 and requeues local rows except those the exclusions table covers at or
below their current `sync_rev`.

## Protocol contract

Routes: `GET /v1/sync/status`, `POST /v1/sync/ops`, `GET /v1/sync/changes`, and (phase 2)
`GET /v1/sync/ws`. All take `Authorization: Bearer <token>`, `X-User-Id`, `X-Device-Id`, and
`X-Device-Name` (≤80 chars; nominally optional but it falls back to `os.hostname()`, so it is
effectively always sent — display only, never key on it).

### Constants

| Name | Value |
|---|---|
| `protocol_version` | `2` (JSON number, checked on status and changes) |
| `body_schema_version` / `payload_schema_version` | `1` / `2` |
| Max op body | 256,000 UTF-8 bytes |
| Max push request | 4,000,000 bytes (client packs to just under; leave framing headroom) |
| Ops per request | ≤200 in practice (one drain batch); client hard cap 500 |
| Pull page | client always sends `limit=500`, up to 40 pages per cycle |
| Content kinds | `observation`, `summary`, `prompt` |
| Mutation ops | `set_title`, `set_prompt_session`, `remap_project` |
| Decimal strings | `^(?:0|[1-9][0-9]*)$`, ≤ `18446744073709551615` |
| Digest | base64url SHA-256, `^[A-Za-z0-9_-]{43}$` — not hex, unpadded |
| `origin_device_id` | ≤128 UTF-8 bytes |
| Filterable strings | ≤4096 bytes, non-blank |
| `deleted_at` | canonical ISO timestamp ≤40 chars |

**Every sequence, revision, and epoch is a decimal string on the wire, never a JSON number.** This
includes `server_ts`.

### Operation wrapper

Exactly two keys; extras are rejected:

```json
{ "body": "<canonical JSON string>", "operation_sha256": "<base64url SHA-256 of body>" }
```

`body` parses to exactly twelve keys: `body_schema_version`, `deleted`, `deleted_at`, `entity_rev`,
`id`, `kind`, `mutation`, `origin_device_id`, `origin_local_id`, `payload`,
`payload_schema_version`, `payload_sha256`.

Content ids are `<kind>:base64url_sha256(JSON.stringify(["cmem-doc-id-v1","device",kind,
origin_device_id,origin_local_id]))`. Mutation ids are `mutation:<uuid>` with a strict lowercase UUID
pattern (version `[1-8]`, variant `[89ab]`). The hub need not verify derivation, but `docs/protocol.md`
publishes it — it is what makes the format reimplementable, and it explains why device-id stability
matters.

**Invariant, the highest-consequence rule in this project: the hub stores and returns body bytes
verbatim.** On pull the client re-canonicalizes every received body and compares it against the raw
string, then re-verifies the digest. One byte of drift permanently wedges every client. The hub
verifies the digest and parses the body for routing fields; it never re-serializes.

### `GET /v1/sync/status`

```json
{ "protocol_version": 2, "epoch": "1", "head_seq": "402", "projected_seq": "402" }
```

### `POST /v1/sync/ops`

Request `{ "protocol_version": 2, "ops": [ <wrapper>, … ] }`. Response:

```json
{ "acked": [ { "id": "observation:…", "kind": "observation", "entity_rev": "1",
               "operation_sha256": "…", "seq": "403", "origin_local_id": "91" } ],
  "head_seq": "403", "projected_seq": "403" }
```

**`projected_seq` must equal `head_seq` exactly.** `/status` rejects `projected_seq > head_seq`;
`/ops` rejects `head_seq > projected_seq`. Only equality satisfies both. Additionally every acked
`seq` must be ≤ both.

**The ack contract, in full — each rule below wedges the client if broken:**

1. **Every received op is acked exactly once.** Multiplicity is counted per received op, not per
   unique entity. There is no partial success: `200` means all ops are durable.
2. **The ack echoes the client's `operation_sha256`**, never a digest read from storage. The tuple
   key is `[id, kind, entity_rev, operation_sha256]`; echoing a stored digest misses the key and
   reads as an extra acknowledgment.
3. **A duplicate op appearing twice in one push must be acked twice with the same `seq`.** Collapsing
   duplicate acks is a multiplicity mismatch.
4. **Two distinct tuples must never share a `seq` within one response**, and one tuple must never
   carry two different `seq` values.
5. **All six ack fields are mandatory.** `origin_local_id` may be `null` but must be present —
   omitting it throws. `entity_rev` and `seq` are positive decimal strings.
6. Ack **order is irrelevant**; validation is map-based.

**First-write-wins on `(entity_id, entity_rev)`.** The hub must never admit two different bodies at
the same entity revision — a pulling client throws "same entity revision has a different canonical
operation hash". A duplicate consumes no sequence and is re-acked with the stored one. The
pathological case — one push containing two ops with the same `(entity_id, entity_rev)` but different
digests — has no conforming response (acking both with the stored seq trips rule 4; rejecting wedges
the pipeline). A conforming client cannot produce it: first-write-wins, ack both with the stored seq,
log loudly.

### `GET /v1/sync/changes?since=<seq>&limit=<n>`

```json
{ "protocol_version": 2, "epoch": "1", "head_seq": "403", "more": false,
  "ops": [ { "seq": "403", "server_ts": "1755300000000",
             "body": "…", "operation_sha256": "…" } ] }
```

`more` must be a boolean. `server_ts` is **a decimal string or absent** — a JSON number throws, and
so does `null`. Absent yields 0.

**The client applies with `requireContiguous: true`, anchored to its stored cursor**, so the first op
of every page must be exactly `cursor + 1`, and each subsequent seq exactly the previous plus one. A
gap throws, the whole page rolls back, the cursor does not advance, and that page is retried forever
with backoff to 10 minutes. Consequences, all of which are hub obligations:

- **Never filter out the requesting device's own ops.** The natural optimization — don't echo a
  device its own writes — creates gaps and wedges that device permanently. The client filters them
  itself, after the contiguity check.
- **Never prune ops from the middle of the sequence space.** See Blocked, below.
- **Any sequence-space discontinuity requires an epoch bump** — including restoring the hub database
  from an older backup. Without one, clients sit above `head_seq` polling an empty result forever
  with no error.
- **The first op is `seq "1"`.** The initial cursor is `"0"` and `SF` requires a positive seq.

### Error handling: a persistent 4xx wedges the client permanently

There is no dead-lettering on the HTTP path. Every non-2xx on every route is one generic throw plus
backoff; `sync_dead_letter` is written only on *local* canonicalization failure. A rejected op stays
at the head of the outbox and is re-sent forever, **blocking every op behind it**. The client
distinguishes no status codes — a bad PSK is indistinguishable from an outage.

This inverts the usual posture: **ingest validation is a hazard surface, not a safety feature.**
Validate only what protects the hub; accept anything a conforming client can construct. The hub must
never return 3xx (fetch cannot follow), and never 204/205 (they pass `res.ok` then fail JSON
parsing). Every success path is `200` with a JSON body. Reject reasons come from a fixed enum and
never echo request bytes — the client logs them into its own dead-letter store on another machine.

`doctor` and `status` must surface `lastError` and outbox depth, because the client never will.

### `X-Sync-Mode: poll`

`X-Sync-Mode: poll` means "do not attempt the WebSocket": the client tears down the socket and
suppresses reconnects. Any other value, **including an absent header**, re-enables it.

The client enables WebSockets by default (`CLAUDE_MEM_CLOUD_SYNC_WS !== "false"`) and connects to
`hubUrl.replace(/^http/i,"ws") + "/v1/sync/ws"` with backoff capped at 60 s. Without this header a
phase-1 hub gets an endless reconnect loop against a 404 from every device. **Emit
`X-Sync-Mode: poll` on all three routes until the WebSocket endpoint ships** — server-side, needs no
client config, self-corrects when we ship it. `connect` also writes `CLAUDE_MEM_CLOUD_SYNC_WS=false`
as belt and braces.

### WebSocket contract (phase 2, recorded now)

Upgrade carries the same four headers; no subprotocol, no hello frame — authenticate from the upgrade
request alone. Text frames only. Optional `epoch` on any frame must match the client's stored epoch.

- `{"type":"advance","head_seq":"<decimal string>"}` → client force-pulls over HTTP.
- `{"type":"op","ops":[<change records>]}` → must be internally contiguous and start at ≤ cursor+1;
  overlap tolerated, gaps fatal.

The client pings every 40 s and never checks for a pong; standard libraries auto-pong.
**Recommendation: implement `advance` only.** It carries no data-loss risk and the client pulls over
HTTP anyway. HTTP-only operation is genuinely lossless.

## Architecture

One Go binary. Server and client roles are independent; the host machine runs both.

| Package | Responsibility |
|---|---|
| `internal/proto` | Wire types, validation, digests, decimal handling. Imports nothing internal. |
| `internal/store` | SQLite, migrations, sequencer, dedupe. Imports `proto`. |
| `internal/hub` | HTTP handlers, middleware, `/healthz`. Imports `proto`, `store`. |
| `internal/discover` | mDNS advertise and browse. Imports neither `store` nor `hub`. |
| `internal/pair` | Pairing codes, PSK storage. |
| `internal/clientdb` | claude-mem database access for backfill and status. |
| `internal/settings` | claude-mem settings file read/modify/write. |
| `internal/cli` | Command dispatch. Takes an `io.Writer`, a path root, and a hub-client interface so `doctor` and `connect` are testable. |

## Storage

```sql
PRAGMA user_version = 1;   -- forward-only ladder; refuse to run against a newer schema

CREATE TABLE ops (
  user_id          TEXT    NOT NULL,
  seq              INTEGER NOT NULL,          -- signed 64-bit; ceiling is 2^63-1
  entity_id        TEXT    NOT NULL,
  entity_rev       TEXT    NOT NULL,          -- canonical decimal string
  kind             TEXT    NOT NULL,
  origin_device_id TEXT    NOT NULL,
  operation_sha256 TEXT    NOT NULL,
  body             TEXT    NOT NULL,          -- raw JSON string literal, quotes included
  server_ts        INTEGER NOT NULL,
  PRIMARY KEY (user_id, seq)
);
CREATE UNIQUE INDEX ux_ops_entity ON ops (user_id, entity_id, entity_rev);

CREATE TABLE meta    (user_id TEXT PRIMARY KEY, epoch TEXT NOT NULL, head_seq INTEGER NOT NULL);
CREATE TABLE devices (user_id TEXT NOT NULL, device_id TEXT NOT NULL, device_name TEXT,
                      first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL,
                      token_hash TEXT, revoked_at INTEGER,
                      PRIMARY KEY (user_id, device_id));
```

Driver `modernc.org/sqlite` (pure Go) — this is what makes darwin/windows cross-compilation from
Linux CI a plain `go build`, and it is the load-bearing reason for the choice.

DSN: `?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate`.
modernc's DSN syntax differs from mattn's and **unknown keys are silently ignored**, so verify with
`PRAGMA journal_mode;` at startup and fail hard if it is not `wal`. Use `'single quotes'` for all SQL
string literals — SQLite accepts double-quoted literals as a fallback, turning a mistyped column name
into a silent string.

Two `sql.DB` handles on the same file: a write handle with `SetMaxOpenConns(1)` and
`_txlock=immediate`, and a multi-connection read handle for `/changes` and `/status`.

### Sequencer

`BEGIN IMMEDIATE` is **mandatory**. The sequencer reads `head_seq` then writes; under a deferred
transaction that is a lock upgrade, and SQLite cannot honor `busy_timeout` on an upgrade.

Per push, inside one immediate transaction: select existing `(entity_id, entity_rev)` rows, assign
`head_seq+1 …` to the misses only, insert, update `head_seq`, commit. Never increment per-op before
dedupe. `INSERT … ON CONFLICT DO NOTHING RETURNING seq` returns nothing on conflict and so cannot
supply the stored seq for the re-ack.

`AUTOINCREMENT` is prohibited — it burns values on rollback and would wedge every client.

Serialize with a `sync.Mutex` in the handler goroutine, not a writer goroutine: no batching win, and
a channel design creates a requester-gave-up case plus hazardous shutdown ordering.
**Do not pass the request context into the write transaction** — modernc implements neither
`driver.Validator` nor `driver.SessionResetter`, so a client hang-up discards the connection. Use
`context.WithoutCancel` plus a bounded timeout. Both crash outcomes are safe: commit means the client
retries and dedupe re-acks; rollback assigns nothing and leaves no gap.

Shutdown: `http.Server.Shutdown` → close the write handle → `PRAGMA wal_checkpoint(TRUNCATE)`.
Cross-process safety comes from SQLite's own locking; take a lockfile so two `serve` processes fail
loudly rather than subtly.

### Epoch

A positive decimal string within uint64 — **not** a UUID or hex id, which the client's regex rejects.
Generate 63 bits from `crypto/rand`, format as decimal, store in `meta`, and keep it byte-stable
across restarts. It changes only on a wipe or any sequence-space discontinuity. **Rotating the PSK
must not change the epoch** — conflating them forces a needless full replay everywhere.

### Byte-verbatim handling in Go

`json.RawMessage` is verbatim on decode but **not on encode**: `json.Marshal` hardcodes HTML escaping
and compaction even for `RawMessage`. Worse, Go's decoder silently replaces invalid UTF-16 surrogates
with U+FFFD — an emoji truncated at a UTF-16 boundary would mutate the body, fail its own digest, and
that memory would never sync. The required pattern:

1. Decode the wrapper into `struct{ Body json.RawMessage; OperationSHA256 json.RawMessage }`. `Body`
   is now the raw JSON string literal, quotes and escapes intact.
2. Unquote with a strict custom unquoter — `strconv.Unquote` does not implement JSON escapes — that
   **rejects** invalid UTF-8 and lone surrogates rather than substituting.
3. Digest and validate the unquoted bytes; unmarshal them for the twelve routing keys; discard both.
4. **Store the step-1 literal** in the `body` column.
5. Emit responses by writing bytes directly to an `io.Writer`. Never place a body in a struct and
   marshal it.

Parsing hazards to handle explicitly: `http.MaxBytesReader` before any read; duplicate JSON keys
defeat a `map[string]json.RawMessage` length check (use a `Decoder` token walk); `Decoder.Decode`
does not reject trailing data; use `UseNumber()` anywhere numbers are inspected.

Forward risk: `encoding/json/v2` becomes default in Go 1.27 and may shift escaping and duplicate-key
behavior. Byte-level golden tests are what pin this.

### Decimal type strategy

Define one `type Dec uint64` in `internal/proto` whose `MarshalJSON`/`UnmarshalJSON` emit and parse
*quoted* decimal with canonical-form and range checks. Use it for seq, entity_rev, head_seq,
projected_seq, and epoch, so no handler can accidentally emit a JSON number.

- SQLite INTEGER is signed: parse with `ParseUint` then explicitly reject `> math.MaxInt64`. The real
  sequence ceiling is 2^63−1, not uint64.
- **Enforce canonical form before insert** — `ux_ops_entity` is a TEXT comparison, so `"1"` and
  `"01"` would be distinct rows for one logical revision and break the dedupe guarantee.
- The shared regex admits `"0"`; `entity_rev` and `seq` additionally require positive. Both checks
  live in `Dec`.
- Never compare or sort these as strings (`"9" > "10"`), and never `CAST(... AS INTEGER)` above 2^63.

## Validation at ingest

Minimal and permissive, because rejection is permanent (see above). Reject only:

1. `protocol_version != 2`
2. Wrapper keys other than exactly `body` and `operation_sha256`
3. Digest mismatch against the raw body bytes
4. Body not parsing, or not carrying exactly the twelve keys (duplicate-key aware)
5. Unknown `kind`; `entity_rev` non-positive, non-canonical, or out of range
6. Body > 256,000 bytes; request > 4,000,000 bytes; `limit` outside `[1,500]`; malformed `since`
7. `X-User-Id` not matching this hub's id → `403`, never auto-create a partition

Rule 7 prevents a silent one-way divergence: a device with a stale user id but a valid PSK would push
its whole history, receive valid acks, stamp `synced_at`, and never push again — while the other
device sees nothing and both report healthy sync.

Responses are byte-capped as well as count-capped (fill to ~8 MB, then `more: true`), truncating only
at an op boundary so contiguity holds.

## HTTP hardening

Go's zero-value `http.Server` has no timeouts at all. Required: `ReadHeaderTimeout: 5s`,
`ReadTimeout: 60s`, `WriteTimeout: 120s`, `IdleTimeout: 60s`, `MaxHeaderBytes: 16KB`; bounded
in-flight requests and per-IP connections; panic-recovery middleware that cannot take down the
sequencer. Reject `Content-Encoding: gzip` or bound the decompressed size. Validate `Host` and reject
any `Origin` header on protocol routes (the real client never sends one) to close browser-driven and
DNS-rebinding paths. `/healthz` returns a bare `200 ok` — no version, id, or counts — and no
versioned `Server` header.

## Bind policy

Classify with `net/netip` — `IsLoopback`, `IsPrivate`, `IsLinkLocalUnicast`, plus an explicit
`100.64.0.0/10` for Tailscale. **Never string-match `0.0.0.0`**: `::`, `[::]`, a bare `:8787` (Go's
dual-stack wildcard), and hostnames resolving to global SLAAC addresses all bypass it. Refuse any
wildcard and any global unicast without `--insecure-public-bind`.

`--allow-cidr` (default RFC1918 + CGNAT + link-local) is enforced **at accept time**, closing
disallowed connections before reading a byte. That is what protects the laptop that moves from home
to a cafe — the bind address is stable, the network is not.

Container caveat, documented prominently: the published image must bind the wildcard to be reachable,
so the guard is permanently overridden there, and `docker run -p 8787:8787` publishes on all
interfaces via DNAT rules that **bypass ufw and firewalld**. Document `-p 127.0.0.1:8787:8787`, and
have the image refuse to start without `CMEMLAN_ALLOW_CIDR`.

## Discovery

Advertise `_cmemlan._tcp.local` via **`brutella/dnssd`** — actively maintained, with conflict
probing, goodbye packets, and interface-change handling, all of which matter for a laptop that
changes networks. (`grandcat/zeroconf`, the usual choice, has had no functional commit since January
2023.) In-process mDNS coexists with `avahi-daemon` and macOS `mDNSResponder` on port 5353 because Go
sets `SO_REUSEADDR`/`SO_REUSEPORT` on multicast sockets; document that this rests on socket-option
grace. Windows multicast is the known weak spot — either test it or have `connect` require an
explicit URL there.

**Advertise only when bound to a non-loopback address**, support `--no-mdns`, default the instance
name to a random per-hub label with `--advertise-name` to override, and **do not publish the hub id**
in TXT — it is the routing partition id, and broadcasting it means an attacker needs only the PSK.

Multicast does not cross Tailscale or route between subnets: same-LAN discovery works, cross-network
setups pass the hostname explicitly.

## Auth and pairing

The hub is the identity boundary; no Claude Code credentials are read.

- First `serve` generates a hub id and a 32-byte PSK, stored `0600` in a `0700` data directory
- `user_id` = hub id; every request needs `Authorization: Bearer <psk>`, compared constant-time
- Pairing codes are ≥128 bits if copy-pasted, or short **only** with hard limits: five failures
  destroys the window, a global 1/s cap on `POST /pair`, and constant-time comparison of the code
- `pair` prints a hub-key fingerprint alongside the code; `connect` displays the fingerprint it
  received and requires the user to confirm it matches. This is the only defense against a rogue mDNS
  advertiser relaying to the real hub — **the PSK authenticates client→hub, never hub→client**
- `POST /pair` requires `Content-Type: application/json` and rejects requests bearing `Origin`
- Per-device tokens recorded in `devices` with `revoked_at`; `cmemlan devices`, `cmemlan revoke`,
  and `cmemlan rotate-token`. Rotation is the operational floor — without it a leaked PSK means
  wiping the data directory, which forces a new epoch and a full replay everywhere

## CLI surface

| Command | Behavior |
|---|---|
| `serve` | Run the hub. `--bind`, `--allow-cidr`, `--data-dir`, `--no-mdns`, `--record <dir>`, `--install-service`, `--uninstall-service`, `--print-unit`. |
| `pair` / `devices` / `revoke` / `rotate-token` | Pairing window; list, revoke, rotate. |
| `connect [url]` | Discover or take a URL, confirm fingerprint, pair, write sync config, restart the worker, offer backfill. |
| `backfill` | Requeue locally-authored rows. `--dry-run`, `--project`, `--since`, `--undo`, `--force`. |
| `status` / `doctor` | Sync state and diagnosis. |
| `version` | Stamped via `-ldflags -X`; needed by `doctor` and bug reports. |

Stdlib `flag` stops parsing at the first positional argument, so `connect http://x --code Y` would
silently drop `--code`. Reorder arguments before parsing. Flags plus `CMEMLAN_*` env only — no config
file. Logging via `log/slog` with `--log-level`/`--log-format`.

**Data directory**, defined once and passed explicitly to generated service units (otherwise
`--install-service` and a manual `serve` land on two different databases): `$XDG_DATA_HOME/cmemlan`
(default `~/.local/share/cmemlan`), `~/Library/Application Support/cmemlan`,
`%LocalAppData%\cmemlan`, overridable via `--data-dir`/`CMEMLAN_DATA_DIR`. The database and its
`-wal`/`-shm` files are chmod `0600` explicitly after creation — SQLite otherwise creates them
world-readable under a default umask.

`--install-service` must run `loginctl enable-linger`, or the systemd *user* unit stops at logout and
presents as "sync is broken." Ship a hardened unit: `ProtectSystem=strict`,
`ReadWritePaths=<data dir>`, `NoNewPrivileges`, `PrivateTmp`, `RestrictAddressFamilies`,
`SystemCallFilter=@system-service`, `MemoryMax`, empty `CapabilityBoundingSet` — `serve` needs
neither the client database nor the settings file, and that one file bounds the blast radius of both
a dependency compromise and any hub RCE.

## Sync configuration

Six environment variables, resolved by the client as file-then-env-override
(`loadFromFile(<dataDir>/settings.json)` then `applyEnvOverrides`):

`CLAUDE_MEM_CLOUD_SYNC_HUB_URL`, `_TOKEN`, `_USER_ID` (all three non-empty activates sync),
`_DEVICE_ID`, `_DEVICE_NAME`, `_WS`.

**Write to `~/.claude-mem/settings.json`, not `~/.claude/settings.json`.** The claude-mem file is
flat-schema and already `0600`; Claude Code's is `0664` under a default umask, is injected into the
environment of every hook, MCP server, and Bash command Claude Code runs (readable via
`/proc/<pid>/environ`), and is frequently committed to dotfiles repos. The claude-mem file is also
more reliable: the Claude Code env block only reaches a worker spawned by a Claude Code process that
applied it, so a worker started by systemd or a plain shell would never see it and `connect` would
report success while sync stayed off. Because env overrides file, `doctor` must check **both**
locations and report a stale variable in Claude Code's settings shadowing the good value.

**`CLAUDE_MEM_CLOUD_SYNC_DEVICE_ID` must be preserved on every rewrite.** The client mints it on
first activation and writes it back. It is baked into every content entity id, so dropping it gives
every local row a new identity: full re-upload, dedupe defeated, old entities orphaned.

Writes are read-modify-write preserving unknown keys (`map[string]json.RawMessage`), refuse to
proceed if the existing file does not parse, go to a temp file in the same directory followed by
`os.Rename`, re-stat before rename and abort if the file changed underneath, `chmod 0600`, and leave
a timestamped backup. `connect --undo` restores it.

## Backfill

Existing memories predate the launch baseline and are marked synced, so they never upload.

```sql
UPDATE observations      SET synced_at = NULL WHERE origin_device_id IS NULL;
UPDATE session_summaries SET synced_at = NULL WHERE origin_device_id IS NULL;
UPDATE user_prompts      SET synced_at = NULL WHERE origin_device_id IS NULL;
DELETE FROM sync_launch_exclusions;   -- scoped to match the UPDATEs
```

Safety properties: `sync_rev` was created `TEXT NOT NULL DEFAULT '1'`, so every legacy row has a
valid positive revision and none is quarantined; `origin_device_id IS NULL` restricts the requeue to
locally-authored rows; re-running costs bandwidth only, because the hub dedupes.

Required safeguards, because this mutates another application's live database and the DELETE destroys
state migration v47 will never regenerate:

- `VACUUM INTO '<data-dir>/backfill-backup-<ts>.db'` first — a consistent snapshot that works against
  a live writer, unlike a file copy. Print the restore command
- Copy `sync_launch_exclusions` into a cmemlan-owned table in the same transaction; `--undo` restores
- All statements in one `BEGIN IMMEDIATE` transaction with a busy timeout
- **Refuse to run while the worker is alive** unless `--force` (check `~/.claude-mem/worker.pid`)
- Scope the DELETE to the same predicate as the UPDATEs, so `--project`/`--since` do not carry a
  wider irreversible side effect than the visible change
- Assert expected `schema_versions` state; refuse on anything unrecognized
- **Honor `CLAUDE_MEM_EXCLUDED_PROJECTS`** — without it, backfill uploads memories from projects the
  user deliberately excluded from capture
- `--dry-run` reports per-project counts **and bytes to be uploaded**

Fragility to watch: backfill depends on the flusher's predicate and on `sync_launch_exclusions` not
becoming a filter in that path. A fixture test against a real database pins this.

## Failure modes

| Situation | Behavior |
|---|---|
| Hub down | Client queues locally, retries with backoff. Nothing lost. |
| Hub data wiped | New epoch → clients reset cursors, replay from 0, dedupe absorbs duplicates. |
| Hub restored from an older backup | **Fatal without an epoch bump** — clients poll above `head_seq` forever with no error. Any discontinuity must bump the epoch. |
| Malformed push | `400` → that op blocks the outbox permanently. Never reject what a conforming client can send. |
| Bad PSK | Indistinguishable from an outage to the client; silent retry forever. `doctor` must diagnose it. |
| Duplicate push after a dropped response | Original sequence re-acked; no duplicate log entry. |
| Two devices edit the same entity | Distinct `entity_rev`s; both reach the log; the client's stale-revision check decides. |
| Clock skew | `server_ts` is informational; ordering comes from sequences. |
| Disk full | Free-space check before accepting a push; `507` below a floor. The hub shares a disk with the user's live memory database, so its failure must not become claude-mem's. |
| Sequence exhaustion | Errors at 2^63−1 rather than wrapping. |

## Testing

1. **Unit** — validation tables, digest handling (pin base64url vs standard base64, 43-char unpadded,
   explicit hex rejection), decimal parsing and comparison.
2. **Property** (`pgregory.net/rapid`, an explicit test-only exception to the dependency budget) —
   concurrent pushes with rollback injected through a `func(tx) error` seam in `store`; assert the
   log is always contiguous `1..head`, that dedupe never consumes a sequence, and that
   `meta.head_seq == MAX(ops.seq)` survives a kill mid-transaction.
3. **Contiguity across pagination** — page `/changes` with a small `limit` while a concurrent push
   commits mid-pagination; assert every seq is previous+1 across page boundaries. This is the exact
   scenario that wedges a client forever, and it is currently the least-tested constraint.
4. **Golden replay, split so it is honest about what is evidence:**
   - *(a) regression net, CI:* replay against our Go validators. This tests our reading of the
     client, not the client.
   - *(b) evidence, local and scheduled:* a `//go:build clientvalidate` test shelling out to `node`
     with a harness importing claude-mem's real validators.
   Byte-verbatim assertions use `bytes.Equal` on the raw response — a JSON-equality helper passes on
   exactly the re-escaping we are guarding against.
5. **End-to-end** — the earlier assumption that CI cannot run one is worth retesting: claude-mem's
   sync worker may need only the six variables and a database with unsynced rows, not an
   authenticated Claude account. **Verify this early**; if it holds, a CI job with node + claude-mem +
   our hub retires most of the protocol-inference risk. Highest-value change available to this plan.
6. **Backfill against a real claude-mem database fixture**, including a worker-running-concurrently
   case.
7. **Fuzz** — round-trip property: for arbitrary input, if `Validate` accepts then
   `Emit(Store(Parse(b)))` equals `b` byte for byte. Also the decimal parser and base64url decoder.
   Seed corpora run in CI; real fuzzing on a schedule.
8. **Auth** — missing/wrong bearer → 401, user-id mismatch → 403, pairing brute-force limits.

### Fixture capture

`serve --record` captures real traffic, which means observation narratives, full prompt text, project
paths, usernames, and `Authorization` headers. The documented workflow would publish the maintainer's
private memory to a public repo, permanently, in git history.

Therefore: raw captures write `0600` into a gitignored `testdata/captures-local/`, and `--record`
refuses to write inside a git work tree without an explicit flag. `cmemlan fixtures scrub` produces
committable fixtures by replacing bodies with synthetic content of the same shape and length,
recomputing `operation_sha256`/`payload_sha256`, and stripping identity headers. CI fails on any
`Bearer` string, `/home/`, `/Users/`, or hostname in `testdata/fixtures/`, and a pre-commit hook does
the same locally.

## Repository

```
cmd/cmemlan/                main, subcommand dispatch
internal/{proto,store,hub,discover,pair,clientdb,settings,cli}/
testdata/fixtures/          scrubbed, committable
testdata/captures-local/    gitignored raw captures
docs/                       protocol.md, architecture.md, security.md,
                            backfill.md, testing.md, troubleshooting.md
.claude-plugin/             marketplace.json
plugin/                     plugin.json + skills/claude-mem-lan-sync/SKILL.md
.github/workflows/          ci.yml, release.yml
Dockerfile · install.sh · README.md · CLAUDE.md · LICENSE
```

**CI:** `go vet`, golangci-lint v2 (its own `version: 2` config), `go test -race` in a job with
`CGO_ENABLED=1` kept separate from release builds, `govulncheck`, `gosec` (G112 catches the missing
`ReadHeaderTimeout`), `goreleaser check`, manifest and skill-frontmatter validation, and the fixture
secret scan.

**Release:** GoReleaser v2 — `version: 2` is a required top-level key; `dockers_v2` for the ghcr
multi-arch image; explicit `env: [CGO_ENABLED=0]` (it defaults to 1 on a native Linux build, which
dynamically links libc and breaks a scratch image); `flags: [-trimpath]`;
`-ldflags "-s -w -X main.version={{.Version}}"`; `actions/attest-build-provenance` and cosign keyless
signing over `checksums.txt`; actions pinned by commit SHA; minimal `permissions:` with
`id-token: write` only in the release job. `install.sh` **verifies the published checksum** —
otherwise generating checksums is theater — documents the download-then-verify two-step rather than
only `curl | sh`, restarts an installed service after replacing the binary, and strips
`com.apple.quarantine` on macOS (binaries are unsigned and unnotarized; state this).

**Dependabot** covers actions and modules but **must ignore `modernc.org/libc`** — it requires the
exact version pinned by `modernc.org/sqlite`, and an independent bump breaks the build. Never
`go get -u ./...`.

## Claude Code plugin marketplace

The repository doubles as a marketplace so an agent can install the setup skill and drive
installation correctly.

```
.claude-plugin/marketplace.json          marketplace manifest (owner: andrhamm)
plugin/.claude-plugin/plugin.json        plugin manifest (name: claude-mem-lan-sync)
plugin/skills/claude-mem-lan-sync/SKILL.md
```

```
/plugin marketplace add andrhamm/claude-mem-lan-sync
/plugin install claude-mem-lan-sync@andrhamm
```

Skill frontmatter must fire whenever memory sync comes up:

```yaml
---
name: claude-mem-lan-sync
description: Set up, verify, or troubleshoot self-hosted LAN sync for claude-mem — sharing Claude
  Code memory across machines without cmem.ai cloud sync. Use whenever claude-mem, Claude Code
  memory, cross-device or multi-machine memory sync, CLAUDE_MEM_CLOUD_SYNC_* variables, cmem.ai,
  or "sync my memories between computers" come up, including install, pairing, backfill, and
  diagnosing sync that is not working.
---
```

Body: choose a host; install; `serve --install-service`; `pair`; `connect --code` elsewhere;
`backfill --dry-run` then `backfill`; verify with `doctor`. Plus the failure playbook and the rules an
agent must not break — never bind a wide interface unasked, never backfill while the worker is
writing, never fall back to recommending cmem.ai. The skill runs `cmemlan doctor` first and acts on
its output, keeping diagnosis in the binary where it is testable.

## CLAUDE.md invariants

- The `body` column holds the JSON **string literal including quotes**; it is never passed to
  `json.Marshal`. Responses are assembled by writing bytes.
- Sequences are gapless and start at 1; `AUTOINCREMENT` is prohibited.
- Ack the digest the client sent, never a stored one. Ack every received op exactly once.
- Never admit two bodies at the same `(entity_id, entity_rev)`.
- Never filter a device's own ops out of its pull; never prune mid-log.
- Any sequence discontinuity bumps the epoch. Rotating the PSK does not.
- Digests are base64url, unpadded, 43 chars — never hex.
- Sequences, revisions, epochs, and `server_ts` are decimal strings end to end.
- Rejecting a push wedges the client permanently: validate minimally.
- Never log op bodies, pairing codes, or the PSK.
- Re-capture and **scrub** fixtures when claude-mem updates; `doctor` warns on untested versions.
- Dependency budget: stdlib, `modernc.org/sqlite`, `brutella/dnssd`, `pgregory.net/rapid`
  (test-only). Anything else needs justification. Never bump `modernc.org/libc` directly.

## Compatibility policy

Compatibility is derived from observed behavior of claude-mem 13.15.0, not a published specification.
The README says so, along with the project being independent of claude-mem and cmem.ai. `doctor`
reports the installed version and warns outside the tested range; `docs/protocol.md` records which
version each detail was verified against.

## Security posture

Threat model: a trusted home network or tailnet. Stated plainly in the README:

- Bodies are plaintext at rest on the hub and in transit over HTTP on the LAN. "Private" means
  private to your network, not encrypted
- Anyone who can read the hub database can read every device's memories
- The PSK is a password; pairing is the only enrollment path
- Do not run this on a VPS. Behind Tailscale is the supported remote story — tailnet traffic is
  WireGuard-encrypted, so plain HTTP over it is fine. Do **not** use self-signed TLS: the client's
  Node `fetch` rejects it without `NODE_EXTRA_CA_CERTS`

Accurate privacy wording, avoiding overclaim:

> cmemlan has no analytics and no crash reporting, and connects to no host other than the hub you
> configure. When discovery is enabled it sends and answers mDNS multicast on the local link;
> `--no-mdns` disables it. It performs no update checks. Note that claude-mem itself sends anonymous
> usage metadata (install id, counts, versions — not memory content) to PostHog by default; set
> `DO_NOT_TRACK=1` if you don't want that. cmemlan neither enables nor disables it.

On cmem.ai, claim only what is verifiable: Cloud Sync transmits observation and prompt bodies to a
third-party host; this hub keeps them on your network.

## Deferred

- **WebSocket endpoint** — phase 2, `advance` frames only. Contract recorded above.
- **Metrics** — Prometheus behind a flag.
- **Discovery beyond the LAN** — mDNS cannot cross Tailscale; a static peer list is the likely answer.

## Blocked (not deferred)

- **Retention and compaction.** In-place pruning breaks `requireContiguous` retroactively and would
  wedge every client. Reclaiming space is only legal alongside an epoch bump, which forces a full
  replay everywhere. `--max-db-bytes` with a `507` floor is the safety valve instead.

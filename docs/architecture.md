# Architecture

One binary, two roles. `serve` is the hub; every other command configures or inspects a machine that
talks to one. They share nothing but the wire format.

## The hub

```
HTTP  ──►  middleware  ──►  handlers  ──►  store  ──►  SQLite (WAL)
           auth              emit bytes     sequencer
           limits                           dedupe
           host/origin
```

| Package | Responsibility |
|---|---|
| `internal/proto` | Wire types, validation, digests, byte-exact emission. Depends on nothing internal. |
| `internal/store` | The operation log, gapless sequencer, device registry. |
| `internal/hub` | Routing, authentication, hardening, bind policy. |
| `internal/pair` | Pre-shared key, fingerprints, pairing windows. |
| `internal/discover` | mDNS advertising and browsing. |

### The log

```sql
ops(user_id, seq, entity_id, entity_rev, kind, origin_device_id,
    operation_sha256, body, server_ts)      PRIMARY KEY (user_id, seq)
UNIQUE INDEX (user_id, entity_id, entity_rev)

meta(user_id, epoch, head_seq)
devices(user_id, device_id, device_name, first_seen, last_seen, revoked_at)
```

`body` holds the JSON string literal exactly as received, quotes included. Nothing re-encodes it.

### The sequencer

One writer, one connection, `BEGIN IMMEDIATE`. Each push reads `head_seq`, looks up existing
`(entity_id, entity_rev)` pairs, assigns numbers only to the misses, inserts, updates the counter, and
commits. A rollback assigns nothing, so the log never develops a gap.

`AUTOINCREMENT` is prohibited because it burns values on rollback — and a gap stalls every client
permanently.

Serialisation is a mutex in the handler goroutine rather than a dedicated writer goroutine: there is
no batching win (a push is already a batch), and a channel design adds a requester-gave-up case and
awkward shutdown ordering.

### Epochs

The epoch is a positive 63-bit decimal. It changes only when the sequence space becomes
discontinuous — a wipe, or a restore from an older backup, which `Open` detects by comparing
`head_seq` against `MAX(seq)`. A changed epoch tells clients their cursors are void and a full replay
is needed; dedupe absorbs the resulting duplicates.

Rotating the pre-shared key deliberately does **not** touch the epoch.

## The client side

| Package | Responsibility |
|---|---|
| `internal/clientdb` | Reads and carefully modifies claude-mem's database. |
| `internal/settings` | Read/modify/write claude-mem's flat settings file. |
| `internal/paths` | Resolves every location once, so a service unit and a manual run agree. |
| `internal/cli` | Command dispatch; all I/O arrives through `Env`. |

Sync configuration is written to `~/.claude-mem/settings.json`, not Claude Code's settings. That file
is already `0600`, is read directly by the worker regardless of who started it, and is not injected
into the environment of every hook and MCP server. Environment variables still override it, so
`doctor` checks both places and reports a stale value shadowing a good one.

## Data flow

1. A memory is written; claude-mem's flusher drains rows where `synced_at IS NULL AND
   origin_device_id IS NULL`.
2. `POST /v1/sync/ops` → the hub validates, assigns contiguous sequences, acks with them.
3. The client stamps `synced_at` from the acks.
4. Another device polls `GET /v1/sync/changes?since=<cursor>`, applies with `ON CONFLICT DO NOTHING`,
   and advances its cursor.

The hub keeps no per-device sync state. Both directions are driven by the client's own cursor, which
is why a hub restart or a new device needs no coordination.

## Failure behaviour

| Situation | What happens |
|---|---|
| Hub down | Clients queue locally and retry with backoff. Nothing is lost. |
| Hub wiped | New epoch → clients replay from zero → dedupe absorbs it. |
| Restored from an older backup | Detected at open; the epoch rotates so clients replay. |
| Malformed push | `400`, and that op blocks the device's outbox — hence minimal validation. |
| Bad key | Indistinguishable from an outage to the client; `doctor` names it. |
| Disk nearly full | `507` before accepting more, so the hub does not take claude-mem down with it. |
| Two devices edit one entity | Distinct revisions; both reach the log; the client's stale check decides. |

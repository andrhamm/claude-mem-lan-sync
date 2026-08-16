# Working on cmemlan

A self-hosted LAN sync hub for claude-mem. Go 1.26, single binary, SQLite.

## Invariants

Each of these exists because breaking it wedges a client permanently, silently, or both. None is
stylistic.

1. **The `body` column holds the JSON string literal, quotes included, and is never passed to
   `json.Marshal`.** `json.Marshal` re-escapes `<`, `>` and `&` even inside a `json.RawMessage`, and
   the client re-canonicalises every body it receives and compares it to the raw string. Responses
   are assembled by writing bytes. `json.RawMessage` is verbatim on decode but *not* on encode — that
   asymmetry is the trap.
2. **Sequences are gapless and start at 1.** `AUTOINCREMENT` is prohibited: it burns values on
   rollback. Allocate from the counter row inside the same `BEGIN IMMEDIATE` transaction as the
   insert.
3. **Ack the digest the client sent, never one read from storage**, and ack every received op exactly
   once. A duplicate appearing twice in one push gets two acks carrying the same sequence.
4. **Never admit two different bodies at the same `(entity_id, entity_rev)`.** First write wins.
5. **Never filter a device's own ops out of its pull**, and never prune from the middle of the log.
   Both create gaps. The client discards its own ops itself, after checking contiguity.
6. **Any sequence discontinuity bumps the epoch. Rotating the pre-shared key does not.** Conflating
   them forces a needless full replay across every device.
7. **Digests are base64url, unpadded, 43 characters.** Not hex, not standard base64.
8. **Sequences, revisions, epochs and `server_ts` are decimal strings end to end**, via `proto.Dec`.
   A JSON number where a string belongs makes the client throw mid-page and retry forever.
9. **Lone surrogates are replaced with U+FFFD, matching Node.** The client hashes them that way, so
   rejecting one would 400 a body it considers valid — and a 400 blocks that device's outbox forever.
10. **Rejecting a push is expensive.** There is no dead-letter path for HTTP failures: a rejected op
    stays at the head of the outbox and blocks everything behind it. Validate minimally; reject only
    what the hub cannot store or route.
11. **Never log op bodies, pairing codes, or the pre-shared key.** Everything relayed here is the
    user's private memory. Error responses carry a fixed reason enum and nothing else, because the
    client copies the first 200 bytes into its own log on another machine.
12. **Re-capture and scrub fixtures when claude-mem updates.** Raw captures contain real prompts.

## Layout

```
cmd/cmemlan/            entry point
internal/proto/         wire types, validation, digests, emission — imports nothing internal
internal/store/         SQLite log, sequencer, devices — imports proto
internal/hub/           HTTP handlers, bind policy — imports proto, store
internal/pair/          pre-shared key, pairing windows
internal/discover/      mDNS
internal/clientdb/      claude-mem's database (backfill)
internal/settings/      claude-mem's settings file
internal/cli/           command dispatch; everything injectable arrives through Env
```

Dependency direction is one-way: `proto` depends on nothing internal, `store` and `hub` depend on
`proto`, and `discover`/`pair` depend on neither `store` nor `hub`.

## Dependencies

Budget: stdlib, `modernc.org/sqlite`, `github.com/brutella/dnssd`, `pgregory.net/rapid` (test only).
Anything else needs a reason.

**Never bump `modernc.org/libc` on its own** — it must match the version pinned by
`modernc.org/sqlite`, and Dependabot is configured to ignore it. Do not run `go get -u ./...`.

## Testing

```bash
make test     # unit, property, golden
make race     # CGO_ENABLED=1, needed for -race
make e2e      # real claude-mem worker against a real hub; needs node + claude-mem
make fuzz     # the byte-preservation round trip
```

The end-to-end tier is the evidence tier: everything else asserts against our reading of the client,
that one asserts against the client. It needs no Claude account — replication works with only the six
`CLAUDE_MEM_CLOUD_SYNC_*` variables and a row with `synced_at IS NULL`.

Byte-fidelity assertions use `bytes.Equal` on the raw response. A JSON-equality helper passes on
exactly the re-escaping invariant 1 guards against.

## SQLite specifics

- DSN pragmas use modernc's syntax (`?_pragma=journal_mode(WAL)`), and **unknown DSN keys are
  silently ignored** — a mattn-style DSN yields a rollback-journal database that looks fine until it
  deadlocks. `store.Open` verifies `PRAGMA journal_mode` and fails hard.
- `_txlock=immediate` is mandatory: the sequencer reads then writes, and SQLite cannot honour
  `busy_timeout` on a lock upgrade.
- Never pass an HTTP request context into a write transaction. modernc implements neither
  `driver.Validator` nor `driver.SessionResetter`, so a client hang-up discards the connection.
- Use single quotes for SQL string literals; SQLite accepts double-quoted ones as a fallback, turning
  a mistyped column name into a silent string.

## Things that surprised us

Recorded so nobody rediscovers them the hard way.

- `worker.pid` contains JSON, not a bare integer.
- `user_prompts` has no `project` column; it reaches one through `sdk_sessions`.
- claude-mem's settings file is flat, `0600`, and env vars override it.
- The client mints `CLAUDE_MEM_CLOUD_SYNC_DEVICE_ID` and writes it back itself. It is baked into
  every content entity id, so losing it re-uploads everything under new identities.
- With no active Claude Code session the client polls every 300s, so tests restart the worker instead
  of waiting.
- `mutation` is a fourth op kind alongside the three content kinds.

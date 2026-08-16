# The claude-mem sync protocol

Reverse-engineered from claude-mem **13.15.0** and verified end to end against a real worker. This
is not a published specification; every claim here was checked against the client's own validators,
and the ones marked **verified live** were additionally confirmed by driving a real worker against a
hub.

## Endpoints

```
GET  /v1/sync/status
POST /v1/sync/ops
GET  /v1/sync/changes?since=<seq>&limit=<n>
GET  /v1/sync/ws                              (optional; see WebSocket below)
```

Every request carries:

```
Authorization: Bearer <token>
X-User-Id: <user id>
X-Device-Id: <uuid the client mints>
X-Device-Name: <display name>          (falls back to the hostname, so effectively always present)
```

**Verified live**: the exact header set above, `limit=500` on every pull, and `since=0` on a first
sync.

## Constants

| | |
|---|---|
| `protocol_version` | `2` — a JSON number, checked on `/status` and `/changes` |
| `body_schema_version` / `payload_schema_version` | `1` / `2` |
| Max op body | 256,000 UTF-8 bytes |
| Max push request | 4,000,000 bytes |
| Ops per push | 200 in practice (one drain batch); client cap 500 |
| Pull page | always 500, up to 40 pages per cycle |
| Content kinds | `observation`, `summary`, `prompt` |
| Fourth kind | `mutation` — `set_title`, `set_prompt_session`, `remap_project` |
| Decimal strings | `^(?:0|[1-9][0-9]*)$`, ≤ `18446744073709551615` |
| Digest | base64url SHA-256, 43 chars, unpadded |
| `origin_device_id` | ≤128 bytes |
| Poll cadence | 30s active, 300s idle, suspends after 1h |
| Debounce | 1500ms, or 250ms while a WebSocket is live |

**Every sequence, revision, epoch and `server_ts` is a decimal string on the wire, never a JSON
number.**

## Operations

An operation wrapper has exactly two keys:

```json
{ "body": "<canonical JSON string>", "operation_sha256": "<base64url digest of body>" }
```

`body` decodes to exactly twelve keys, sorted:

```
body_schema_version, deleted, deleted_at, entity_rev, id, kind, mutation,
origin_device_id, origin_local_id, payload, payload_schema_version, payload_sha256
```

A real observation push, captured live:

```json
{"protocol_version":2,"ops":[{
  "body":"{\"body_schema_version\":1,\"deleted\":false,\"deleted_at\":null,\"entity_rev\":\"1\",\"id\":\"observation:Xnk4YXtYbz0Jr8TQZLoAlwVUpABoXokKpQhqtZm4B7s\",\"kind\":\"observation\",\"mutation\":null,\"origin_device_id\":\"94a3962b-…\",\"origin_local_id\":\"1\",\"payload\":{…},\"payload_schema_version\":2,\"payload_sha256\":\"dugjMFCJBg4sBYFK2NgbmYOeUZ9vg5tGk27UB-7X4rc\"}",
  "operation_sha256":"ViM6Gst3CgkMwkrE569lCy-NIyU4zUW_2mRbOUuz0-M"}]}
```

Note that **payload integers are decimal strings too** (`created_at_epoch`, `discovery_tokens`,
`prompt_number`). The hub never inspects payloads, but a fixture generator must reproduce this.

### Canonical form

Sorted keys, plain objects, finite numbers, safe integers only, uint64 values as decimal strings.
The client re-canonicalises every body it receives and compares against the raw string, so **a hub
must return body bytes verbatim**.

### Entity ids

```
content:  <kind>:base64url_sha256(JSON.stringify(
              ["cmem-doc-id-v1","device",kind,origin_device_id,origin_local_id]))
mutation: mutation:<uuid>        lowercase, version [1-8], variant [89ab]
```

A hub need not verify this derivation, but it explains why device-id stability matters: change the
device id and every entity gets a new identity.

### Lone surrogates

`JSON.stringify` emits unpaired surrogates as `\udXXX`, and Node's UTF-8 encoder replaces them with
U+FFFD when hashing. So the client's own digest is computed over the replaced bytes:

```
JSON.stringify("a\ud800b")       →  "a\ud800b"
Buffer.from(…, "utf8")           →  61 ef bf bd 62
```

A hub must therefore replace rather than reject, or it will 400 a body the client considers valid.

## `GET /v1/sync/status`

```json
{"protocol_version":2,"epoch":"1","head_seq":"402","projected_seq":"402"}
```

`epoch` must be a **positive decimal string** within uint64 — a UUID is rejected outright.

## `POST /v1/sync/ops`

```json
{"acked":[{"id":"observation:…","kind":"observation","entity_rev":"1",
           "operation_sha256":"…","seq":"403","origin_local_id":"91"}],
 "head_seq":"403","projected_seq":"403"}
```

**`projected_seq` must equal `head_seq` exactly.** `/status` rejects `projected_seq > head_seq` and
`/ops` rejects `head_seq > projected_seq`; equality is the only value satisfying both. Every acked
`seq` must also be ≤ both.

The acknowledgement rules, each of which throws on the client if broken:

1. Every received op is acked exactly once — multiplicity is per op, not per entity.
2. The ack echoes **the client's** `operation_sha256`, never one read from storage.
3. A duplicate appearing twice in one push is acked twice with the same `seq`.
4. Two distinct tuples must not share a `seq`, and one tuple must not carry two.
5. All six fields are mandatory. `origin_local_id` may be `null` but must be **present** — an omitted
   key is `undefined` and fails the type check.
6. Order is irrelevant.

A duplicate of a stored op consumes no sequence and is re-acked with the original, which makes a
retry after a dropped response free.

## `GET /v1/sync/changes`

```json
{"protocol_version":2,"epoch":"1","head_seq":"403","more":false,
 "ops":[{"seq":"403","server_ts":"1755300000000","body":"…","operation_sha256":"…"}]}
```

`server_ts` is a **decimal string or absent**; a JSON number throws, and so does `null`.

**The client applies with `requireContiguous`, anchored to its stored cursor.** The first op of every
page must be exactly `cursor + 1`. A gap throws, the page rolls back, the cursor does not advance,
and that page is retried forever with backoff to 10 minutes. Consequences for a hub:

- **Never filter out the requesting device's own ops.** The client discards them itself, *after* the
  contiguity check.
- **Never prune from the middle of the log.** Reclaiming space is only legal alongside an epoch bump.
- **Any sequence discontinuity — including restoring from an older backup — requires a new epoch.**
  Otherwise clients sit above `head_seq` polling empty pages forever, with no error.
- The first op is `seq "1"`.

## Errors

There is **no dead-letter path for HTTP failures**. Every non-2xx is one generic throw plus backoff,
and the rejected op stays at the head of the outbox blocking everything behind it. A bad token is
indistinguishable from an outage: the client retries silently, forever.

Therefore:

- Validate minimally. Reject only what cannot be stored or routed.
- Never return 3xx — `fetch` cannot follow it.
- Never return 204 or 205 — they pass `res.ok` and then fail JSON parsing.
- Every success is `200` with a JSON body.
- Reject reasons come from a fixed enum and never echo request bytes; the client copies the first 200
  bytes of an error body into its own log on another machine.

### Deliberate deviation

`limit` outside `[1,500]` is **clamped rather than rejected**. The client always sends 500, and
rejecting a value it will simply repeat would stall its pulls for no benefit.

## `X-Sync-Mode`

`X-Sync-Mode: poll` means "do not attempt the WebSocket": the client closes any socket and suppresses
reconnects. Any other value, **including an absent header**, re-enables it.

The client enables WebSockets by default and reconnects with a backoff capped at 60s, so a hub
without `/v1/sync/ws` must send this header or every device retries a 404 once a minute forever.

**Verified live:** the client logs *"Hub is in poll mode (X-Sync-Mode: poll) — socket closed,
reconnects suppressed, HTTP sync continues"*.

## WebSocket (not implemented here)

Recorded for whoever adds it. URL is `hubUrl.replace(/^http/i,"ws") + "/v1/sync/ws"`, authenticated
from the upgrade request alone with the same four headers. Text frames only; a binary frame throws.

- `{"type":"advance","head_seq":"<decimal string>"}` → the client force-pulls over HTTP.
- `{"type":"op","ops":[<change records>]}` → must be internally contiguous and start at ≤ cursor+1.

An optional `epoch` on any frame must match the client's stored epoch. The client pings every 40s and
never checks for a pong.

**Recommendation: implement `advance` only.** It carries no data-loss risk, and the client pulls over
HTTP anyway. HTTP-only operation is genuinely lossless.

## Client-side mechanics worth knowing

- The push queue is `WHERE synced_at IS NULL AND origin_device_id IS NULL ORDER BY id LIMIT 200`.
  Rows that arrived from the hub keep their origin set, which is what prevents echo loops.
- Migration **v47** recorded every pre-existing row in `sync_launch_exclusions` and stamped
  `synced_at`, so memories that predate cloud sync never upload. See [backfill](backfill.md).
- `sync_launch_exclusions` is consulted on epoch change, not by the flusher.
- The client mints `CLAUDE_MEM_CLOUD_SYNC_DEVICE_ID` and writes it into
  `<dataDir>/settings.json` itself.

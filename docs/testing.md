# Testing

## Task 0 spike — does the claude-mem worker sync without Claude credentials?

**Answer: yes.** Run 2026-08-16 against claude-mem 13.15.0 on Linux.

Method: a ~150-line Node stub hub (`test/spike/stub_hub.js`) answering the three sync routes, two
scratch claude-mem workers with isolated `CLAUDE_MEM_DATA_DIR` and non-default
`CLAUDE_MEM_WORKER_PORT`, and one observation row inserted directly into device A's database with
`synced_at IS NULL` and `origin_device_id IS NULL`. No Anthropic API key in the environment;
`CLAUDE_MEM_AUTH_MODE` resolved to `api-key` with an empty key.

Result: device A pushed the row, accepted the ack, and stamped `synced_at`. Device B pulled it and
materialised the observation in its own database. **CI can run a real end-to-end test** — no Claude
account, no model calls. Observation *generation* needs credentials; replication does not.

### What the client actually sends

```
GET  /v1/sync/changes?since=0&limit=500
POST /v1/sync/ops        Content-Type: application/json
GET  /v1/sync/status
```

Headers on every request:

```
authorization: Bearer <token>
x-user-id: <user id>
x-device-id: <uuid minted by the client>
x-device-name: <name>
user-agent: Bun/1.3.10
accept-encoding: gzip, deflate, br, zstd
```

A push body, reformatted for reading (the wire form is compact and its byte order is significant):

```json
{"protocol_version":2,"ops":[{
  "body":"{\"body_schema_version\":1,\"deleted\":false,\"deleted_at\":null,\"entity_rev\":\"1\",\"id\":\"observation:Xnk4YXtYbz0Jr8TQZLoAlwVUpABoXokKpQhqtZm4B7s\",\"kind\":\"observation\",\"mutation\":null,\"origin_device_id\":\"94a3962b-…\",\"origin_local_id\":\"1\",\"payload\":{…},\"payload_schema_version\":2,\"payload_sha256\":\"dugjMFCJBg4sBYFK2NgbmYOeUZ9vg5tGk27UB-7X4rc\"}",
  "operation_sha256":"ViM6Gst3CgkMwkrE569lCy-NIyU4zUW_2mRbOUuz0-M"}]}
```

Confirmed against the spec, previously inferred from reading the bundle:

- Twelve body keys, sorted, exactly as documented
- `origin_local_id` is a decimal **string** (`"1"`)
- **Payload integers are decimal strings too** — `created_at_epoch:"1755374401000"`,
  `discovery_tokens:"10"`, `prompt_number:"1"`. The hub never inspects payloads, but the fixture
  generator must reproduce this
- Content id is `observation:<base64url digest>`
- `limit` is always 500; `since` starts at 0

### Behaviours worth designing against

| Observation | Consequence |
|---|---|
| `X-Sync-Mode: poll` → *"Hub is in poll mode — socket closed, reconnects suppressed, HTTP sync continues"* | The header works as designed; no WebSocket 404 storm in phase 1 |
| `Adopted initial sync hub epoch {epoch=1}` on first contact | Epoch adoption is silent and cursor starts at 0 |
| The client minted a device id and wrote it to `<dataDir>/settings.json` as `CLAUDE_MEM_CLOUD_SYNC_DEVICE_ID` | `connect` must read-modify-write that file and preserve the key |
| The settings file it creates contains **every** default, including `CLAUDE_MEM_DATA_DIR` pointing at the real directory | `connect` must not clobber unrelated keys |
| Logs show `tokenLength=10`, never the token | The client redacts; the hub must too |
| A restart triggers *"kicking post-launch catch-up drain"* and an immediate push | Restart is the reliable way to force a flush in tests |
| Device B's copy carries `origin_device_id` = A's id and a stamped `synced_at` | Echo prevention works; B never re-pushes A's op |
| Push landed ~30 s after startup without a restart | The 30 s active poll governs; tests should restart rather than wait |

### Schema note

`sqlite3 "file:$HOME/.claude-mem/claude-mem.db?immutable=1" .schema` captures the real schema safely
against a live writer; the result is `testdata/clientdb-schema.sql` (25 tables). It includes
`sync_entity_heads`, which the design document does not mention — the client's own per-entity head
tracking. The hub does not need it, but `clientdb` tests must not assume it away.

## Tiers

1. **Unit** — validation, digests, decimal handling.
2. **Property and stress** — deterministic `rapid` model test for the sequencer; a separate `-race`
   stress test for concurrency.
3. **Golden replay** — fixtures replayed through proto+store+hub with `bytes.Equal`. A regression
   net against our own understanding, not evidence.
4. **End-to-end** — the real claude-mem worker against our hub, per the spike above. This is the
   evidence tier. Requires node and a claude-mem install; no Claude account.
5. **Fuzz** — round-trip byte preservation, the decimal parser, the base64url decoder.

## Running the spike again

```bash
node test/spike/stub_hub.js &                  # STUB_PORT, STUB_LOG
export CLAUDE_MEM_DATA_DIR=/tmp/spike/dataA CLAUDE_MEM_WORKER_PORT=37999
export CLAUDE_MEM_CLOUD_SYNC_HUB_URL=http://127.0.0.1:8899 \
       CLAUDE_MEM_CLOUD_SYNC_TOKEN=spiketoken \
       CLAUDE_MEM_CLOUD_SYNC_USER_ID=spikeuser \
       CLAUDE_MEM_CLOUD_SYNC_WS=false
node "$PLUGIN/scripts/bun-runner.js" "$PLUGIN/scripts/worker-service.cjs" start
# seed a row with synced_at IS NULL, then `restart` to force the drain
```

Always use a scratch `CLAUDE_MEM_DATA_DIR` and a port that is not 37700, or the test will attach to
the developer's live memory database.

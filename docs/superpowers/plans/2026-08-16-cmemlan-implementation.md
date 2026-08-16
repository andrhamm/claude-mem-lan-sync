# cmemlan Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `cmemlan`, a single Go binary that serves claude-mem's cloud-sync protocol on a LAN and provides the CLI to point machines at it.

**Architecture:** An append-only operation log in SQLite with gapless sequence numbers, served over three HTTP routes the claude-mem worker already speaks. Op bodies are opaque and byte-preserved end to end. The same binary carries client-side commands that configure claude-mem, requeue history, and diagnose sync.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure Go), `brutella/dnssd` (mDNS), `pgregory.net/rapid` (test-only), stdlib `net/http`, `log/slog`, `flag`.

**Spec:** `docs/superpowers/specs/2026-08-16-claude-mem-lan-sync-design.md`

**Revision 2** — restructured after a three-way plan review. Changes: the end-to-end spike moves to Task 0 (the spec calls it "the highest-value change available to this plan" and it was scheduled fourteenth); Task 1 now produces a tree that compiles; the eleven-command CLI task is dissolved into the tasks that build each command's engine; missing infrastructure (paths, logging, testutil, device registry, shutdown/lockfile, error conventions) gets real tasks; every type named is now defined.

## Global Constraints

- Go 1.26.x; explicit `go` and `toolchain` directives in `go.mod`
- Dependency budget: stdlib, `modernc.org/sqlite`, `brutella/dnssd`, `pgregory.net/rapid` (test-only)
- Never bump `modernc.org/libc` directly; Dependabot ignores it
- **Two body representations, never confused:** `RawLiteral` is the JSON string literal *including its surrounding quotes*, exactly as received — this is what is stored and re-emitted. The decoded form exists only to compute the digest and is discarded.
- **Lone surrogates are substituted with U+FFFD, matching Node** (`Buffer.from(s,'utf8')`), because the client computes its own digest over the substituted bytes. Rejecting them would 400 a valid body and wedge that device permanently.
- Sequences are gapless, start at 1, `AUTOINCREMENT` prohibited
- All sequences, revisions, epochs, and `server_ts` are decimal strings on the wire, via `proto.Dec`
- Digests are base64url, unpadded, 43 chars
- Ingest validation is minimal — a rejected push wedges the client permanently
- Reject reasons come from a fixed enum; no error body ever contains a request byte
- Never log op bodies, pairing codes, or the PSK
- SQL string literals use single quotes; DSN pragmas verified at open
- Every symbol a test touches, including test-only seams, appears in its task's **Interfaces**
- Each task ends: write tests → run and confirm they fail for the right reason → implement → run, `go vet`, commit
- Keep everything local; no remote, no push until the user approves

---

### Task 0: End-to-end spike — does the worker sync without Claude credentials?

No Go code. Resolves the project's largest unknown before anything is built on inference.

**Files:** Create `docs/testing.md`, `test/spike/stub_hub.js`, `test/spike/README.md`

- [ ] Write a ~40-line Node stub hub: `GET /v1/sync/status` returning `{"protocol_version":2,"epoch":"1","head_seq":"0","projected_seq":"0"}`, `POST /v1/sync/ops` logging the request and acking every op with sequential seqs, `GET /v1/sync/changes` returning an empty page. Header `X-Sync-Mode: poll` on all three
- [ ] Create a scratch claude-mem data dir; copy the schema from a real `claude-mem.db`; insert one observation row with `synced_at IS NULL` and `origin_device_id IS NULL`
- [ ] Run the worker against it with `CLAUDE_MEM_DATA_DIR`, `CLAUDE_MEM_WORKER_PORT` (not 37700), the six `CLAUDE_MEM_CLOUD_SYNC_*` variables pointed at the stub, and no Claude credentials in the environment
- [ ] Record in `docs/testing.md`: does it push? What exact bytes arrive — headers, `limit`, ack expectations, reaction to the `/v1/sync/ws` 404? Does it stamp `synced_at` on our ack shape?
- [ ] Save the captured request bodies as the seed corpus for Task 13's fixtures
- [ ] If it does not work without credentials, record the exact failure and mark the `//go:build clientvalidate` node-harness tier as the evidence path instead
- [ ] Commit the findings either way

### Task 1: Foundations — module, paths, logging, CLI shell, CI

Produces a tree that builds and tests from the first commit.

**Files:** Create `go.mod`, `.gitignore`, `LICENSE`, `Makefile`, `cmd/cmemlan/main.go`, `internal/buildinfo/buildinfo.go`, `internal/paths/paths.go` (+test), `internal/logging/logging.go` (+test), `internal/cli/cli.go` (+test), `.github/workflows/ci.yml`

**Interfaces:**
```go
// buildinfo
var Version = "dev" // set via -ldflags -X
func String() string

// paths
func DataDir(override string) (string, error)   // CMEMLAN_DATA_DIR > --data-dir > XDG/platform default
func ClaudeMemDir() string                      // CLAUDE_MEM_DATA_DIR > ~/.claude-mem
func ClaudeCodeSettings() string                // ~/.claude/settings.json, honors CLAUDE_CONFIG_DIR

// logging
func New(level, format string, w io.Writer) *slog.Logger
func Redact(v string) string                    // never emit secrets

// cli
type HubClient interface {
    Status(ctx context.Context, url, userID, token string) (StatusResult, error)
}
type StatusResult struct{ Epoch, HeadSeq, ProjectedSeq string; SyncMode string }
type Env struct {
    Stdout, Stderr io.Writer
    HomeDir, DataDir string
    Hub HubClient
    Now func() time.Time
}
func Run(args []string, env Env) int
```

- [ ] Tests: `DataDir` honors env over flag over default, per platform; `ClaudeMemDir` honors `CLAUDE_MEM_DATA_DIR`
- [ ] Tests: `Run` with no args prints usage and returns 2; `version` prints the stamped version; **arguments are reordered so `connect http://x --code Y` parses** (stdlib `flag` stops at the first positional)
- [ ] Tests: the logger never emits a value passed through `Redact`; a body-sized field is not logged at debug level
- [ ] `Makefile`: `test`, `lint`, `vet`, `build`, `fuzz`
- [ ] `ci.yml`: `go vet`, `go build ./...`, `go test ./...` on push and PR — so no later commit lands unchecked
- [ ] Commit

### Task 2: `internal/proto` — decimals and digests

**Files:** Create `internal/proto/dec.go`, `internal/proto/digest.go`, `internal/proto/errors.go`, tests

**Interfaces:**
```go
type Dec uint64
func ParseDec(s string) (Dec, error)          // canonical only: ^(0|[1-9][0-9]*)$, <= uint64 max
func ParseDecPositive(s string) (Dec, error)  // additionally rejects "0"
func (d Dec) String() string
func (d Dec) MarshalJSON() ([]byte, error)    // always quoted
func (d *Dec) UnmarshalJSON(b []byte) error   // rejects JSON numbers
func (d Dec) Int64() (int64, error)           // rejects > math.MaxInt64 — SQLite INTEGER is signed

func Digest(b []byte) string                  // base64url, unpadded
func ValidDigest(s string) bool               // ^[A-Za-z0-9_-]{43}$

type RejectReason string
const (
    ReasonProtocolVersion RejectReason = "protocol_version"
    ReasonWrapperShape    RejectReason = "wrapper_shape"
    ReasonDigestMismatch  RejectReason = "digest_mismatch"
    ReasonBodyShape       RejectReason = "body_shape"
    ReasonUnknownKind     RejectReason = "unknown_kind"
    ReasonEntityRev       RejectReason = "entity_rev"
    ReasonTooLarge        RejectReason = "too_large"
    ReasonBadCursor       RejectReason = "bad_cursor"
    ReasonUserMismatch    RejectReason = "user_mismatch"
    ReasonUnauthorized    RejectReason = "unauthorized"
    ReasonStorageFull     RejectReason = "storage_full"
)
type RejectError struct{ Reason RejectReason }  // carries no request bytes, by construction
func (e *RejectError) Error() string
```

- [ ] Tests: `"01"`, `" 1"`, `"1.0"`, `"-1"`, `""`, `"1e3"` rejected; `"0"` accepted by `ParseDec`, rejected by `ParseDecPositive`; `ParseUint` overflow rejected; `Int64` rejects above `MaxInt64`
- [ ] Tests: `MarshalJSON` emits `"403"` not `403`; `UnmarshalJSON` rejects `403`, accepts `"403"`
- [ ] Tests: digest is 43 chars base64url; hex and standard-base64 (`+/`) rejected; digest of `"null"` matches the client's mutation constant
- [ ] Tests: `RejectError` has no field capable of carrying request data (compile-time: the struct has exactly one field)
- [ ] Commit

### Task 3: `internal/proto` — wrapper, unquoting, body validation

**Files:** Create `internal/proto/unquote.go`, `internal/proto/wrapper.go`, `internal/proto/body.go`, `internal/proto/emit.go`, tests, `internal/proto/fuzz_test.go`, `internal/proto/node_diff_test.go` (build tag `nodediff`)

**Interfaces:**
```go
type Op struct {
    RawLiteral     []byte  // JSON string literal INCLUDING surrounding quotes, verbatim
    Digest         string
    ID             string
    Kind           string  // observation | summary | prompt | mutation
    EntityRev      Dec
    OriginDeviceID string
    OriginLocalID  *Dec    // nil for mutations; must serialize as null, never omitted
}
func ParseOp(wrapper json.RawMessage) (Op, error)
func UnquoteJSONString(literal []byte) ([]byte, error) // Node semantics: lone surrogates -> U+FFFD
func EmitChangeOp(w io.Writer, seq, serverTS Dec, op Op) error
func EmitAck(w io.Writer, a Ack) error
```

- [ ] Tests: wrapper with a third key rejected; either key missing rejected; digest mismatch rejected; **kind `mutation` accepted** alongside the three content kinds (a mutation op that 400s blocks the outbox permanently — verified: the client has a dedicated `kind === "mutation"` branch)
- [ ] Tests: `UnquoteJSONString` handles `\"`, `\\`, `\/`, `\b\f\n\r\t`, `\uXXXX`, valid surrogate pairs; **a lone surrogate becomes U+FFFD** (`"a\ud800b"` → bytes `61 ef bf bd 62`), matching Node
- [ ] Differential test (`//go:build nodediff`): a corpus of nasty strings hashed by both `node -e` and our path must agree on `operation_sha256`
- [ ] Tests: duplicate JSON keys in the body rejected via a `json.Decoder` token walk, not map length; trailing data after the value rejected
- [ ] Tests: exactly twelve keys; `entity_rev` non-canonical (`"01"`) rejected at parse so the TEXT index can never hold two forms of one revision; body over 256,000 **decoded UTF-8 bytes** rejected (the literal is longer; guard it separately and loosely)
- [ ] Tests: `EmitChangeOp` writes `RawLiteral` byte-for-byte with no re-quoting; `EmitAck` emits `"origin_local_id":null` when nil — asserted on raw bytes
- [ ] Fuzz: for arbitrary input, if `ParseOp` succeeds then emitting `RawLiteral` reproduces the input body bytes exactly
- [ ] Commit

### Task 4: `internal/store` — open, schema, epoch

**Files:** Create `internal/store/store.go`, `internal/store/schema.go`, `internal/store/epoch.go`, tests

**Interfaces:**
```go
func Open(path string, log *slog.Logger) (*Store, error)
func (s *Store) Close() error
func (s *Store) UserID() string            // = hub id; single-tenant, cached at Open
func (s *Store) Epoch() Dec                // cached at Open
func (s *Store) BumpEpoch(reason string) error
func (s *Store) SetTxHook(f func(*sql.Tx) error)   // test-only seam for rollback injection
```

- [ ] Tests: schema created at `user_version = 1`; reopening is idempotent; a newer `user_version` is refused with a clear error
- [ ] Tests: `PRAGMA journal_mode` reports `wal` after open, and open fails loudly otherwise (guards silently-ignored DSN keys)
- [ ] Tests: db, `-wal`, `-shm` are 0600 **after a write** (they do not exist before), directory 0700
- [ ] Tests: `Epoch()` is byte-stable across close/reopen
- [ ] Tests: **if `meta.head_seq != MAX(ops.seq)` at open — a restore from an older backup — the store bumps the epoch and logs loudly rather than serving a stale one.** Without this every client sits above `head_seq` polling an empty result forever
- [ ] Implement: write handle `SetMaxOpenConns(1)` + `_txlock=immediate`; separate multi-conn read handle; epoch = 63 bits from `crypto/rand`
- [ ] Commit

### Task 4b: `internal/testutil`

**Files:** Create `internal/testutil/testutil.go`, `internal/testutil/ops.go`

**Interfaces:**
```go
func TempStore(t *testing.T) *store.Store
func FixedClock(ts time.Time) func() time.Time
func ValidOp(t *testing.T, kind string, deviceID string, localID uint64, rev uint64) json.RawMessage
func MutationOp(t *testing.T, op string, rev uint64) json.RawMessage
type FakeHub struct{ StatusFn func(...) (cli.StatusResult, error) }
```

- [ ] `ValidOp` builds a canonical twelve-key body with sorted keys and a correct digest, so every later task shares one definition of "valid"
- [ ] Tests: `ValidOp` output passes `proto.ParseOp`; `MutationOp` produces `kind:"mutation"`, null payload, and `payload_sha256` equal to the digest of `null`
- [ ] Commit

### Task 5a: `internal/store` — push, dedupe, acks

**Files:** Create `internal/store/ops.go`, tests

**Interfaces:**
```go
type Ack struct {
    ID, Kind string
    EntityRev Dec
    Digest string
    OriginLocalID *Dec
    Seq Dec
}
type PushResult struct{ Acks []Ack; HeadSeq, ProjectedSeq Dec }
func (s *Store) Push(ctx context.Context, ops []proto.Op, deviceID, deviceName string) (PushResult, error)
```

- [ ] Tests: first op is seq 1; sequences contiguous across pushes; `ProjectedSeq == HeadSeq` always
- [ ] Tests: duplicate `(entity_id, entity_rev)` across pushes consumes no sequence and re-acks the original
- [ ] Tests: **the same op twice in one push yields two acks carrying the same seq** (collapsing them is a multiplicity mismatch that wedges the client)
- [ ] Tests: acks echo the **client's** digest; one ack per received op; all six fields set
- [ ] Tests: two ops in one push at the same `(entity_id, entity_rev)` with different digests → first-write-wins, both acked with the stored seq, one WARN naming the entity (the spec's documented exception to seq-uniqueness)
- [ ] Tests: across any ack set, no two distinct tuples share a seq and no tuple carries two seqs
- [ ] Tests: `Push` upserts `devices` (name truncated to 80 bytes, never used as a key); a revoked device is rejected before the transaction opens
- [ ] Tests: sequence allocation refuses above `math.MaxInt64` rather than wrapping
- [ ] Implement: one `BEGIN IMMEDIATE`, dedupe SELECT inside it, sequences assigned only to misses, `context.WithoutCancel` plus a bounded timeout, `sync.Mutex` around the write path
- [ ] Commit

### Task 5b: `internal/store` — changes and pagination

**Files:** Create `internal/store/changes.go`, tests

**Interfaces:**
```go
type ChangeOp struct{ Seq, ServerTS Dec; RawLiteral []byte; Digest string }
type ChangesResult struct{ Ops []ChangeOp; More bool; HeadSeq, Epoch Dec }
func (s *Store) Changes(ctx context.Context, since Dec, limit, maxBytes int) (ChangesResult, error)
```

- [ ] Tests: returns ops strictly after `since`, ascending, contiguous from `since+1`
- [ ] Tests: **never filters by device** — a device's own ops come back to it (filtering them creates gaps and wedges that device)
- [ ] Tests: `More` reflects remaining rows; the byte cap truncates only at an op boundary
- [ ] Tests: page, `head_seq`, and `epoch` are read in **one read transaction**, so `HeadSeq` is never inconsistent with the returned page under a concurrent push; assert `HeadSeq >= last returned Seq`
- [ ] Tests: **paginate with `limit=3` while a goroutine pushes between pages** — concatenated seqs are exactly `since+1…` with no gap or repeat, and `head_seq` never goes backwards. The spec calls this the least-tested critical constraint
- [ ] Commit

### Task 5c: `internal/store` — invariant tests

**Files:** Create `internal/store/model_test.go`, `internal/store/stress_test.go`

- [ ] Deterministic model test (`rapid`): generated push sequences including duplicates, cross-push repeats, and injected rollbacks via `SetTxHook`, applied **serially**; after every step `seq == 1..head` contiguous and `meta.head_seq == MAX(ops.seq)`
- [ ] Separate `-race` stress test: N goroutines × M pushes, same invariants asserted at the end (rapid shrinks by deterministic replay, so concurrency belongs here, not there)
- [ ] Commit

### Task 6a: `internal/hub` — middleware, auth, hardening

**Files:** Create `internal/hub/server.go`, `internal/hub/middleware.go`, `internal/hub/errors.go`, tests

**Interfaces:**
```go
type Authenticator interface{ Verify(userID, token string) bool }
type Options struct {
    UserID string
    Auth Authenticator
    Now func() time.Time
    MaxRequestBytes, MaxBodyBytes, MaxResponseBytes int
    MinFreeBytes int64
    Logger *slog.Logger
    RecordDir string
}
func New(st *store.Store, opts Options) *Server
func (s *Server) Handler() http.Handler
func writeError(w http.ResponseWriter, status int, r proto.RejectReason)
```

- [ ] Tests: missing bearer → 401; wrong bearer → 401 (constant-time compare); `X-User-Id` mismatch → 403 and no partition created
- [ ] Tests: every error body is exactly `{"error":"<enum>"}`; a fuzz pass confirms no request byte, header, or path fragment appears in any error response
- [ ] Tests: the five timeout/limit fields carry the spec's exact values; `Host` outside the allowlist → 400; `Content-Encoding: gzip` → 400; above the in-flight cap → 503; per-IP connection cap enforced; no `Server` header
- [ ] Tests: `/healthz` returns bare `200 ok` with no version, id, or counts
- [ ] Tests: no route ever returns 3xx (including trailing-slash and `//` path forms) and never 204/205; unknown path → 404
- [ ] Tests: `X-Sync-Mode: poll` present on all three protocol routes while the WebSocket is unimplemented
- [ ] Commit

### Task 6b: `internal/hub` — /status and /ops

**Files:** Create `internal/hub/status.go`, `internal/hub/ops.go`, tests

- [ ] Tests (golden, on raw bytes): `/status` emits `protocol_version` as a JSON **number** and epoch/head/projected as decimal **strings**, with `projected_seq` byte-identical to `head_seq`
- [ ] Tests: `/ops` — every acked seq ≤ head_seq; `head_seq == projected_seq` byte-for-byte; an ack with a nil origin_local_id serializes as `null`
- [ ] Tests: request over 4,000,000 bytes → 400 via `MaxBytesReader` before any read; op body over 256,000 → 400; `protocol_version != 2` → 400
- [ ] Tests: below the free-space floor → **507** with a JSON body and nothing written
- [ ] Commit

### Task 6c: `internal/hub` — /changes and byte-verbatim emission

**Files:** Create `internal/hub/changes.go`, tests, `internal/hub/golden_test.go`

- [ ] Tests: `/changes` emits `protocol_version` (number), `epoch` and `head_seq` (decimal strings), `more` (boolean), `server_ts` as a **decimal string** — a JSON number here wedges the client on its first pull
- [ ] Tests: the emitted body for each op is **`bytes.Equal`** to what was pushed — never a JSON-equality helper, which would pass on exactly the re-escaping this guards against
- [ ] Tests: `limit` outside `[1,500]` is clamped (deliberate deviation from the spec's "reject" rule, recorded in `docs/protocol.md` — clamping fits the minimal-validation posture); malformed `since` → 400
- [ ] Tests: an empty result returns `200` with `"ops":[]`, never 204
- [ ] Commit

### Task 6d: End-to-end against the real client (walking skeleton)

Runs only if Task 0 succeeded; otherwise this task builds the `clientvalidate` harness instead.

**Files:** Create `test/e2e/e2e_test.go` (build tag `e2e`), `test/clientvalidate/` (build tag `clientvalidate`)

- [ ] Real claude-mem worker → our hub: a seeded unsynced row is pushed, acked, and `synced_at` stamped
- [ ] A second scratch worker pulls that op and lands it in its own database
- [ ] Assert the worker logs no sync errors and its outbox drains to zero
- [ ] Wire into CI if Task 0 proved credentials are unnecessary; otherwise schedule it
- [ ] Commit

### Task 7: `internal/pair` + device registry + pairing commands

**Files:** Create `internal/pair/pair.go`, `internal/pair/codes.go`, `internal/store/devices.go`, `internal/hub/pair_handler.go`, `internal/cli/pair.go`, tests

**Interfaces:**
```go
func LoadOrCreate(dir string) (*Keys, error)
type Keys struct{ HubID, PSK string }
func (k *Keys) Fingerprint() string
func NewWindow(ttl time.Duration, now func() time.Time) *Window
func (w *Window) Code() string
func (w *Window) Redeem(code string) (psk string, err error)

func (s *Store) ListDevices() ([]Device, error)
func (s *Store) RevokeDevice(id string) error
func (s *Store) RotateToken(newHash string) error
```

- [ ] Tests: PSK is 32 bytes, file 0600, stable across reloads
- [ ] Tests: code is single-use; expires (fake clock, no sleeps); five failures destroy the window; both code and bearer compared constant-time; the `/pair` limiter allows 1/s
- [ ] Tests: different PSKs produce different fingerprints; **`rotate-token` changes the fingerprint and leaves `meta.epoch` byte-identical** (conflating them would force a full replay everywhere)
- [ ] Tests: a revoked device gets 401 on all three protocol routes; `devices` lists what pushed
- [ ] Tests: `/pair` requires JSON content-type and rejects any `Origin`
- [ ] CLI: `pair`, `devices`, `revoke`, `rotate-token`
- [ ] Commit

### Task 8: `internal/discover` — mDNS

**Files:** Create `internal/discover/discover.go`, `internal/discover/txt.go`, tests (live paths behind `//go:build mdns`)

**Interfaces:**
```go
type Config struct{ Enabled bool; InstanceName string; Port int; BindAddr netip.Addr }
type Found struct{ Name, Host string; Port int }
func txtRecords(cfg Config) map[string]string   // pure, unit-testable
func Advertise(ctx context.Context, cfg Config) error
func Browse(ctx context.Context, timeout time.Duration) ([]Found, error)
```

- [ ] Tests (pure): TXT carries `v=1` and the instance name and **not** the hub id (it is the routing partition id); `Enabled=false` makes `Advertise` a no-op; a loopback `BindAddr` disables advertising
- [ ] Tests: default instance name is a random label, not the hostname
- [ ] Live advertise/browse tests gated behind the `mdns` build tag so CI has no multicast flake
- [ ] On `GOOS=windows`, `Browse` is not used by `connect` unless verified on Windows CI — covered by an injected-GOOS test
- [ ] Commit

### Task 9: `internal/settings` + `connect`

**Files:** Create `internal/settings/settings.go`, `internal/cli/connect.go`, tests

**Interfaces:**
```go
func Read(path string) (map[string]json.RawMessage, error)
func Update(path string, kv map[string]string) (backupPath string, err error)
func Restore(path, backupPath string) error
```

- [ ] Tests: unknown keys survive; an unparseable file is never overwritten; temp-file-plus-rename in the same directory; result 0600; timestamped backup returned
- [ ] Tests: **re-stat before rename** — if size/mtime/inode changed underneath, abort without writing
- [ ] Tests: target is `~/.claude-mem/settings.json` honoring `CLAUDE_MEM_DATA_DIR`, flat schema, **not** `~/.claude/settings.json`
- [ ] Tests: `connect` writes `HUB_URL`, `TOKEN`, `USER_ID`, `DEVICE_NAME`, and `WS` — all as JSON **strings**, `WS` as `"false"` (the client tests `!== "false"`) — preserves any existing `DEVICE_ID` and never mints one
- [ ] Tests: `connect` prints the hub fingerprint and refuses to write until confirmed (`--fingerprint` must match exactly); a mismatch aborts non-zero with no write. This is the only defense against a rogue mDNS advertiser relaying to the real hub
- [ ] Tests: `connect --undo` restores the recorded backup
- [ ] Commit

### Task 10: `internal/clientdb` + `backfill`

**Files:** Create `internal/clientdb/clientdb.go`, `internal/clientdb/backfill.go`, `internal/cli/backfill.go`, `testdata/clientdb-schema.sql`, tests

**Interfaces:**
```go
type Counts struct{ Observations, Summaries, Prompts, Mutations, Tombstones int }
type BackfillOpts struct {
    DryRun, Force bool
    Project string
    Since time.Time
    ExcludedProjects []string
    ProcessAlive func(pid int) bool
}
type BackfillResult struct{ PerTable map[string]int; Bytes int64; BackupPath string }
func Open(path string) (*DB, error)
func (d *DB) Pending() (Counts, error)
func (d *DB) LastError() (string, error)
func (d *DB) Backfill(o BackfillOpts) (BackfillResult, error)
func (d *DB) UndoBackfill() error
```

- [ ] `testdata/clientdb-schema.sql` is captured from a **real** claude-mem 13.15.0 database via `.schema` — a hand-built miniature would test our transcription, not the schema
- [ ] Tests: `--dry-run` reports per-table counts and bytes, writes nothing
- [ ] Tests: `VACUUM INTO` backup exists before any mutation; the restore command is printed
- [ ] Tests: nulls `synced_at` only where `origin_device_id IS NULL`; saves exclusions to a cmemlan-owned table; `UndoBackfill` restores both
- [ ] Tests: **`--project X` scopes the DELETE to the same predicate as the UPDATEs** — other projects' exclusions survive; same for `--since`
- [ ] Tests: refuses while the worker pid is live unless `--force` (via `ProcessAlive`); refuses on unrecognized `schema_versions`; honors `CLAUDE_MEM_EXCLUDED_PROJECTS`
- [ ] Tests: the flusher predicate (`synced_at IS NULL AND origin_device_id IS NULL`) selects exactly the rows backfill requeued
- [ ] Commit

### Task 11: `status` and `doctor`

**Files:** Create `internal/cli/status.go`, `internal/cli/doctor.go`, tests

- [ ] Tests (fake hub, fake filesystem root): reports missing env; a stale `CLAUDE_MEM_CLOUD_SYNC_*` in `~/.claude/settings.json` **shadowing** the good value (env overrides file); pending counts; `lastError` and outbox depth — the only place a wedged push surfaces
- [ ] Tests: a hub returning 401 is reported as "bad token", distinct from "unreachable" — the client itself cannot tell these apart
- [ ] Tests: prints the installed claude-mem version and warns outside the tested range (13.15.0)
- [ ] Tests: reports the hub's epoch and head_seq versus the local cursor
- [ ] Commit

### Task 12a: `serve` — bind policy, listener filter, shutdown

**Files:** Create `internal/hub/bind.go`, `internal/cli/serve.go`, tests

**Interfaces:**
```go
func ClassifyBind(bind string, allowPublic bool) (netip.AddrPort, error)
func ParseCIDRList(s string) ([]netip.Prefix, error)  // empty -> RFC1918 + CGNAT + link-local
func AllowCIDR(prefixes []netip.Prefix) func(net.Addr) bool
func FilterListener(l net.Listener, allow func(net.Addr) bool) net.Listener
func Lockfile(dir string) (release func() error, err error)
```

- [ ] Tests: `0.0.0.0`, `::`, `[::]`, and a bare `:8787` are all refused without `--insecure-public-bind`; loopback, RFC1918, and CGNAT accepted; classification is `net/netip`, never string matching
- [ ] Tests: a connection from outside `--allow-cidr` is closed at accept time before any read
- [ ] Tests: a second `serve` on the same data dir exits non-zero with "already running"
- [ ] Tests: on SIGTERM the order is `Shutdown` → close write handle → `PRAGMA wal_checkpoint(TRUNCATE)`, verified by a truncated `-wal`
- [ ] `--max-db-bytes` enforced; over the cap → 507
- [ ] Commit

### Task 12b: service install

**Files:** Create `internal/cli/service.go`, `internal/cli/service_linux.go`, `internal/cli/service_darwin.go`, tests

- [ ] Tests: the generated systemd unit contains the **resolved absolute data dir** (not an inherited env var) and every hardening directive: `ProtectSystem=strict`, `ReadWritePaths`, `NoNewPrivileges`, `PrivateTmp`, `RestrictAddressFamilies`, `SystemCallFilter=@system-service`, `MemoryMax`, empty `CapabilityBoundingSet`
- [ ] Tests: `--print-unit` prints without installing; `--uninstall-service` removes it
- [ ] Tests: install runs `loginctl enable-linger` — without it the user unit dies at logout, presenting exactly as "sync is broken"
- [ ] Tests: launchd plist has `RunAtLoad` and `KeepAlive`
- [ ] Commit

### Task 13: capture, scrub, golden replay

**Files:** Create `internal/hub/record.go`, `internal/cli/fixtures.go`, `test/golden/replay_test.go`, tests

- [ ] Tests: `--record` refuses inside a git work tree without an override (upward `.git` walk, no shelling to git); files 0600 in a 0700 dir; raw captures land in gitignored `testdata/captures-local/`
- [ ] Tests: scrub replaces bodies with synthetic content of the same shape and length, recomputes `operation_sha256` and `payload_sha256`, strips `Authorization`/`X-User-Id`/`X-Device-Id`; the result passes `proto.ParseOp`
- [ ] Golden replay: fixtures captured in Task 0/6d replay through proto+store+hub with `bytes.Equal` assertions. Labelled a **regression net**, not evidence — the evidence tier is the real client (6d)
- [ ] Commit

### Task 14: docs and marketplace

**Files:** Create `README.md`, `CLAUDE.md`, `docs/protocol.md`, `docs/architecture.md`, `docs/security.md`, `docs/backfill.md`, `docs/troubleshooting.md`, `.claude-plugin/marketplace.json`, `plugin/.claude-plugin/plugin.json`, `plugin/skills/claude-mem-lan-sync/SKILL.md`

- [ ] `docs/protocol.md`: the full wire contract, the content-id derivation string, the mutation UUID pattern, the WebSocket frame contract with "implement `advance` only", the `limit`-clamping deviation, and the version each detail was verified against
- [ ] `docs/security.md`: LAN threat model, `-p 127.0.0.1:8787:8787` and the ufw/firewalld DNAT bypass, no self-signed TLS (the client's Node fetch rejects it), the accurate privacy wording including claude-mem's own PostHog default
- [ ] `README.md`: quickstart, independence disclaimer, unsigned/unnotarized binaries note
- [ ] `CLAUDE.md`: the invariants list verbatim, plus "the `body` column holds the JSON string literal including quotes; it is never passed to `json.Marshal`"
- [ ] Marketplace manifests matching the shapes the current loader reads; skill frontmatter triggering on claude-mem, Claude Code memory, cross-device sync, `CLAUDE_MEM_CLOUD_SYNC_*`, cmem.ai; skill body runs `cmemlan doctor` first
- [ ] Commit

### Task 15: full CI, release, packaging

**Files:** Extend `.github/workflows/ci.yml`; create `.github/workflows/release.yml`, `.github/workflows/fuzz.yml`, `.goreleaser.yaml`, `.golangci.yml`, `.github/dependabot.yml`, `Dockerfile`, `install.sh`, `.githooks/pre-commit`

- [ ] `ci.yml`: golangci-lint v2 (its own `version: 2` config), `go test -race` in a separate `CGO_ENABLED=1` job, `govulncheck`, `gosec`, `goreleaser check`, manifest/frontmatter validation, fixture secret scan (`Bearer`, `/home/`, `/Users/`, hostname)
- [ ] `.goreleaser.yaml`: `version: 2`, `env: [CGO_ENABLED=0]`, `-trimpath`, `-ldflags "-s -w -X …buildinfo.Version={{.Version}}"`, `dockers_v2`, checksums, cosign, `actions/attest-build-provenance`
- [ ] Both workflows: top-level `permissions: contents: read`; `id-token: write` only on release; actions pinned by commit SHA
- [ ] `dependabot.yml`: ignore `modernc.org/libc`
- [ ] `Dockerfile`: entrypoint exits non-zero without `CMEMLAN_ALLOW_CIDR`; CI asserts it
- [ ] `install.sh`: verifies the published checksum, documents the download-then-verify path, restarts an installed service, strips `com.apple.quarantine`
- [ ] `fuzz.yml`: scheduled real fuzzing; seed corpora in CI
- [ ] `.githooks/pre-commit`: the fixture secret scan locally
- [ ] Commit

---

## Self-Review

**Spec coverage.** Protocol: Tasks 2, 3, 6a–6c. Storage and sequencer: 4, 5a–5c. Evidence against the real client: 0, 6d, 13. Auth, devices, pairing: 7. Discovery: 8. Settings and connect: 9. Backfill: 10. Diagnostics: 11. Bind, shutdown, service: 12a–12b. Docs and marketplace: 14. CI and release: 15. Deliberately untasked: the WebSocket endpoint, Prometheus metrics, discovery beyond the LAN, and retention/compaction — but the `--max-db-bytes`/507 safety valve **is** tasked (12a, 6b), since it lives in the Blocked section without being blocked.

**Placeholders.** Every task names exact files, defined types, and specific assertions. Types previously named but undefined — `ChangesResult`, `Options`, `Counts`, `BackfillOpts`, `BackfillResult`, `Config`, `StatusResult` — now have declarations.

**Type consistency.** `proto.Dec` covers seq, entity_rev, head_seq, projected_seq, epoch, server_ts, and origin_local_id — including `Ack.EntityRev` and `Op.EntityRev`, which were `string` in revision 1 and would have let `"1"` and `"01"` become two rows for one revision. `proto.Op.RawLiteral` carries quotes and is consumed unchanged by `store` and emitted unchanged by `hub`. Store methods are single-tenant (no `userID` parameter); the user id is compared once, in the hub.

**Known deviations from the spec, deliberate and recorded:** `limit` is clamped rather than rejected (minimal-validation posture); lone surrogates are substituted rather than rejected (matching Node, verified empirically — rejecting would wedge a client on a valid body). Both are documented in `docs/protocol.md`.

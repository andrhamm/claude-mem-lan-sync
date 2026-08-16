# cmemlan Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `cmemlan`, a single Go binary that serves claude-mem's cloud-sync protocol on a LAN and provides the CLI to point machines at it.

**Architecture:** An append-only operation log in SQLite with gapless per-user sequence numbers, served over three HTTP routes the claude-mem worker already speaks. Op bodies are opaque and byte-preserved end to end. The same binary carries client-side commands that configure claude-mem, requeue history, and diagnose sync.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure Go), `brutella/dnssd` (mDNS), `pgregory.net/rapid` (test-only), stdlib `net/http`, `log/slog`, `flag`.

**Spec:** `docs/superpowers/specs/2026-08-16-claude-mem-lan-sync-design.md`

## Global Constraints

- Go 1.26.x; explicit `go` and `toolchain` directives in `go.mod`
- Dependency budget: stdlib, `modernc.org/sqlite`, `brutella/dnssd`, `pgregory.net/rapid` (test-only). Anything else needs justification
- Never bump `modernc.org/libc` directly; Dependabot ignores it
- Body bytes are stored and emitted **verbatim as the JSON string literal**; never `json.Marshal` a body
- Sequences are gapless, start at 1, `AUTOINCREMENT` prohibited
- All sequences, revisions, epochs, and `server_ts` are decimal strings on the wire
- Digests are base64url, unpadded, 43 chars
- Ingest validation is minimal — a rejected push wedges the client permanently
- Never log op bodies, pairing codes, or the PSK
- SQL string literals use single quotes; DSN pragmas verified at open
- Keep everything local; no remote, no push until the user approves

---

### Task 1: Repository scaffolding

**Files:** Create `go.mod`, `.gitignore`, `LICENSE`, `cmd/cmemlan/main.go`, `internal/buildinfo/buildinfo.go`

**Interfaces:** Produces `buildinfo.Version` (string, set via `-ldflags -X`), `buildinfo.String() string`.

- [ ] Init module `github.com/andrhamm/claude-mem-lan-sync`, Go 1.26
- [ ] `.gitignore` covers `testdata/captures-local/`, `dist/`, `*.db`, `cmemlan`
- [ ] MIT LICENSE, author andrhamm
- [ ] `main.go` dispatches to `internal/cli.Run(args, stdout, stderr) int`
- [ ] Commit

### Task 2: `internal/proto` — decimal type and digests

**Files:** Create `internal/proto/dec.go`, `internal/proto/digest.go`, tests for both

**Interfaces:** Produces `type Dec uint64` with `ParseDec(string) (Dec, error)`, `(Dec) String() string`, `MarshalJSON`/`UnmarshalJSON` (quoted decimal), `ParseDecPositive`; `Digest(b []byte) string`, `ValidDigest(string) bool`.

- [ ] Tests: canonical form only (`"01"`, `" 1"`, `"1.0"`, `"-1"`, `""` rejected); `"0"` allowed for `ParseDec`, rejected by `ParseDecPositive`; range rejects `> math.MaxInt64` and `> uint64 max`; `MarshalJSON` emits quoted; `UnmarshalJSON` rejects JSON numbers
- [ ] Tests: digest is base64url unpadded 43 chars, rejects hex and standard-base64 alphabets
- [ ] Implement, run, commit

### Task 3: `internal/proto` — wrapper and body validation

**Files:** Create `internal/proto/wrapper.go`, `internal/proto/unquote.go`, `internal/proto/body.go`, tests, `internal/proto/fuzz_test.go`

**Interfaces:** Produces `type Op struct { RawLiteral []byte; Digest string; ID, Kind, EntityRev, OriginDeviceID string; OriginLocalID *string }`, `ParseOp(raw json.RawMessage) (Op, error)`, `UnquoteJSONString([]byte) ([]byte, error)`.

- [ ] Tests: wrapper with extra key rejected; missing key rejected; digest mismatch rejected; body over 256,000 bytes rejected
- [ ] Tests: `UnquoteJSONString` handles `\"`, `\\`, `\/`, `\uXXXX` surrogate pairs; **rejects** lone surrogates and invalid UTF-8 rather than substituting
- [ ] Tests: duplicate JSON keys in the body rejected (token walk, not map length); trailing data after the value rejected
- [ ] Tests: exactly twelve keys required; unknown kind rejected; non-positive/non-canonical `entity_rev` rejected
- [ ] Fuzz: `Emit(Store(Parse(b))) == b` byte-for-byte whenever `Parse` succeeds
- [ ] Implement, run, commit

### Task 4: `internal/store` — schema, open, migrations

**Files:** Create `internal/store/store.go`, `internal/store/schema.go`, tests

**Interfaces:** Produces `Open(path string) (*Store, error)`, `(*Store) Close() error`, `(*Store) UserID() string`, `(*Store) Epoch() proto.Dec`.

- [ ] Tests: opening creates schema at `user_version = 1`; reopening is idempotent; a newer `user_version` is refused with a clear error
- [ ] Tests: `PRAGMA journal_mode` reports `wal` after open (guards the silent-DSN-key failure mode)
- [ ] Tests: database and `-wal`/`-shm` files are mode 0600, directory 0700
- [ ] Implement: two `sql.DB` handles (write `SetMaxOpenConns(1)` + `_txlock=immediate`; read multi-conn), pragmas, epoch generated from `crypto/rand` as 63-bit decimal
- [ ] Commit

### Task 5: `internal/store` — sequencer and dedupe

**Files:** Create `internal/store/ops.go`, `internal/store/ops_test.go`, `internal/store/property_test.go`

**Interfaces:** Produces `(*Store) Push(userID string, ops []proto.Op, deviceID string) (PushResult, error)` where `PushResult{Acks []Ack; HeadSeq proto.Dec}` and `Ack{ID, Kind, EntityRev, Digest string; OriginLocalID *string; Seq proto.Dec}`; `(*Store) Changes(userID string, since proto.Dec, limit int) (ChangesResult, error)`; `(*Store) HeadSeq(userID string) proto.Dec`.

- [ ] Tests: first op gets seq 1; sequences are contiguous across pushes
- [ ] Tests: duplicate `(entity_id, entity_rev)` consumes no sequence and re-acks the original seq
- [ ] Tests: the same op twice in one push yields two acks with the same seq (multiplicity rule)
- [ ] Tests: acks echo the **client's** digest, one ack per received op, all six fields populated
- [ ] Tests: a body differing at an existing `(entity_id, entity_rev)` is first-write-wins, acked with the stored seq
- [ ] Property test (rapid): concurrent pushes with rollback injected via a `func(*sql.Tx) error` seam — log is always contiguous `1..head`, and `meta.head_seq == MAX(ops.seq)`
- [ ] Tests: `Changes` never filters by device; returns ops strictly after `since`; `more` reflects remaining rows; byte cap truncates only at an op boundary
- [ ] Implement with `BEGIN IMMEDIATE`, dedupe select inside the transaction, `context.WithoutCancel`
- [ ] Commit

### Task 6: `internal/hub` — HTTP routes and byte-verbatim emission

**Files:** Create `internal/hub/server.go`, `internal/hub/handlers.go`, `internal/hub/emit.go`, `internal/hub/middleware.go`, tests

**Interfaces:** Produces `New(st *store.Store, opts Options) *Server`, `(*Server) Handler() http.Handler`.

- [ ] Tests (`httptest`): `/v1/sync/status` returns `protocol_version` as a number, `epoch`/`head_seq`/`projected_seq` as decimal strings, with `projected_seq == head_seq`
- [ ] Tests: `/v1/sync/ops` ack shape; `head_seq >= projected_seq` never violated; unknown `protocol_version` rejected
- [ ] Tests: `/v1/sync/changes` emits `server_ts` as a decimal string and `more` as a boolean; response body is **byte-identical** for the op body (`bytes.Equal`, not JSON equality)
- [ ] Tests: `X-Sync-Mode: poll` present on all three routes while WS is unimplemented
- [ ] Tests: missing/wrong bearer → 401; `X-User-Id` mismatch → 403 and no partition created; `limit` clamped to `[1,500]`; malformed `since` → 400; body over cap → 400 via `MaxBytesReader`; `Origin` header → 400; unknown path → 404 with no redirect
- [ ] Tests: `/healthz` returns bare `200 ok` with no version or counts
- [ ] Implement emission by writing bytes to an `io.Writer`; server timeouts set explicitly; panic-recovery middleware
- [ ] Commit

### Task 7: `internal/pair` — PSK and pairing codes

**Files:** Create `internal/pair/pair.go`, `internal/pair/codes.go`, tests

**Interfaces:** Produces `LoadOrCreate(dir string) (*Keys, error)` with `Keys{HubID, PSK string}`, `(*Keys) Fingerprint() string`, `NewWindow(ttl time.Duration) *Window`, `(*Window) Code() string`, `(*Window) Redeem(code string) (string, error)`.

- [ ] Tests: PSK is 32 bytes, file mode 0600, stable across reloads
- [ ] Tests: code is single-use; expires; five failures destroy the window; comparison is constant-time
- [ ] Tests: fingerprint is derived from the PSK and is stable
- [ ] Implement; `POST /pair` handler in `hub` with a 1/s global limiter, JSON content-type required, `Origin` rejected
- [ ] Commit

### Task 8: `internal/discover` — mDNS

**Files:** Create `internal/discover/discover.go`, tests

**Interfaces:** Produces `Advertise(ctx, cfg Config) error`, `Browse(ctx, timeout) ([]Found, error)` with `Found{Name, Host string; Port int}`.

- [ ] Tests: TXT records carry `v=1` and the instance name but **not** the hub id; advertising is skipped when bound to loopback; `--no-mdns` disables it
- [ ] Implement with `brutella/dnssd`; random instance label by default
- [ ] Commit

### Task 9: `internal/settings` — claude-mem settings file

**Files:** Create `internal/settings/settings.go`, tests

**Interfaces:** Produces `Path() string`, `Read(path) (map[string]json.RawMessage, error)`, `Update(path string, kv map[string]string) error`, `Restore(path string) error`.

- [ ] Tests: unknown keys survive a write; unparseable file refuses to be overwritten; write is temp-file-plus-rename in the same directory; result is mode 0600; a backup is left behind
- [ ] Tests: `CLAUDE_MEM_CLOUD_SYNC_DEVICE_ID` is preserved when present and never generated by us
- [ ] Tests: target is `~/.claude-mem/settings.json`, honoring `CLAUDE_MEM_DATA_DIR`
- [ ] Implement, commit

### Task 10: `internal/clientdb` — status and backfill

**Files:** Create `internal/clientdb/clientdb.go`, `internal/clientdb/backfill.go`, tests with a fixture database

**Interfaces:** Produces `Open(path string) (*DB, error)`, `(*DB) Pending() (Counts, error)`, `(*DB) WorkerRunning() bool`, `(*DB) Backfill(opts BackfillOpts) (BackfillResult, error)`, `(*DB) UndoBackfill() error`.

- [ ] Fixture: build a miniature claude-mem database (three tables, `schema_versions`, `sync_launch_exclusions`) in test setup
- [ ] Tests: `--dry-run` reports per-table counts and bytes without writing
- [ ] Tests: backfill nulls `synced_at` only where `origin_device_id IS NULL`; saves exclusions into a cmemlan-owned table; `UndoBackfill` restores both
- [ ] Tests: refuses to run when the worker pid file is live unless forced; refuses on unrecognized `schema_versions`; honors `CLAUDE_MEM_EXCLUDED_PROJECTS`
- [ ] Tests: `VACUUM INTO` backup is created before any mutation
- [ ] Implement in one `BEGIN IMMEDIATE` transaction, commit

### Task 11: `internal/cli` — command surface

**Files:** Create `internal/cli/cli.go` plus one file per command, tests

**Interfaces:** Produces `Run(args []string, stdout, stderr io.Writer) int`.

- [ ] Tests: arguments are reordered so `connect http://x --code Y` parses (stdlib `flag` stops at the first positional)
- [ ] Tests: `version` prints the stamped version; unknown command exits non-zero with usage
- [ ] Tests: `doctor` reports missing env, a shadowing variable in `~/.claude/settings.json`, an unreachable hub, and pending counts — using injected interfaces, not the real filesystem
- [ ] Implement `serve`, `pair`, `devices`, `revoke`, `rotate-token`, `connect`, `backfill`, `status`, `doctor`, `version`, `fixtures scrub`
- [ ] Commit

### Task 12: `serve` — bind policy and service install

**Files:** Create `internal/cli/serve.go`, `internal/cli/service.go`, `internal/hub/bind.go`, tests

**Interfaces:** Produces `ClassifyBind(addr string) (netip.Addr, error)`, `AllowCIDR(defaults []netip.Prefix) func(net.Addr) bool`.

- [ ] Tests: `0.0.0.0`, `::`, `[::]`, and a bare `:8787` are all refused without the override flag; loopback and RFC1918/CGNAT accepted; classification uses `net/netip`, never string matching
- [ ] Tests: connections outside `--allow-cidr` are closed at accept time
- [ ] Tests: the generated systemd unit contains the resolved absolute data directory and the hardening directives; `--print-unit` prints without installing
- [ ] Implement, including `loginctl enable-linger` and launchd plist, commit

### Task 13: Fixture capture and scrub

**Files:** Create `internal/hub/record.go`, `internal/cli/fixtures.go`, tests

- [ ] Tests: `--record` refuses to write inside a git work tree without the override; files are 0600 in a 0700 directory
- [ ] Tests: scrub replaces bodies with synthetic content of the same shape and length, recomputes both digests, strips `Authorization`/`X-User-Id`/`X-Device-Id`, and the result still validates
- [ ] Implement, commit

### Task 14: End-to-end probe against the real client

**Files:** Create `test/e2e/e2e_test.go` (build tag `e2e`), `docs/testing.md`

- [ ] Verify the open question: run a claude-mem worker with `CLAUDE_MEM_DATA_DIR` and `CLAUDE_MEM_WORKER_PORT` pointed at scratch values, sync env vars pointed at our hub, a seeded row with `synced_at IS NULL`, and no Claude credentials
- [ ] If it pushes: keep the test and wire it into CI with node + claude-mem installed
- [ ] If it does not: record the exact failure in `docs/testing.md` and fall back to the `clientvalidate` node-harness tier
- [ ] Commit either outcome

### Task 15: Docs

**Files:** Create `README.md`, `CLAUDE.md`, `docs/protocol.md`, `docs/architecture.md`, `docs/security.md`, `docs/backfill.md`, `docs/troubleshooting.md`

- [ ] `README.md`: quickstart, how it works, security posture, the accurate privacy wording from the spec, independence disclaimer
- [ ] `docs/protocol.md`: the full wire contract with the version each detail was verified against
- [ ] `CLAUDE.md`: the invariants list verbatim from the spec
- [ ] Commit

### Task 16: Plugin marketplace

**Files:** Create `.claude-plugin/marketplace.json`, `plugin/.claude-plugin/plugin.json`, `plugin/skills/claude-mem-lan-sync/SKILL.md`

- [ ] Manifests match the shapes read by the current plugin loader
- [ ] Skill frontmatter triggers on claude-mem, Claude Code memory, cross-device sync, `CLAUDE_MEM_CLOUD_SYNC_*`, cmem.ai
- [ ] Skill body is the setup procedure plus the failure playbook and the three hard rules
- [ ] Commit

### Task 17: CI, release, packaging

**Files:** Create `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `.goreleaser.yaml`, `.golangci.yml`, `.github/dependabot.yml`, `Dockerfile`, `install.sh`

- [ ] `ci.yml`: vet, golangci-lint v2, `go test -race` (CGO_ENABLED=1, separate job), `govulncheck`, `gosec`, `goreleaser check`, manifest/frontmatter validation, fixture secret scan
- [ ] `.goreleaser.yaml`: `version: 2`, `env: [CGO_ENABLED=0]`, `-trimpath`, version stamping, `dockers_v2`, checksums, cosign, provenance attestation
- [ ] `dependabot.yml`: ignore `modernc.org/libc`; actions pinned by SHA
- [ ] `install.sh`: verifies the published checksum, restarts an installed service, strips `com.apple.quarantine`
- [ ] Commit

---

## Self-Review

**Spec coverage:** protocol (2,3,6), storage and sequencer (4,5), auth and pairing (7), discovery (8), settings (9), backfill (10), CLI (11), bind and service (12), fixtures (13), testing (5,6,13,14), docs (15), marketplace (16), CI and release (17). Blocked/deferred items intentionally have no task.

**Placeholders:** none — every task names exact files, interfaces, and test assertions.

**Type consistency:** `proto.Dec` is used for every sequence, revision, and epoch across tasks 2–6. `proto.Op` carries `RawLiteral` and is consumed unchanged by `store.Push` and `hub` emission. `store.Ack` field names match the ack contract in the spec.

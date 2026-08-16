package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// fakeHub stands in for a running hub so doctor and status can be exercised
// without a listener.
type fakeHub struct {
	result StatusResult
	err    error
	calls  int
}

func (f *fakeHub) Status(_ context.Context, _, _, _ string) (StatusResult, error) {
	f.calls++
	return f.result, f.err
}

// newClaudeMemDir builds a claude-mem data directory: the real schema from a
// captured .schema dump, plus a settings file.
func newClaudeMemDir(t *testing.T, settings map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "clientdb-schema.sql"))
	if err != nil {
		t.Fatalf("reading the captured schema: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "claude-mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatalf("applying the captured schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO schema_versions (version, applied_at) VALUES (47, '2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if settings != nil {
		b, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("CLAUDE_MEM_DATA_DIR", dir)
	return dir
}

func runCmd(t *testing.T, env Env, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	env.Stdout, env.Stderr = &out, &errOut
	code := Run(args, env)
	return code, out.String(), errOut.String()
}

func TestDoctorReportsMissingClaudeMem(t *testing.T) {
	t.Setenv("CLAUDE_MEM_DATA_DIR", t.TempDir()) // empty: no database

	code, stdout, _ := runCmd(t, Env{}, "doctor")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "claude-mem installed") {
		t.Fatalf("doctor did not report the missing install:\n%s", stdout)
	}
}

func TestDoctorReportsUnconfiguredSync(t *testing.T) {
	newClaudeMemDir(t, map[string]string{"CLAUDE_MEM_MODEL": "haiku"})

	code, stdout, _ := runCmd(t, Env{}, "doctor")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 when sync is not configured", code)
	}
	if !strings.Contains(stdout, "sync configured") || !strings.Contains(stdout, "cmemlan connect") {
		t.Fatalf("doctor did not explain how to configure sync:\n%s", stdout)
	}
}

func TestDoctorHealthyReportsHubState(t *testing.T) {
	newClaudeMemDir(t, map[string]string{
		keyHubURL:                         "http://hub.local:8787",
		keyToken:                          "a-key",
		keyUserID:                         "hub-id",
		"CLAUDE_MEM_CLOUD_SYNC_DEVICE_ID": "94a3962b-daef-44c7-9475-a0eb978f4a19",
	})
	hub := &fakeHub{result: StatusResult{Epoch: "7", HeadSeq: "42", ProjectedSeq: "42", SyncMode: "poll"}}

	code, stdout, _ := runCmd(t, Env{Hub: hub}, "doctor")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, stdout)
	}
	if hub.calls != 1 {
		t.Fatalf("doctor made %d hub calls, want 1", hub.calls)
	}
	for _, want := range []string{"hub reachable", "epoch 7", "head 42", "websocket disabled"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor output missing %q:\n%s", want, stdout)
		}
	}
}

// A bad key and an unreachable hub are indistinguishable to the client, which
// retries silently forever. Telling them apart is the point of doctor.
func TestDoctorDistinguishesBadKeyFromOutage(t *testing.T) {
	newClaudeMemDir(t, map[string]string{
		keyHubURL: "http://hub.local:8787", keyToken: "wrong", keyUserID: "hub-id",
	})
	hub := &fakeHub{err: errors.New("hub rejected our credentials (HTTP 401)")}

	code, stdout, _ := runCmd(t, Env{Hub: hub}, "doctor")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "credentials") {
		t.Fatalf("doctor did not surface the credential failure:\n%s", stdout)
	}
}

// claude-mem applies environment overrides on top of its own settings file, so a
// stale value in Claude Code's settings silently wins.
func TestDoctorDetectsShadowingConfiguration(t *testing.T) {
	newClaudeMemDir(t, map[string]string{
		keyHubURL: "http://hub.local:8787", keyToken: "a-key", keyUserID: "hub-id",
	})

	codeDir := t.TempDir()
	shadow := map[string]any{"env": map[string]string{keyHubURL: "http://stale-hub:8787"}}
	b, err := json.Marshal(shadow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codeDir, "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", codeDir)

	exit, stdout, _ := runCmd(t, Env{Hub: &fakeHub{}}, "doctor")
	if exit != 1 {
		t.Fatalf("exit = %d, want 1 when a stale value shadows the good one", exit)
	}
	if !strings.Contains(stdout, "conflicting configuration") {
		t.Fatalf("doctor missed the shadowing value:\n%s", stdout)
	}
}

func TestStatusUnconfigured(t *testing.T) {
	newClaudeMemDir(t, map[string]string{})

	code, stdout, _ := runCmd(t, Env{}, "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "not configured") {
		t.Fatalf("status output:\n%s", stdout)
	}
}

func TestStatusShowsQueueAndHub(t *testing.T) {
	dir := newClaudeMemDir(t, map[string]string{
		keyHubURL: "http://hub.local:8787", keyToken: "a-key", keyUserID: "hub-id",
	})
	seedUnsyncedRow(t, dir)

	hub := &fakeHub{result: StatusResult{Epoch: "3", HeadSeq: "9"}}
	code, stdout, _ := runCmd(t, Env{Hub: hub}, "status")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, stdout)
	}
	for _, want := range []string{"waiting to upload", "observations  1", "hub epoch  3", "hub head   9"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status missing %q:\n%s", want, stdout)
		}
	}
}

func TestStatusReportsUnreachableHubWithoutLosingQueue(t *testing.T) {
	dir := newClaudeMemDir(t, map[string]string{
		keyHubURL: "http://hub.local:8787", keyToken: "a-key", keyUserID: "hub-id",
	})
	seedUnsyncedRow(t, dir)

	hub := &fakeHub{err: errors.New("connection refused")}
	code, stdout, _ := runCmd(t, Env{Hub: hub}, "status")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 when the hub is unreachable", code)
	}
	if !strings.Contains(stdout, "safe locally") {
		t.Fatalf("status should reassure that queued memories are not lost:\n%s", stdout)
	}
}

func seedUnsyncedRow(t *testing.T, dir string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "claude-mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`
		INSERT INTO sdk_sessions (content_session_id, memory_session_id, project, started_at, started_at_epoch, status)
		VALUES ('c', 'm', 'proj', '2026-01-01T00:00:00.000Z', 1, 'completed')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO observations (memory_session_id, project, text, type, title, created_at,
			created_at_epoch, narrative, synced_at, origin_device_id, origin_local_id, sync_rev)
		VALUES ('m', 'proj', 'text', 'discovery', 'Title', '2026-01-01T00:00:00.000Z', 1,
			'narrative', NULL, NULL, NULL, '1')`); err != nil {
		t.Fatal(err)
	}
}

// A pairing code is single-use, so refusing for a missing fingerprint must
// happen before the code is redeemed.
func TestConnectRequiresFingerprintBeforeTouchingTheNetwork(t *testing.T) {
	newClaudeMemDir(t, map[string]string{})

	code, _, stderr := runCmd(t, Env{}, "connect", "http://hub.local:8787", "--code", "123-456-789")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--fingerprint is required") {
		t.Fatalf("stderr:\n%s", stderr)
	}
}

// Discovery is unauthenticated, so an attacker can advertise and relay. The
// address must be typed when a code is being redeemed.
func TestConnectRefusesToPairAgainstDiscoveredAddress(t *testing.T) {
	newClaudeMemDir(t, map[string]string{})

	code, _, stderr := runCmd(t, Env{}, "connect", "--code", "123-456-789", "--fingerprint", "AAAA-BBBB-CCCC")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "explicitly") {
		t.Fatalf("stderr:\n%s", stderr)
	}
}

func TestConnectUndoRestoresSettings(t *testing.T) {
	dir := newClaudeMemDir(t, map[string]string{"CLAUDE_MEM_MODEL": "haiku"})
	settingsPath := filepath.Join(dir, "settings.json")

	// A connect writes a backup; simulate one having happened.
	original, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	backup := settingsPath + ".bak-1"
	if err := os.WriteFile(backup, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"CLAUDE_MEM_MODEL":"changed"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(t, Env{}, "connect", "--undo")
	if code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", code, stdout, stderr)
	}
	restored, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatal("--undo did not restore the original settings")
	}
}

func TestBackfillDryRunReportsWithoutWriting(t *testing.T) {
	dir := newClaudeMemDir(t, map[string]string{})
	seedSyncedRow(t, dir)

	code, stdout, stderr := runCmd(t, Env{}, "backfill", "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", code, stdout, stderr)
	}
	for _, want := range []string{"dry run", "observations", "total"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("backfill output missing %q:\n%s", want, stdout)
		}
	}
	// Nothing was queued.
	if n := unsyncedCount(t, dir); n != 0 {
		t.Fatalf("dry run queued %d rows", n)
	}
}

func TestBackfillQueuesAndUndoes(t *testing.T) {
	dir := newClaudeMemDir(t, map[string]string{})
	seedSyncedRow(t, dir)

	if code, stdout, stderr := runCmd(t, Env{Now: func() time.Time { return time.UnixMilli(1) }},
		"backfill"); code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", code, stdout, stderr)
	}
	if n := unsyncedCount(t, dir); n != 1 {
		t.Fatalf("backfill queued %d rows, want 1", n)
	}

	if code, stdout, stderr := runCmd(t, Env{}, "backfill", "--undo"); code != 0 {
		t.Fatalf("undo exit = %d\n%s\n%s", code, stdout, stderr)
	}
	if n := unsyncedCount(t, dir); n != 0 {
		t.Fatalf("after undo %d rows are still queued", n)
	}
}

func seedSyncedRow(t *testing.T, dir string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "claude-mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(`
		INSERT INTO sdk_sessions (content_session_id, memory_session_id, project, started_at, started_at_epoch, status)
		VALUES ('c', 'm', 'proj', '2026-01-01T00:00:00.000Z', 1, 'completed')`); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`
		INSERT INTO observations (memory_session_id, project, text, type, title, created_at,
			created_at_epoch, narrative, synced_at, origin_device_id, origin_local_id, sync_rev)
		VALUES ('m', 'proj', 'text', 'discovery', 'Title', '2026-01-01T00:00:00.000Z', 1,
			'narrative', 1700000000000, NULL, NULL, '1')`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	// Mirror claude-mem's launch baseline for an already-stamped local row.
	if _, err := db.Exec(`
		INSERT INTO sync_launch_exclusions (kind, origin_local_id, through_rev)
		VALUES ('observation', CAST(? AS TEXT), '1')`, id); err != nil {
		t.Fatal(err)
	}
}

func unsyncedCount(t *testing.T, dir string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "claude-mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM observations WHERE synced_at IS NULL AND origin_device_id IS NULL`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestExcludedProjectsParsing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting string
		want    int
	}{
		{"empty", "", 0},
		{"comma separated", "a, b ,c", 3},
		{"json array", `["a","b"]`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newClaudeMemDir(t, map[string]string{"CLAUDE_MEM_EXCLUDED_PROJECTS": tc.setting})
			if got := len(excludedProjects()); got != tc.want {
				t.Fatalf("parsed %d projects, want %d", got, tc.want)
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.0 KB"},
		{5 << 20, "5.0 MB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitiseStripsControlCharacters(t *testing.T) {
	// Device names are attacker-supplied and printed to a terminal.
	if got := sanitise("laptop\x1b[31mred\x07"); strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("sanitise left control characters: %q", got)
	}
	if got := sanitise(""); got != "(unnamed)" {
		t.Fatalf("sanitise(\"\") = %q", got)
	}
}

func TestLockfileRejectsSecondHolder(t *testing.T) {
	dir := t.TempDir()

	release, err := Lockfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()

	if _, err := Lockfile(dir); err == nil {
		t.Fatal("a second serve acquired the same data directory")
	}
}

func TestLockfileClearsStaleLock(t *testing.T) {
	dir := t.TempDir()
	// A pid that cannot be running.
	if err := os.WriteFile(filepath.Join(dir, LockFile), []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	release, err := Lockfile(dir)
	if err != nil {
		t.Fatalf("a stale lock was not cleared: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestServiceUnitCarriesResolvedPathsAndHardening(t *testing.T) {
	unit := serviceUnit("/usr/local/bin/cmemlan", "/home/u/.local/share/cmemlan", "192.168.1.10:8787", "192.168.1.0/24")

	for _, want := range []string{
		"/home/u/.local/share/cmemlan", // resolved absolute path, not inherited
		"--allow-cidr 192.168.1.0/24",
		"ProtectSystem=strict",
		"NoNewPrivileges=yes",
		"CapabilityBoundingSet=",
		"ReadWritePaths=/home/u/.local/share/cmemlan",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q", want)
		}
	}
}

// The macOS agent silently dropped the operator's narrowing before this.
func TestLaunchdPlistCarriesAllowCIDR(t *testing.T) {
	plist := launchdPlist("/usr/local/bin/cmemlan", "/data", "192.168.1.10:8787", "192.168.1.0/24")

	if !strings.Contains(plist, "--allow-cidr") || !strings.Contains(plist, "192.168.1.0/24") {
		t.Fatalf("plist dropped the peer allowlist:\n%s", plist)
	}
	if !strings.Contains(plist, "<key>RunAtLoad</key><true/>") {
		t.Error("plist should run at load")
	}

	// And it must be omitted cleanly when not set.
	plain := launchdPlist("/usr/local/bin/cmemlan", "/data", "127.0.0.1:8787", "")
	if strings.Contains(plain, "--allow-cidr") {
		t.Errorf("plist emitted an empty allow-cidr flag:\n%s", plain)
	}
}

func TestVersionCommandIsStamped(t *testing.T) {
	code, stdout, _ := runCmd(t, Env{}, "version")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "claude-mem") {
		t.Fatalf("version output should name the verified claude-mem release: %q", stdout)
	}
}

func TestFixturesRequiresInputDirectory(t *testing.T) {
	code, _, stderr := runCmd(t, Env{}, "fixtures", "scrub")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--in is required") {
		t.Fatalf("stderr: %s", stderr)
	}
}

func TestUnknownFixturesSubcommand(t *testing.T) {
	code, _, stderr := runCmd(t, Env{}, "fixtures", "wat")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Fatalf("stderr: %s", stderr)
	}
}

func TestServeRefusesPublicBind(t *testing.T) {
	dataDir := t.TempDir()

	code, _, stderr := runCmd(t, Env{DataDir: dataDir}, "serve", "--bind", "0.0.0.0:8787")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "insecure-public-bind") {
		t.Fatalf("serve did not explain the override:\n%s", stderr)
	}
}

func TestServeRequiresAllowCIDRWhenDemanded(t *testing.T) {
	t.Setenv("CMEMLAN_REQUIRE_ALLOW_CIDR", "1")
	t.Setenv("CMEMLAN_ALLOW_CIDR", "")

	code, _, stderr := runCmd(t, Env{DataDir: t.TempDir()},
		"serve", "--bind", "127.0.0.1:8787")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "CMEMLAN_ALLOW_CIDR is required") {
		t.Fatalf("stderr:\n%s", stderr)
	}
}

func TestPrintUnitDoesNotInstall(t *testing.T) {
	if _, err := os.Stat("/etc/os-release"); err != nil {
		t.Skip("linux only")
	}
	dataDir := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)

	code, stdout, stderr := runCmd(t, Env{DataDir: dataDir}, "serve", "--print-unit")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "[Service]") {
		t.Fatalf("no unit printed:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(configDir, "systemd", "user", "cmemlan.service")); err == nil {
		t.Fatal("--print-unit installed the service")
	}
}

func TestDevicesOnEmptyHub(t *testing.T) {
	dataDir := t.TempDir()

	code, stdout, stderr := runCmd(t, Env{}, "devices", "--data-dir", dataDir)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "no devices") {
		t.Fatalf("devices output:\n%s", stdout)
	}
}

func TestPairPrintsCodeAndFingerprint(t *testing.T) {
	dataDir := t.TempDir()

	code, stdout, stderr := runCmd(t, Env{}, "pair", "--data-dir", dataDir)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "code") || !strings.Contains(stdout, "fingerprint") {
		t.Fatalf("pair output:\n%s", stdout)
	}
	// The window must exist for the running hub to redeem against.
	if _, err := os.Stat(filepath.Join(dataDir, "pairing.json")); err != nil {
		t.Fatalf("no pairing window was written: %v", err)
	}
}

func TestRotateTokenChangesKeyWithoutTouchingEpoch(t *testing.T) {
	dataDir := t.TempDir()

	// Establish a hub so there is an epoch to compare.
	if code, _, stderr := runCmd(t, Env{}, "devices", "--data-dir", dataDir); code != 0 {
		t.Fatalf("setup failed: %s", stderr)
	}
	epochBefore := readEpoch(t, dataDir)

	if code, stdout, stderr := runCmd(t, Env{}, "rotate-token", "--data-dir", dataDir); code != 0 {
		t.Fatalf("exit = %d\n%s\n%s", code, stdout, stderr)
	}

	if after := readEpoch(t, dataDir); after != epochBefore {
		t.Fatalf("rotating the key changed the epoch (%s -> %s), forcing every device to replay",
			epochBefore, after)
	}
}

func readEpoch(t *testing.T, dataDir string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var epoch string
	if err := db.QueryRow(`SELECT epoch FROM meta LIMIT 1`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

func TestParseFlagsSurfacesErrors(t *testing.T) {
	code, _, _ := runCmd(t, Env{}, "backfill", "--nonexistent-flag")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 for an unknown flag", code)
	}
}

func TestCmpPrefersFirstNonEmpty(t *testing.T) {
	if got := cmp("a", "b"); got != "a" {
		t.Errorf("cmp = %q", got)
	}
	if got := cmp("", "b"); got != "b" {
		t.Errorf("cmp = %q", got)
	}
}

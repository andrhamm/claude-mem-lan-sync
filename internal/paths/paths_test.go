package paths

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDataDirPrecedence(t *testing.T) {
	t.Setenv(EnvDataDir, "/from/env")
	got, err := DataDir("/from/flag")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/from/env" {
		t.Fatalf("DataDir = %q, want the environment to win", got)
	}
}

func TestDataDirFlagBeatsDefault(t *testing.T) {
	t.Setenv(EnvDataDir, "")
	got, err := DataDir("/from/flag")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/from/flag" {
		t.Fatalf("DataDir = %q, want the flag value", got)
	}
}

func TestDataDirDefault(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("default layout is platform specific")
	}
	home := t.TempDir()
	t.Setenv(EnvDataDir, "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", home)

	got, err := DataDir("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "share", "cmemlan")
	if got != want {
		t.Fatalf("DataDir = %q, want %q", got, want)
	}
}

func TestDataDirHonoursXDG(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG is linux only here")
	}
	t.Setenv(EnvDataDir, "")
	t.Setenv("XDG_DATA_HOME", "/xdg")

	got, err := DataDir("")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/xdg", "cmemlan") {
		t.Fatalf("DataDir = %q, want it under XDG_DATA_HOME", got)
	}
}

// claude-mem reads CLAUDE_MEM_DATA_DIR itself. Honouring it is what keeps us
// pointed at the same database the worker is using — and what makes the
// end-to-end tests able to run against a scratch install.
func TestClaudeMemDirHonoursEnv(t *testing.T) {
	t.Setenv(EnvClaudeMemDir, "/scratch/claude-mem")

	dir, err := ClaudeMemDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/scratch/claude-mem" {
		t.Fatalf("ClaudeMemDir = %q", dir)
	}

	settings, err := ClaudeMemSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings != "/scratch/claude-mem/settings.json" {
		t.Fatalf("ClaudeMemSettings = %q", settings)
	}

	db, err := ClaudeMemDB()
	if err != nil {
		t.Fatal(err)
	}
	if db != "/scratch/claude-mem/claude-mem.db" {
		t.Fatalf("ClaudeMemDB = %q", db)
	}

	pid, err := ClaudeMemWorkerPID()
	if err != nil {
		t.Fatal(err)
	}
	if pid != "/scratch/claude-mem/worker.pid" {
		t.Fatalf("ClaudeMemWorkerPID = %q", pid)
	}
}

func TestClaudeMemDirDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvClaudeMemDir, "")
	t.Setenv("HOME", home)

	dir, err := ClaudeMemDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(home, ".claude-mem") {
		t.Fatalf("ClaudeMemDir = %q", dir)
	}
}

// We write sync config to claude-mem's settings file, never Claude Code's.
func TestClaudeCodeSettingsIsSeparate(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvClaudeMemDir, "")
	t.Setenv(EnvClaudeConfigDir, "")
	t.Setenv("HOME", home)

	code, err := ClaudeCodeSettings()
	if err != nil {
		t.Fatal(err)
	}
	mem, err := ClaudeMemSettings()
	if err != nil {
		t.Fatal(err)
	}
	if code == mem {
		t.Fatal("Claude Code settings and claude-mem settings must be different files")
	}
	if code != filepath.Join(home, ".claude", "settings.json") {
		t.Fatalf("ClaudeCodeSettings = %q", code)
	}
}

func TestClaudeCodeSettingsHonoursConfigDir(t *testing.T) {
	t.Setenv(EnvClaudeConfigDir, "/custom/claude")

	got, err := ClaudeCodeSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/claude/settings.json" {
		t.Fatalf("ClaudeCodeSettings = %q", got)
	}
}

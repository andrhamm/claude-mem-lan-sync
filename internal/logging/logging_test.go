package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestRedactHidesTheValue(t *testing.T) {
	secret := "s3cret-pre-shared-key-material"
	got := Redact(secret)
	if strings.Contains(got, secret) {
		t.Fatalf("Redact leaked the value: %q", got)
	}
	if !strings.Contains(got, "len=30") {
		t.Fatalf("Redact = %q, want the length preserved", got)
	}
	if Redact("") != "[empty]" {
		t.Fatalf("Redact(\"\") = %q", Redact(""))
	}
}

// A redacted secret must not reach the log even when the caller logs it directly.
func TestLoggerNeverEmitsRedactedSecrets(t *testing.T) {
	var buf bytes.Buffer
	log := New("debug", "text", &buf)

	psk := "aaaabbbbccccddddeeeeffff00001111"
	log.Debug("hub configured", "token", Redact(psk))

	if strings.Contains(buf.String(), psk) {
		t.Fatalf("log leaked the psk: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "len=32") {
		t.Fatalf("log = %q, want the redacted form", buf.String())
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := New("warn", "text", &buf)

	log.Info("this should not appear")
	if buf.Len() != 0 {
		t.Fatalf("info logged at warn level: %q", buf.String())
	}

	log.Warn("this should appear")
	if !strings.Contains(buf.String(), "this should appear") {
		t.Fatalf("warn not logged: %q", buf.String())
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	log := New("info", "json", &buf)
	log.Info("hello", "k", "v")

	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("json handler produced %q", out)
	}
}

func TestUnknownLevelAndFormatFallBack(t *testing.T) {
	var buf bytes.Buffer
	log := New("nonsense", "nonsense", &buf)
	log.Info("still works")
	if !strings.Contains(buf.String(), "still works") {
		t.Fatalf("fallback logger dropped the record: %q", buf.String())
	}
	if parseLevel("nonsense") != slog.LevelInfo {
		t.Fatal("unknown level should fall back to info")
	}
}

//go:build clientvalidate

// Package clientvalidate checks the hub's responses against claude-mem's own
// validation rules rather than our Go reimplementation of them.
//
// The golden replay suite asserts that our code agrees with itself. This asserts
// that it agrees with the client, which is the part that matters — a shared
// misunderstanding between our implementation and our tests is invisible to
// every other tier.
//
// Run: make clientvalidate   (needs node)
package clientvalidate

import (
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrhamm/claude-mem-lan-sync/internal/hub"
	"github.com/andrhamm/claude-mem-lan-sync/internal/store"
)

func TestResponsesSatisfyClientValidators(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for the client-validation tier")
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	const token = "clientvalidate-token"
	srv := hub.New(st, hub.Options{
		UserID: st.UserID(),
		Auth:   hub.StaticToken{UserID: st.UserID(), Token: token},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	out, err := exec.Command("node", "validate.js", ts.URL, st.UserID(), token).CombinedOutput()
	t.Log("\n" + string(out))
	if err != nil {
		t.Fatalf("hub responses failed claude-mem's own validation rules: %v", err)
	}
	if !strings.Contains(string(out), "All responses satisfy") {
		t.Fatal("validator did not report success")
	}
}

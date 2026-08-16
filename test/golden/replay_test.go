// Package golden replays captured traffic through the hub.
//
// This is a regression net, not evidence. The fixtures came from a real client,
// but the assertions are ours: it catches us breaking our own understanding of
// the protocol. The evidence tier is test/e2e, which drives the real worker.
package golden

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/andrhamm/claude-mem-lan-sync/internal/hub"
	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
	"github.com/andrhamm/claude-mem-lan-sync/internal/store"
)

const token = "golden-token"

type exchange struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Query    string            `json:"query"`
	Headers  map[string]string `json:"headers"`
	Request  json.RawMessage   `json:"request"`
	Status   int               `json:"status"`
	Response json.RawMessage   `json:"response"`
}

func loadFixtures(t *testing.T) []exchange {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "fixtures", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Skip("no fixtures captured yet")
	}

	var out []exchange
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var e exchange
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
		out = append(out, e)
	}
	return out
}

func newServer(t *testing.T) (*hub.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := hub.New(st, hub.Options{
		UserID: st.UserID(),
		Auth:   hub.StaticToken{UserID: st.UserID(), Token: token},
	})
	return srv, st
}

// Every op a real client sent must still be accepted, and its body must come
// back byte for byte.
func TestReplayCapturedPushes(t *testing.T) {
	fixtures := loadFixtures(t)
	srv, st := newServer(t)

	var pushed int
	for _, e := range fixtures {
		if e.Path != "/v1/sync/ops" || len(e.Request) == 0 {
			continue
		}

		req := httptest.NewRequest(http.MethodPost, e.Path, bytes.NewReader(e.Request))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-User-Id", st.UserID())
		req.Header.Set("X-Device-Id", "golden-device")

		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("captured push rejected with %d: %s", w.Code, w.Body)
		}

		var sent struct {
			Ops []map[string]json.RawMessage `json:"ops"`
		}
		if err := json.Unmarshal(e.Request, &sent); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Acked []map[string]json.RawMessage `json:"acked"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Acked) != len(sent.Ops) {
			t.Fatalf("acked %d of %d ops", len(got.Acked), len(sent.Ops))
		}
		pushed += len(sent.Ops)

		// The ack must echo the digest the client sent.
		for i, ack := range got.Acked {
			if !bytes.Equal(ack["operation_sha256"], sent.Ops[i]["operation_sha256"]) {
				t.Errorf("ack %d echoed %s, client sent %s",
					i, ack["operation_sha256"], sent.Ops[i]["operation_sha256"])
			}
			for _, k := range []string{"id", "kind", "entity_rev", "seq", "origin_local_id"} {
				if _, ok := ack[k]; !ok {
					t.Errorf("ack %d missing mandatory field %q", i, k)
				}
			}
		}
	}

	if pushed == 0 {
		t.Skip("no push exchanges among the fixtures")
	}

	// Pull them back and compare on raw bytes. A JSON-equality helper would pass
	// on exactly the HTML re-escaping the byte-verbatim rule exists to prevent.
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/changes?since=0&limit=500", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-Id", st.UserID())
	req.Header.Set("X-Device-Id", "golden-device")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("changes returned %d", w.Code)
	}

	var page struct {
		Ops []struct {
			Seq      json.RawMessage `json:"seq"`
			ServerTS json.RawMessage `json:"server_ts"`
			Body     string          `json:"body"`
			Digest   string          `json:"operation_sha256"`
		} `json:"ops"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Ops) != pushed {
		t.Fatalf("pulled %d ops, pushed %d", len(page.Ops), pushed)
	}

	for i, op := range page.Ops {
		if proto.Digest([]byte(op.Body)) != op.Digest {
			t.Errorf("op %d: returned body does not hash to its own digest", i)
		}
		if !bytes.HasPrefix(op.Seq, []byte(`"`)) {
			t.Errorf("op %d: seq %s is not a quoted decimal", i, op.Seq)
		}
		if !bytes.HasPrefix(op.ServerTS, []byte(`"`)) {
			t.Errorf("op %d: server_ts %s is not a quoted decimal — a number wedges the client", i, op.ServerTS)
		}
	}
}

// Fixtures are committed to a public repository; this is the last line of
// defence before a capture with real content slips through.
func TestFixturesCarryNoSecrets(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "fixtures", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range []string{"Bearer ", "/home/", "/Users/"} {
			if bytes.Contains(raw, []byte(marker)) {
				t.Errorf("%s contains %q — scrub it before committing", filepath.Base(p), marker)
			}
		}
		// A bare credential in a JSON field matches none of the markers above,
		// which is how a live pre-shared key once reached this directory.
		for _, re := range []*regexp.Regexp{
			regexp.MustCompile(`"(token|psk|key|secret|password)"\s*:\s*"[A-Za-z0-9_+/=-]{16,}"`),
			regexp.MustCompile(`\b\d{3}-\d{3}-\d{3}\b`),
		} {
			if re.Find(raw) != nil {
				t.Errorf("%s contains something shaped like a credential or pairing code", filepath.Base(p))
			}
		}
	}
}

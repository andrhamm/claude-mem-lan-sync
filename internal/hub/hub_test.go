package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
	"github.com/andrhamm/claude-mem-lan-sync/internal/store"
)

const testToken = "test-pre-shared-key"

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := New(st, Options{
		UserID: st.UserID(),
		Auth:   StaticToken{UserID: st.UserID(), Token: testToken},
		Now:    func() time.Time { return time.UnixMilli(1755300000000) },
	})
	return srv, st
}

func request(t *testing.T, srv *Server, method, path string, body []byte, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("X-User-Id", srv.opts.UserID)
	r.Header.Set("X-Device-Id", "device-A")
	r.Header.Set("X-Device-Name", "laptop")
	for _, m := range mutate {
		m(r)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

func makeWrapper(t *testing.T, localID, rev string) json.RawMessage {
	t.Helper()
	payload := `{"p":1}`
	body := fmt.Sprintf(
		`{"body_schema_version":1,"deleted":false,"deleted_at":null,"entity_rev":"%s",`+
			`"id":"observation:%s","kind":"observation","mutation":null,"origin_device_id":"device-A",`+
			`"origin_local_id":"%s","payload":%s,"payload_schema_version":2,"payload_sha256":"%s"}`,
		rev, localID, localID, payload, proto.Digest([]byte(payload)))
	w, err := json.Marshal(map[string]string{
		"body":             body,
		"operation_sha256": proto.Digest([]byte(body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func pushBody(t *testing.T, ops ...json.RawMessage) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"protocol_version": 2, "ops": ops})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStatusShape(t *testing.T) {
	srv, st := newTestServer(t)
	w := request(t, srv, http.MethodGet, "/v1/sync/status", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if string(got["protocol_version"]) != "2" {
		t.Errorf("protocol_version = %s, want the JSON number 2", got["protocol_version"])
	}
	// The client rejects these as anything but decimal strings.
	for _, k := range []string{"epoch", "head_seq", "projected_seq"} {
		if !strings.HasPrefix(string(got[k]), `"`) {
			t.Errorf("%s = %s, want a quoted decimal", k, got[k])
		}
	}
	if string(got["head_seq"]) != string(got["projected_seq"]) {
		t.Errorf("head_seq %s != projected_seq %s; only equality satisfies both routes",
			got["head_seq"], got["projected_seq"])
	}
	if string(got["epoch"]) != `"`+st.Epoch().String()+`"` {
		t.Errorf("epoch = %s", got["epoch"])
	}
}

// Until /v1/sync/ws exists, the header is what stops every device retrying a
// 404 once a minute forever.
func TestSyncModePollHeaderOnAllRoutes(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, tc := range []struct {
		method, path string
		body         []byte
	}{
		{http.MethodGet, "/v1/sync/status", nil},
		{http.MethodGet, "/v1/sync/changes?since=0&limit=500", nil},
		{http.MethodPost, "/v1/sync/ops", pushBody(t)},
	} {
		w := request(t, srv, tc.method, tc.path, tc.body)
		if got := w.Header().Get("X-Sync-Mode"); got != "poll" {
			t.Errorf("%s %s: X-Sync-Mode = %q, want poll", tc.method, tc.path, got)
		}
	}
}

func TestSyncModeSuppressedWhenWebSocketReady(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.opts.WebSocketReady = true

	w := request(t, srv, http.MethodGet, "/v1/sync/status", nil)
	if got := w.Header().Get("X-Sync-Mode"); got != "" {
		t.Errorf("X-Sync-Mode = %q, want it absent once the socket exists", got)
	}
}

func TestPushAckShape(t *testing.T) {
	srv, _ := newTestServer(t)
	w := request(t, srv, http.MethodPost, "/v1/sync/ops", pushBody(t, makeWrapper(t, "1", "1")))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}

	var got struct {
		Acked []map[string]json.RawMessage `json:"acked"`
		Head  json.RawMessage              `json:"head_seq"`
		Proj  json.RawMessage              `json:"projected_seq"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Acked) != 1 {
		t.Fatalf("acked %d ops, want 1", len(got.Acked))
	}
	for _, k := range []string{"id", "kind", "entity_rev", "operation_sha256", "seq", "origin_local_id"} {
		if _, ok := got.Acked[0][k]; !ok {
			t.Errorf("ack missing the mandatory field %q", k)
		}
	}
	if string(got.Head) != string(got.Proj) {
		t.Errorf("head_seq %s != projected_seq %s", got.Head, got.Proj)
	}
	if string(got.Acked[0]["seq"]) != `"1"` {
		t.Errorf("first seq = %s, want \"1\"", got.Acked[0]["seq"])
	}
}

// A mutation ack carries a null origin_local_id, and the key must be present:
// the client type-checks string|null, so an omitted key is undefined and throws.
func TestAckIncludesNullOriginLocalID(t *testing.T) {
	srv, _ := newTestServer(t)

	payload := "null"
	body := fmt.Sprintf(
		`{"body_schema_version":1,"deleted":false,"deleted_at":null,"entity_rev":"1",`+
			`"id":"mutation:0b7f3e2a-1c4d-4a6b-8f2e-9d0c1a2b3c4d","kind":"mutation",`+
			`"mutation":{"op":"set_title","title":"x"},"origin_device_id":"device-A",`+
			`"origin_local_id":null,"payload":null,"payload_schema_version":2,"payload_sha256":"%s"}`,
		proto.Digest([]byte(payload)))
	wrapper, err := json.Marshal(map[string]string{
		"body": body, "operation_sha256": proto.Digest([]byte(body)),
	})
	if err != nil {
		t.Fatal(err)
	}

	w := request(t, srv, http.MethodPost, "/v1/sync/ops", pushBody(t, wrapper))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"origin_local_id":null`) {
		t.Fatalf("ack omitted a null origin_local_id: %s", w.Body)
	}
}

func TestChangesShapeAndByteFidelity(t *testing.T) {
	srv, _ := newTestServer(t)
	wrapper := makeWrapper(t, "1", "1")

	if w := request(t, srv, http.MethodPost, "/v1/sync/ops", pushBody(t, wrapper)); w.Code != http.StatusOK {
		t.Fatalf("push failed: %s", w.Body)
	}

	w := request(t, srv, http.MethodGet, "/v1/sync/changes?since=0&limit=500", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}

	var got struct {
		ProtocolVersion json.RawMessage `json:"protocol_version"`
		Epoch           json.RawMessage `json:"epoch"`
		HeadSeq         json.RawMessage `json:"head_seq"`
		More            json.RawMessage `json:"more"`
		Ops             []struct {
			Seq      json.RawMessage `json:"seq"`
			ServerTS json.RawMessage `json:"server_ts"`
			Body     string          `json:"body"`
			Digest   string          `json:"operation_sha256"`
		} `json:"ops"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if string(got.ProtocolVersion) != "2" {
		t.Errorf("protocol_version = %s", got.ProtocolVersion)
	}
	if string(got.More) != "false" {
		t.Errorf("more = %s, want a boolean", got.More)
	}
	if len(got.Ops) != 1 {
		t.Fatalf("returned %d ops", len(got.Ops))
	}
	// A JSON number here wedges the client on its first pull.
	if !strings.HasPrefix(string(got.Ops[0].ServerTS), `"`) {
		t.Errorf("server_ts = %s, want a quoted decimal", got.Ops[0].ServerTS)
	}
	if !strings.HasPrefix(string(got.Ops[0].Seq), `"`) {
		t.Errorf("seq = %s, want a quoted decimal", got.Ops[0].Seq)
	}

	// Byte fidelity, compared on raw bytes rather than by JSON equality — a
	// JSONEq-style check would pass on exactly the HTML re-escaping we guard against.
	var sent struct {
		Body   string `json:"body"`
		Digest string `json:"operation_sha256"`
	}
	if err := json.Unmarshal(wrapper, &sent); err != nil {
		t.Fatal(err)
	}
	if got.Ops[0].Body != sent.Body {
		t.Error("body did not survive the round trip byte-for-byte")
	}
	if got.Ops[0].Digest != sent.Digest {
		t.Error("digest changed in transit")
	}
	if proto.Digest([]byte(got.Ops[0].Body)) != got.Ops[0].Digest {
		t.Error("returned body does not hash to its own digest")
	}
}

func TestChangesEmptyReturns200WithEmptyArray(t *testing.T) {
	srv, _ := newTestServer(t)
	w := request(t, srv, http.MethodGet, "/v1/sync/changes?since=0&limit=500", nil)

	// 204 would pass the client's res.ok check and then fail JSON parsing.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when empty", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ops":[]`) {
		t.Fatalf("body = %s, want an empty ops array", w.Body)
	}
}

func TestAuthFailures(t *testing.T) {
	srv, _ := newTestServer(t)

	cases := map[string]struct {
		mutate func(*http.Request)
		want   int
	}{
		"missing bearer": {func(r *http.Request) { r.Header.Del("Authorization") }, http.StatusUnauthorized},
		"wrong bearer":   {func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, http.StatusUnauthorized},
		"wrong user":     {func(r *http.Request) { r.Header.Set("X-User-Id", "someone-else") }, http.StatusUnauthorized},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := request(t, srv, http.MethodGet, "/v1/sync/status", nil, tc.mutate)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

// A device with a valid key but a stale user id must be refused, not given its
// own partition: otherwise it pushes its whole history, gets valid acks, stamps
// synced_at, and diverges silently while both machines report healthy sync.
func TestUserMismatchIsForbiddenNotAutoCreated(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	// Authenticator accepts any user id; only the hub's identity check should stop it.
	srv := New(st, Options{
		UserID: st.UserID(),
		Auth:   anyUserAuth{token: testToken},
	})

	w := request(t, srv, http.MethodGet, "/v1/sync/status", nil, func(r *http.Request) {
		r.Header.Set("X-User-Id", "typo-user-id")
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), string(proto.ReasonUserMismatch)) {
		t.Fatalf("body = %s", w.Body)
	}
}

type anyUserAuth struct{ token string }

func (a anyUserAuth) Verify(_, token string) bool { return token == a.token }

func TestRevokedDeviceRejected(t *testing.T) {
	srv, st := newTestServer(t)

	if w := request(t, srv, http.MethodPost, "/v1/sync/ops", pushBody(t, makeWrapper(t, "1", "1"))); w.Code != http.StatusOK {
		t.Fatalf("seed push failed: %s", w.Body)
	}
	if err := st.RevokeDevice(context.Background(), "device-A"); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/v1/sync/status", "/v1/sync/changes?since=0"} {
		w := request(t, srv, http.MethodGet, path, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s after revocation: status = %d, want 401", path, w.Code)
		}
	}
}

// Error bodies carry a fixed reason and nothing else: the client copies the
// first 200 bytes into its own log on another machine.
func TestErrorBodiesLeakNothing(t *testing.T) {
	srv, _ := newTestServer(t)

	secret := "SUPER-SECRET-PROMPT-TEXT"
	w := request(t, srv, http.MethodPost, "/v1/sync/ops",
		[]byte(`{"protocol_version":2,"ops":[{"body":"`+secret+`","operation_sha256":"x"}]}`))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("error body echoed request content: %s", w.Body)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["error"] == "" {
		t.Fatalf("error body = %s, want exactly one error field", w.Body)
	}
}

func TestNeverRedirectsOrReturns204(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, path := range []string{
		"/v1/sync/status/", "//v1/sync/status", "/v1/sync/changes/", "/unknown", "/v1/sync/../sync/status",
	} {
		w := request(t, srv, http.MethodGet, path, nil)
		if w.Code >= 300 && w.Code < 400 {
			t.Errorf("%s returned %d; fetch cannot follow a redirect", path, w.Code)
		}
		if w.Code == http.StatusNoContent || w.Code == http.StatusResetContent {
			t.Errorf("%s returned %d, which passes res.ok then fails JSON parsing", path, w.Code)
		}
	}
}

func TestHealthzSaysNothing(t *testing.T) {
	srv, _ := newTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if body != "ok" {
		t.Fatalf("healthz body = %q, want a bare ok with no version or counts", body)
	}
	if w.Header().Get("Server") != "" {
		t.Error("healthz advertised a Server header")
	}
}

func TestRejectsOriginAndContentEncoding(t *testing.T) {
	srv, _ := newTestServer(t)

	w := request(t, srv, http.MethodGet, "/v1/sync/status", nil, func(r *http.Request) {
		r.Header.Set("Origin", "http://evil.example")
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("Origin request: status = %d, want 400", w.Code)
	}

	w = request(t, srv, http.MethodPost, "/v1/sync/ops", pushBody(t), func(r *http.Request) {
		r.Header.Set("Content-Encoding", "gzip")
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("gzip request: status = %d, want 400", w.Code)
	}
}

func TestHostAllowlist(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.opts.AllowedHosts = []string{"hub.local:8787"}

	w := request(t, srv, http.MethodGet, "/v1/sync/status", nil, func(r *http.Request) {
		r.Host = "attacker.example"
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("disallowed host: status = %d, want 400", w.Code)
	}

	w = request(t, srv, http.MethodGet, "/v1/sync/status", nil, func(r *http.Request) {
		r.Host = "hub.local:8787"
	})
	if w.Code != http.StatusOK {
		t.Errorf("allowed host: status = %d, want 200", w.Code)
	}
}

func TestOversizeRequestRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.opts.MaxRequestBytes = 512

	big := bytes.Repeat([]byte("x"), 4096)
	w := request(t, srv, http.MethodPost, "/v1/sync/ops",
		[]byte(`{"protocol_version":2,"ops":[],"pad":"`+string(big)+`"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestProtocolVersionEnforced(t *testing.T) {
	srv, _ := newTestServer(t)
	w := request(t, srv, http.MethodPost, "/v1/sync/ops", []byte(`{"protocol_version":1,"ops":[]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), string(proto.ReasonProtocolVersion)) {
		t.Fatalf("body = %s", w.Body)
	}
}

// The hub shares a disk with the user's live memory database, so it refuses
// writes before filling it rather than taking claude-mem down with it.
func TestStorageFullReturns507(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.opts.MinFreeBytes = 1 << 30
	srv.opts.FreeBytes = func() (int64, error) { return 1024, nil }

	w := request(t, srv, http.MethodPost, "/v1/sync/ops", pushBody(t, makeWrapper(t, "1", "1")))
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", w.Code)
	}

	// And nothing was written.
	res := request(t, srv, http.MethodGet, "/v1/sync/changes?since=0", nil)
	if !strings.Contains(res.Body.String(), `"ops":[]`) {
		t.Fatalf("a rejected push still wrote to the log: %s", res.Body)
	}
}

func TestInFlightCap(t *testing.T) {
	srv, _ := newTestServer(t)
	// Fill the slot so the next request has nowhere to go.
	srv.inflight <- struct{}{}
	defer func() { <-srv.inflight }()
	for len(srv.inflight) < cap(srv.inflight) {
		srv.inflight <- struct{}{}
	}
	defer func() {
		for len(srv.inflight) > 1 {
			<-srv.inflight
		}
	}()

	w := request(t, srv, http.MethodGet, "/v1/sync/status", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when saturated", w.Code)
	}
}

func TestNewHTTPServerHasTimeouts(t *testing.T) {
	// Go's zero-value server has none, and that is reachable pre-auth.
	s := NewHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if s.ReadHeaderTimeout == 0 || s.ReadTimeout == 0 || s.WriteTimeout == 0 || s.IdleTimeout == 0 {
		t.Fatalf("missing timeout: %+v", s)
	}
	if s.MaxHeaderBytes == 0 {
		t.Fatal("MaxHeaderBytes not set")
	}
}

func TestBadCursorRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, q := range []string{"since=-1", "since=01", "since=abc"} {
		w := request(t, srv, http.MethodGet, "/v1/sync/changes?"+q, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, w.Code)
		}
	}
}

func TestLimitIsClampedNotRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	if w := request(t, srv, http.MethodPost, "/v1/sync/ops", pushBody(t, makeWrapper(t, "1", "1"))); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}

	// The client always sends 500; rejecting a value it will simply repeat would
	// stall its pulls for no benefit.
	for _, q := range []string{"limit=100000", "limit=0", "limit=1"} {
		w := request(t, srv, http.MethodGet, "/v1/sync/changes?since=0&"+q, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", q, w.Code)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv, _ := newTestServer(t)
	w := request(t, srv, http.MethodPost, "/v1/sync/status", []byte(`{}`))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestResponsesCarryContentLength(t *testing.T) {
	srv, _ := newTestServer(t)
	w := request(t, srv, http.MethodGet, "/v1/sync/status", nil)

	if w.Header().Get("Content-Length") == "" {
		t.Fatal("no Content-Length on a protocol response")
	}
	body, _ := io.ReadAll(w.Body)
	if w.Header().Get("Content-Length") != itoa(len(body)) {
		t.Fatalf("Content-Length %q does not match body length %d", w.Header().Get("Content-Length"), len(body))
	}
}

// Identity is mandatory. If an absent X-Device-Id meant "unknown device, allow",
// a revoked device would regain access simply by dropping the header.
func TestMissingDeviceIDRejected(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, path := range []string{"/v1/sync/status", "/v1/sync/changes?since=0"} {
		w := request(t, srv, http.MethodGet, path, nil, func(r *http.Request) {
			r.Header.Del("X-Device-Id")
		})
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s without a device id: status = %d, want 400", path, w.Code)
		}
	}
}

func TestRevokedDeviceCannotEvadeByChangingHeader(t *testing.T) {
	srv, st := newTestServer(t)

	if w := request(t, srv, http.MethodPost, "/v1/sync/ops", pushBody(t, makeWrapper(t, "1", "1"))); w.Code != http.StatusOK {
		t.Fatalf("seed push failed: %s", w.Body)
	}
	if err := st.RevokeDevice(context.Background(), "device-A"); err != nil {
		t.Fatal(err)
	}

	// The revoked device is refused.
	if w := request(t, srv, http.MethodGet, "/v1/sync/status", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("revoked device: status = %d, want 401", w.Code)
	}
	// And it cannot simply omit the header to become anonymous.
	w := request(t, srv, http.MethodGet, "/v1/sync/status", nil, func(r *http.Request) {
		r.Header.Del("X-Device-Id")
	})
	if w.Code == http.StatusOK {
		t.Error("dropping X-Device-Id let a revoked device back in")
	}
}

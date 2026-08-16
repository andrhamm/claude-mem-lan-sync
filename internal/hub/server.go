// Package hub serves claude-mem's sync protocol.
//
// Two rules shape everything here. First, response bytes are assembled by hand
// rather than marshalled, because the client re-canonicalises every op body it
// receives and compares it to the raw string — and encoding/json rewrites <, >
// and & even inside a json.RawMessage. Second, rejection is expensive: the
// client has no dead-letter path for HTTP failures, so a 4xx parks that op at
// the head of its outbox and blocks everything behind it forever. Validation is
// therefore minimal, and every rejection is a deliberate choice.
package hub

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"time"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
	"github.com/andrhamm/claude-mem-lan-sync/internal/store"
)

// Authenticator verifies a bearer token for a user.
type Authenticator interface {
	Verify(userID, token string) bool
}

// StaticToken authenticates against one pre-shared key.
type StaticToken struct {
	UserID string
	Token  string
}

// Verify compares in constant time so a token cannot be recovered by timing.
func (s StaticToken) Verify(userID, token string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(userID), []byte(s.UserID)) == 1
	tokenOK := subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) == 1
	return userOK && tokenOK
}

// Options configures a Server. Everything injectable is injected so the tests
// need no clock, no disk, and no real listener.
type Options struct {
	UserID string
	Auth   Authenticator

	Now func() time.Time

	MaxRequestBytes  int
	MaxBodyBytes     int
	MaxResponseBytes int
	MaxOpsPerPush    int
	MaxInFlight      int

	// MinFreeBytes refuses writes when the filesystem is nearly full. The hub
	// shares a disk with the user's live memory database, so its own growth must
	// not become claude-mem's outage.
	MinFreeBytes int64
	FreeBytes    func() (int64, error)

	// AllowedHosts, when non-empty, restricts the Host header. Combined with the
	// Origin rejection below this closes browser-driven and DNS-rebinding paths.
	AllowedHosts []string

	Logger *slog.Logger

	// WebSocketReady suppresses the X-Sync-Mode: poll hint once /v1/sync/ws
	// exists. Until then the header stops every device retrying a 404 once a
	// minute, forever.
	WebSocketReady bool
}

func (o *Options) withDefaults() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxRequestBytes <= 0 {
		o.MaxRequestBytes = proto.MaxRequestBytes
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = proto.MaxBodyBytes
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = store.DefaultMaxPageBytes
	}
	if o.MaxOpsPerPush <= 0 {
		o.MaxOpsPerPush = proto.MaxOpsPerPush
	}
	if o.MaxInFlight <= 0 {
		o.MaxInFlight = 64
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
}

// Server implements the protocol routes.
type Server struct {
	st       *store.Store
	opts     Options
	inflight chan struct{}
	pair     PairExchanger
}

// PairExchanger redeems a pairing code for the pre-shared key. Supplied by the
// pair package; nil disables the route.
type PairExchanger interface {
	Redeem(code string) (token string, err error)
}

// New builds a Server.
func New(st *store.Store, opts Options) *Server {
	opts.withDefaults()
	return &Server{
		st:       st,
		opts:     opts,
		inflight: make(chan struct{}, opts.MaxInFlight),
	}
}

// SetPairExchanger enables POST /pair.
func (s *Server) SetPairExchanger(p PairExchanger) { s.pair = p }

// Handler returns the routed, wrapped handler.
func (s *Server) Handler() http.Handler {
	return s.recoverPanics(s.limitInFlight(s.checkHostAndOrigin(http.HandlerFunc(s.route))))
}

// route dispatches on an exact path.
//
// http.ServeMux would 301 on a trailing slash or a doubled separator, and the
// client's fetch cannot follow a redirect: it would surface as an unexplained
// sync failure. Matching exactly means no route can ever produce a 3xx.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet {
			s.writeError(w, http.StatusMethodNotAllowed, proto.ReasonBadRequest)
			return
		}
		// Deliberately says nothing: no version, no hub id, no counts.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))

	case "/v1/sync/status":
		s.authed(w, r, http.MethodGet, s.handleStatus)

	case "/v1/sync/ops":
		s.authed(w, r, http.MethodPost, s.handlePush)

	case "/v1/sync/changes":
		s.authed(w, r, http.MethodGet, s.handleChanges)

	case "/pair":
		s.handlePair(w, r)

	default:
		s.writeError(w, http.StatusNotFound, proto.ReasonBadRequest)
	}
}

// NewHTTPServer wraps the handler with the timeouts Go's zero value omits.
//
// An http.Server with no timeouts can be held open indefinitely by a single LAN
// machine dribbling one header byte per minute, and that is reachable before any
// authentication runs.
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

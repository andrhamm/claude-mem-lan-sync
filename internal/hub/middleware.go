package hub

import (
	"net"
	"net/http"
	"strings"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
)

// writeError sends a fixed-vocabulary reason and nothing else.
//
// The client slices the first 200 bytes of an error body into its own log on
// another machine, so echoing any part of a request — a path, a header, a body
// fragment — would copy memory content onto a second device. The reason enum is
// the whole payload.
func (s *Server) writeError(w http.ResponseWriter, status int, reason proto.RejectReason) {
	body := []byte(`{"error":"` + string(reason) + `"}`)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(len(body)))
	s.setSyncMode(w)
	w.WriteHeader(status)
	// reason comes from a fixed enum, so body contains no caller-supplied bytes.
	_, _ = w.Write(body) //nolint:gosec // no request data reaches this response
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// setSyncMode tells the client not to attempt a WebSocket.
//
// The client enables WebSockets by default and reconnects with a backoff capped
// at 60s, so without this every paired device hammers a 404 once a minute
// forever. Any other value, including an absent header, re-enables the socket,
// so this self-corrects when the endpoint ships.
func (s *Server) setSyncMode(w http.ResponseWriter) {
	if !s.opts.WebSocketReady {
		w.Header().Set("X-Sync-Mode", "poll")
	}
}

// authed applies method, authentication, identity, and revocation checks.
func (s *Server) authed(w http.ResponseWriter, r *http.Request, method string, h func(http.ResponseWriter, *http.Request)) {
	if r.Method != method {
		s.writeError(w, http.StatusMethodNotAllowed, proto.ReasonBadRequest)
		return
	}

	userID := r.Header.Get("X-User-Id")
	token := bearerToken(r)

	if s.opts.Auth == nil || !s.opts.Auth.Verify(userID, token) {
		// A bad token is indistinguishable from an outage to the client — it
		// retries silently forever — so doctor is what surfaces this to a human.
		s.writeError(w, http.StatusUnauthorized, proto.ReasonUnauthorized)
		return
	}

	// A partition is never created implicitly. A device configured with a stale
	// user id but a valid key would otherwise push its whole history, receive
	// valid acks, stamp synced_at, and never push again — while the other device
	// sees nothing and both report healthy sync.
	if userID != s.opts.UserID {
		s.writeError(w, http.StatusForbidden, proto.ReasonUserMismatch)
		return
	}

	deviceID := r.Header.Get("X-Device-Id")
	if deviceID != "" {
		revoked, err := s.st.DeviceRevoked(r.Context(), deviceID)
		if err != nil {
			s.opts.Logger.Error("checking device revocation", "error", err)
			s.writeError(w, http.StatusInternalServerError, proto.ReasonInternal)
			return
		}
		if revoked {
			s.writeError(w, http.StatusUnauthorized, proto.ReasonUnauthorized)
			return
		}
	}

	h(w, r)
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
		return v[len(prefix):]
	}
	return ""
}

// checkHostAndOrigin blocks browser-driven and DNS-rebinding requests.
//
// The real client never sends an Origin header, so its presence means something
// else is talking to us — including a page loaded in a browser on this machine.
func (s *Server) checkHostAndOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			s.writeError(w, http.StatusBadRequest, proto.ReasonBadRequest)
			return
		}
		if r.Header.Get("Content-Encoding") != "" {
			// Refusing compression avoids an unbounded decompression path.
			s.writeError(w, http.StatusBadRequest, proto.ReasonBadRequest)
			return
		}
		if len(s.opts.AllowedHosts) > 0 && !s.hostAllowed(r.Host) {
			s.writeError(w, http.StatusBadRequest, proto.ReasonBadRequest)
			return
		}
		// No versioned Server header: it is free reconnaissance.
		w.Header()["Server"] = nil
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	for _, allowed := range s.opts.AllowedHosts {
		a, _, err := net.SplitHostPort(allowed)
		if err != nil {
			a = allowed
		}
		if strings.EqualFold(h, a) {
			return true
		}
	}
	return false
}

// limitInFlight bounds concurrent requests so one machine cannot exhaust memory
// or connections.
func (s *Server) limitInFlight(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.inflight <- struct{}{}:
			defer func() { <-s.inflight }()
			next.ServeHTTP(w, r)
		default:
			s.writeError(w, http.StatusServiceUnavailable, proto.ReasonOverloaded)
		}
	})
}

// recoverPanics keeps one malformed request from taking down the hub, and in
// particular from killing the process that owns the sequencer.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				// The panic value may contain request data, so it is logged as a
				// type only, never formatted into the response.
				s.opts.Logger.Error("panic serving request", "path", r.URL.Path)
				s.writeError(w, http.StatusInternalServerError, proto.ReasonInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

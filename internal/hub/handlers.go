package hub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
)

// writeJSON sends a pre-rendered body with an explicit length.
func (s *Server) writeJSON(w http.ResponseWriter, buf *bytes.Buffer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", itoa(buf.Len()))
	s.setSyncMode(w)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	head, err := s.st.HeadSeq(r.Context())
	if err != nil {
		s.opts.Logger.Error("reading head_seq", "error", err)
		s.writeError(w, http.StatusInternalServerError, proto.ReasonInternal)
		return
	}
	s.recordDevice(r)

	var buf bytes.Buffer
	buf.WriteString(`{"protocol_version":2,"epoch":"`)
	buf.WriteString(s.st.Epoch().String())
	buf.WriteString(`","head_seq":"`)
	buf.WriteString(head.String())
	// projected_seq always equals head_seq: /status rejects projected > head and
	// /ops rejects head > projected, so equality is the only value satisfying both.
	buf.WriteString(`","projected_seq":"`)
	buf.WriteString(head.String())
	buf.WriteString(`"}`)

	s.writeJSON(w, &buf)
}

type pushRequest struct {
	ProtocolVersion int               `json:"protocol_version"`
	Ops             []json.RawMessage `json:"ops"`
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if err := s.checkFreeSpace(); err != nil {
		// 507 rather than a silent failure: the hub shares a disk with the user's
		// live memory database and must not take it down.
		s.opts.Logger.Error("refusing push, free space below the floor", "error", err)
		s.writeError(w, http.StatusInsufficientStorage, proto.ReasonStorageFull)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, int64(s.opts.MaxRequestBytes))
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, proto.ReasonTooLarge)
		return
	}

	var req pushRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, proto.ReasonBadRequest)
		return
	}
	if req.ProtocolVersion != 2 {
		s.writeError(w, http.StatusBadRequest, proto.ReasonProtocolVersion)
		return
	}
	if len(req.Ops) > s.opts.MaxOpsPerPush {
		s.writeError(w, http.StatusBadRequest, proto.ReasonTooLarge)
		return
	}

	ops := make([]proto.Op, 0, len(req.Ops))
	for _, rawOp := range req.Ops {
		op, err := proto.ParseOp(rawOp)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, proto.ReasonOf(err))
			return
		}
		ops = append(ops, op)
	}

	res, err := s.st.Push(r.Context(), ops,
		r.Header.Get("X-Device-Id"), r.Header.Get("X-Device-Name"))
	if err != nil {
		if reason := proto.ReasonOf(err); reason == proto.ReasonUnauthorized {
			s.writeError(w, http.StatusUnauthorized, reason)
			return
		}
		s.opts.Logger.Error("push failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, proto.ReasonInternal)
		return
	}

	var buf bytes.Buffer
	buf.WriteString(`{"acked":[`)
	for i, ack := range res.Acks {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := proto.EmitAck(&buf, ack); err != nil {
			s.opts.Logger.Error("emitting ack", "error", err)
			s.writeError(w, http.StatusInternalServerError, proto.ReasonInternal)
			return
		}
	}
	buf.WriteString(`],"head_seq":"`)
	buf.WriteString(res.HeadSeq.String())
	buf.WriteString(`","projected_seq":"`)
	buf.WriteString(res.ProjectedSeq.String())
	buf.WriteString(`"}`)

	s.writeJSON(w, &buf)
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	since := proto.Dec(0)
	if v := q.Get("since"); v != "" {
		parsed, err := proto.ParseDec(v)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, proto.ReasonBadCursor)
			return
		}
		since = parsed
	}

	// The limit is clamped rather than rejected: the client always sends 500, and
	// rejecting a request the client will simply repeat would stall its pulls for
	// no benefit. Recorded as a deliberate deviation in docs/protocol.md.
	limit := proto.MaxPageLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, proto.ReasonBadRequest)
			return
		}
		if n > 0 && n < limit {
			limit = n
		}
	}

	res, err := s.st.Changes(r.Context(), since, limit, s.opts.MaxResponseBytes)
	if err != nil {
		if proto.ReasonOf(err) == proto.ReasonBadCursor {
			s.writeError(w, http.StatusBadRequest, proto.ReasonBadCursor)
			return
		}
		s.opts.Logger.Error("reading changes", "error", err)
		s.writeError(w, http.StatusInternalServerError, proto.ReasonInternal)
		return
	}
	s.recordDevice(r)

	var buf bytes.Buffer
	buf.WriteString(`{"protocol_version":2,"epoch":"`)
	buf.WriteString(res.Epoch.String())
	buf.WriteString(`","head_seq":"`)
	buf.WriteString(res.HeadSeq.String())
	buf.WriteString(`","more":`)
	if res.More {
		buf.WriteString("true")
	} else {
		buf.WriteString("false")
	}
	buf.WriteString(`,"ops":[`)
	for i, op := range res.Ops {
		if i > 0 {
			buf.WriteByte(',')
		}
		// server_ts is emitted as a decimal string: a JSON number here makes the
		// client throw while applying the page, and it then retries that same page
		// forever without advancing its cursor.
		if err := proto.EmitChangeOp(&buf, op.Seq, op.ServerTS, proto.Op{
			RawLiteral: op.RawLiteral,
			Digest:     op.Digest,
		}); err != nil {
			s.opts.Logger.Error("emitting change", "error", err)
			s.writeError(w, http.StatusInternalServerError, proto.ReasonInternal)
			return
		}
	}
	buf.WriteString(`]}`)

	s.writeJSON(w, &buf)
}

// recordDevice notes a device that only ever pulls, so it still appears in
// `cmemlan devices`. Failures are logged, never fatal to the request.
func (s *Server) recordDevice(r *http.Request) {
	id := r.Header.Get("X-Device-Id")
	if id == "" {
		return
	}
	if err := s.st.SeenDevice(r.Context(), id, r.Header.Get("X-Device-Name")); err != nil {
		s.opts.Logger.Warn("recording device", "error", err)
	}
}

func (s *Server) checkFreeSpace() error {
	if s.opts.MinFreeBytes <= 0 || s.opts.FreeBytes == nil {
		return nil
	}
	free, err := s.opts.FreeBytes()
	if err != nil {
		return nil // never fail a push because the check itself failed
	}
	if free < s.opts.MinFreeBytes {
		return proto.Reject(proto.ReasonStorageFull)
	}
	return nil
}

// handlePair exchanges a pairing code for the pre-shared key. It is our own
// endpoint, outside the claude-mem protocol surface.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if s.pair == nil {
		s.writeError(w, http.StatusNotFound, proto.ReasonBadRequest)
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, proto.ReasonBadRequest)
		return
	}
	if ct := r.Header.Get("Content-Type"); !isJSONContentType(ct) {
		s.writeError(w, http.StatusBadRequest, proto.ReasonBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, proto.ReasonBadRequest)
		return
	}

	token, err := s.pair.Redeem(req.Code)
	if err != nil {
		s.opts.Logger.Warn("pairing attempt rejected")
		s.writeError(w, http.StatusUnauthorized, proto.ReasonUnauthorized)
		return
	}

	var buf bytes.Buffer
	buf.WriteString(`{"token":`)
	buf.WriteString(strconv.Quote(token))
	buf.WriteString(`,"user_id":`)
	buf.WriteString(strconv.Quote(s.opts.UserID))
	buf.WriteString(`}`)
	s.writeJSON(w, &buf)
}

func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	for i := 0; i < len(ct); i++ {
		if ct[i] == ';' {
			ct = ct[:i]
			break
		}
	}
	return ct == "application/json"
}

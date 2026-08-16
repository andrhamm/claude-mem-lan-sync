package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Recorder captures request/response pairs for fixture generation.
//
// What it writes is real traffic: observation narratives, full prompt text, and
// bearer tokens. Raw captures are for local use only — `cmemlan fixtures scrub`
// produces the committable form, and CI fails if an unscrubbed one is committed.
type Recorder struct {
	dir string
	mu  sync.Mutex
	n   int
}

// NewRecorder prepares a capture directory.
func NewRecorder(dir string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("hub: creating the capture directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if inside, root := insideGitWorkTree(dir); inside && os.Getenv("CMEMLAN_RECORD_IN_REPO") != "1" {
		return nil, fmt.Errorf(
			"hub: refusing to capture inside the git work tree at %s — captured traffic contains real "+
				"prompt text and bearer tokens.\nUse a directory outside the repository, or set "+
				"CMEMLAN_RECORD_IN_REPO=1 if you are certain", root)
	}
	return &Recorder{dir: dir}, nil
}

// insideGitWorkTree walks upward looking for a .git entry.
func insideGitWorkTree(dir string) (bool, string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, ""
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
			return true, abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return false, ""
		}
		abs = parent
	}
}

// Exchange is one captured request/response pair.
type Exchange struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Query    string            `json:"query"`
	Headers  map[string]string `json:"headers"`
	Request  json.RawMessage   `json:"request,omitempty"`
	Status   int               `json:"status"`
	Response json.RawMessage   `json:"response,omitempty"`
}

func (r *Recorder) write(e Exchange) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.n++
	name := filepath.Join(r.dir, fmt.Sprintf("exchange-%04d.json", r.n))
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(name, b, 0o600)
}

// capturingWriter records the response while still writing it.
type capturingWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *capturingWriter) WriteHeader(status int) {
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *capturingWriter) Write(b []byte) (int, error) {
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}

// record wraps a handler with capture, when a recorder is configured.
func (s *Server) record(next http.Handler) http.Handler {
	if s.recorder == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody []byte
		if r.Body != nil {
			reqBody, _ = io.ReadAll(io.LimitReader(r.Body, int64(s.opts.MaxRequestBytes)))
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
		}

		cw := &capturingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cw, r)

		headers := map[string]string{}
		for _, k := range []string{"Authorization", "X-User-Id", "X-Device-Id", "X-Device-Name", "Content-Type"} {
			if v := r.Header.Get(k); v != "" {
				headers[k] = v
			}
		}

		e := Exchange{
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Headers: headers,
			Status:  cw.status,
		}
		if json.Valid(reqBody) {
			e.Request = reqBody
		}
		if json.Valid(cw.buf.Bytes()) {
			e.Response = bytes.Clone(cw.buf.Bytes())
		}
		r2 := e
		s.recorder.write(r2)
	})
}

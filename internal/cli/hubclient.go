package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// httpHubClient talks to a hub over HTTP. Injected through Env so doctor and
// status can be tested without a listener.
type httpHubClient struct {
	client *http.Client
}

func newHubClient() *httpHubClient {
	return &httpHubClient{client: &http.Client{Timeout: 10 * time.Second}}
}

func (h *httpHubClient) Status(ctx context.Context, url, userID, token string) (StatusResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/v1/sync/status", nil)
	if err != nil {
		return StatusResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-Id", userID)

	resp, err := h.client.Do(req)
	if err != nil {
		return StatusResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// The client itself cannot tell a bad key from an outage — it retries
		// silently forever — so naming it here is the whole point of doctor.
		return StatusResult{}, fmt.Errorf("hub rejected our credentials (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return StatusResult{}, fmt.Errorf("hub returned HTTP %d", resp.StatusCode)
	}

	var body struct {
		Epoch        string `json:"epoch"`
		HeadSeq      string `json:"head_seq"`
		ProjectedSeq string `json:"projected_seq"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return StatusResult{}, fmt.Errorf("hub response was not valid JSON: %w", err)
	}
	return StatusResult{
		Epoch:        body.Epoch,
		HeadSeq:      body.HeadSeq,
		ProjectedSeq: body.ProjectedSeq,
		SyncMode:     resp.Header.Get("X-Sync-Mode"),
	}, nil
}

// pairResult is what POST /pair returns.
type pairResult struct {
	Token  string `json:"token"`
	UserID string `json:"user_id"`
}

func (h *httpHubClient) redeem(ctx context.Context, url, code string) (pairResult, error) {
	payload, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return pairResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(url, "/")+"/pair", bytes.NewReader(payload))
	if err != nil {
		return pairResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return pairResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return pairResult{}, fmt.Errorf(
			"pairing was refused (HTTP %d) — the code may be wrong, used, or expired", resp.StatusCode)
	}

	var out pairResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return pairResult{}, err
	}
	if out.Token == "" || out.UserID == "" {
		return pairResult{}, fmt.Errorf("the hub returned an incomplete pairing response")
	}
	return out, nil
}

func (e Env) hub() HubClient {
	if e.Hub != nil {
		return e.Hub
	}
	return newHubClient()
}

func (e Env) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

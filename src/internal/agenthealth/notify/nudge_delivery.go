package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// nudgeRequest mirrors internal/server's wire shape for POST /nudge.
// Duplicated rather than imported to keep this package decoupled from
// internal/server — the same rationale as internal/mcp/roster and
// internal/mcp/health duplicating each other's request-identity types.
type nudgeRequest struct {
	SessionID string `json:"session_id"`
	AgentName string `json:"agent_name"`
	Message   string `json:"message"`
}

// NudgeDelivery implements Delivery by POSTing pressure alerts to hookd's
// /nudge endpoint, for one-shot in-band delivery as additionalContext on the
// target agent's next PreToolUse/UserPromptSubmit turn.
type NudgeDelivery struct {
	nudgeURL string
	client   *http.Client
}

// NewNudgeDelivery returns a NudgeDelivery that POSTs to nudgeURL — the full
// hookd /nudge endpoint URL (e.g. "http://localhost:9125/nudge"), matching
// the convention set by wms.NewHookObserver (caller passes the ready-made
// endpoint URL, not a base host).
func NewNudgeDelivery(nudgeURL string) *NudgeDelivery {
	return &NudgeDelivery{
		nudgeURL: nudgeURL,
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

// Deliver posts the Alert to the /nudge endpoint. Best-effort from the
// engine's point of view (Evaluate ignores the returned error), but callers
// that want to log failures can still inspect it directly.
func (d *NudgeDelivery) Deliver(ctx context.Context, a Alert) error {
	body, err := json.Marshal(nudgeRequest{
		SessionID: a.SessionID,
		AgentName: a.AgentName,
		Message:   a.Message,
	})
	if err != nil {
		return fmt.Errorf("marshal nudge request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.nudgeURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build nudge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("post nudge: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nudge endpoint returned status %d", resp.StatusCode)
	}
	return nil
}

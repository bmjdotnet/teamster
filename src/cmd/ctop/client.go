package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bmjdotnet/teamster/internal/render"
)

// Agent mirrors internal/mcp/health's agentHealthView wire fields (§1.2).
// Defined locally — ctop is an API client and must not import
// internal/mcp/health or internal/agenthealth/... (D3).
type Agent struct {
	RosterID            *string `json:"roster_id"`
	Host                string  `json:"host"`
	Username            string  `json:"username,omitempty"`
	SessionID           string  `json:"session_id"`
	AgentName           string  `json:"agent_name"`
	Runtime             string  `json:"runtime"`
	Model               string  `json:"model"`
	TeamName            string  `json:"team_name,omitempty"`
	Relationship        string  `json:"relationship,omitempty"`
	ParentRef           *string `json:"parent_ref,omitempty"`
	Liveness            string  `json:"liveness,omitempty"`
	ContextFillPct      float64 `json:"context_fill_pct"`
	SessionCostUSD      float64 `json:"session_cost_usd"`
	SessionTotalCostUSD float64 `json:"session_total_cost_usd,omitempty"`
	PressureLevel       string  `json:"pressure_level"`
	CollectorStatus     string  `json:"collector_status"`
	TokensInTotal       int64   `json:"tokens_in_total"`
	TokensOutTotal      int64   `json:"tokens_out_total"`
	ToolCallsTotal      int64   `json:"tool_calls_total"`
	LastActivityTs      *string `json:"last_activity_ts,omitempty"`
	LastActivityDisplay string  `json:"last_activity_display,omitempty"`
	LastActivityTag     string  `json:"last_activity_tag,omitempty"`
	CurrentFocus        string  `json:"current_focus,omitempty"`
	CompositionJSON     *string `json:"composition_json,omitempty"`
}

// SelectionKey returns the stable identity used to keep a selection anchored
// across a refresh: roster_id when present, else session_id|agent_name.
func (a Agent) SelectionKey() string {
	if a.RosterID != nil && *a.RosterID != "" {
		return *a.RosterID
	}
	return a.SessionID + "|" + a.AgentName
}

// Snapshot mirrors agentSnapshotView — the flattened embed of agentHealthView
// plus the extra fields returned only by /health/api/agents/{roster_id}.
type Snapshot struct {
	Agent
	ContextWindowTokens   int64   `json:"context_window_tokens"`
	ContextTokensUsed     int64   `json:"context_tokens_used"`
	ContextTokensFree     int64   `json:"context_tokens_free"`
	LongContextActive     bool    `json:"long_context_active"`
	ContextResetSuspected bool    `json:"context_reset_suspected"`
	CompositionJSON       *string `json:"composition_json,omitempty"`
	ToolCallCountsJSON    *string `json:"tool_call_counts_json,omitempty"`
	StatuslineJSON        *string `json:"statusline_json,omitempty"`
	FidelityNotes         *string `json:"fidelity_notes,omitempty"`
}

// hubClient is a pure HTTP client over hookd's /health/api/* + /health/stream
// surface. No DB imports, no internal/agenthealth/gauge (D3) — works
// identically hub-local and remote.
type hubClient struct {
	base string
	http *http.Client
}

func newHubClient(base string) *hubClient {
	return &hubClient{
		base: strings.TrimSuffix(base, "/"),
		http: &http.Client{Timeout: 4 * time.Second},
	}
}

// apiError carries a non-2xx response's {"error": "..."} body, if present.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string {
	if e.msg != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.msg, e.status)
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

func (c *hubClient) getJSON(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return &apiError{status: resp.StatusCode, msg: body.Error}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ListAgents fetches GET /health/api/agents. A `null` response body (nil
// slice) is treated as an empty list (§1.1 gotcha).
//
// An empty liveness (the default agents-grid scope, scopeMode.livenessParam()
// == nil) explicitly requests live+idle+stale+closed rather than leaving the
// liveness param off the request — the server's own unfiltered default is
// live+idle only, which drops an agent to invisible the moment it goes quiet
// (e.g. the operator reading, or between turns). "closed" has to be
// included too, not just "stale": a teammate's session goes straight to
// closed on Stop — it never passes through stale — so excluding closed left
// the whole team invisible between turns, with only the lead (who stays
// bound to a live session) surviving as "stale". Dimmed "✕ closed" in the
// status dot (statusDots in format.go) is what visually distinguishes a
// finished session from a live one now, not the query filter.
func (c *hubClient) ListAgents(ctx context.Context, host, runtime string, liveness []string) ([]Agent, error) {
	q := url.Values{}
	if host != "" {
		q.Set("host", host)
	}
	if runtime != "" {
		q.Set("runtime", runtime)
	}
	if len(liveness) == 0 {
		liveness = []string{"live", "idle", "stale", "closed"}
	}
	for _, l := range liveness {
		q.Add("liveness", l)
	}
	path := "/health/api/agents"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var agents []Agent
	if err := c.getJSON(ctx, path, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

// Snapshot fetches GET /health/api/agents/{roster_id}.
func (c *hubClient) Snapshot(ctx context.Context, rosterID string) (*Snapshot, error) {
	path := "/health/api/agents/" + url.PathEscape(rosterID)
	var snap Snapshot
	if err := c.getJSON(ctx, path, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// Alerts fetches GET /health/api/alerts.
func (c *hubClient) Alerts(ctx context.Context) ([]Agent, error) {
	var alerts []Agent
	if err := c.getJSON(ctx, "/health/api/alerts", &alerts); err != nil {
		return nil, err
	}
	return alerts, nil
}

// Outcome mirrors internal/wms.Outcome's wire fields actually needed by the
// Focus view — a subset, not the full struct (prior_status/origin_host/
// origin_session/origin_agent omitted, unused here). Defined locally per
// the same D3 rationale as Agent/Snapshot above: ctop must not import
// internal/wms.
type Outcome struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Focus       string    `json:"focus"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Tags is not part of the wms_listOutcomes wire response — it's filled in
	// by ListOutcomes via a per-outcome wms_getEntityTags call, so the Teams
	// view can join a team's outcome by its "team" tag without a second round
	// of client plumbing.
	Tags map[string]string `json:"-"`
}

// WorkUnit mirrors internal/wms.WorkUnit's wire fields the Focus view needs.
// AgentID (json:"agent_id") is set by wms_assignWorkUnit/wms_claimWorkUnit —
// wms_claimWorkUnit in particular sets it to the calling session's bare
// agent_type (e.g. "store", not "@store" — see internal/mcp/wms/wms.go's
// ToolClaimWorkUnit case), the same bare form Agent.AgentName already uses
// throughout this client, so matching the two needs no "@"-stripping.
type WorkUnit struct {
	ID        string    `json:"id"`
	OutcomeID string    `json:"outcome_id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Focus     string    `json:"focus"`
	AgentID   string    `json:"agent_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// rpcContent is one entry of the MCP tools/call content envelope
// (internal/mcp/wms.Result) — only the "text" variant is ever produced by
// wms-mcp's JSONResult/TextResult helpers.
type rpcContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// rpcEnvelope is the JSON-RPC 2.0 response body POST /mcp/wms returns —
// mirrors internal/server.writeRPCResponse/writeRPCError's wire shape.
type rpcEnvelope struct {
	Result *struct {
		Content []rpcContent `json:"content"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// WMSCall POSTs a JSON-RPC 2.0 tools/call request to hookd's /mcp/wms and
// returns the raw JSON text embedded in the MCP content envelope (the
// tool's actual result — e.g. a JSON array of Outcome/WorkUnit). args may
// be nil for a tool that takes no arguments.
func (c *hubClient) WMSCall(ctx context.Context, tool string, args map[string]interface{}) (json.RawMessage, error) {
	if args == nil {
		args = map[string]interface{}{}
	}
	body, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      tool,
			"arguments": args,
		},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/mcp/wms", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var env rpcEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	if env.Error != nil {
		return nil, &apiError{status: resp.StatusCode, msg: env.Error.Message}
	}
	if env.Result == nil || len(env.Result.Content) == 0 {
		return nil, fmt.Errorf("wms call %s: empty result", tool)
	}
	return json.RawMessage(env.Result.Content[0].Text), nil
}

// ListOutcomes calls wms_listOutcomes. status filters by lifecycle state —
// "open" (the value the Teams view uses) returns all non-terminal outcomes.
// Each outcome's Tags is then filled in via a per-outcome EntityTags call —
// best-effort: a failed tag fetch just leaves that outcome's Tags nil rather
// than failing the whole poll, since the Teams view's team→outcome join
// degrades gracefully to "no active outcome" either way.
func (c *hubClient) ListOutcomes(ctx context.Context, status string) ([]Outcome, error) {
	raw, err := c.WMSCall(ctx, "wms_listOutcomes", map[string]interface{}{"status": status})
	if err != nil {
		return nil, err
	}
	var outcomes []Outcome
	if err := json.Unmarshal(raw, &outcomes); err != nil {
		return nil, err
	}
	for i := range outcomes {
		if tags, err := c.EntityTags(ctx, "outcome", outcomes[i].ID); err == nil {
			outcomes[i].Tags = tags
		}
	}
	return outcomes, nil
}

// EntityTags calls wms_getEntityTags and flattens the returned []EntityTag
// into a plain tagKey->tagValue map — the only shape ListOutcomes' team join
// needs, not the full binding metadata (source/category/applied_at).
func (c *hubClient) EntityTags(ctx context.Context, entityType, entityID string) (map[string]string, error) {
	raw, err := c.WMSCall(ctx, "wms_getEntityTags", map[string]interface{}{"entityType": entityType, "entityID": entityID})
	if err != nil {
		return nil, err
	}
	var tags []struct {
		TagKey   string `json:"tag_key"`
		TagValue string `json:"tag_value"`
	}
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		out[t.TagKey] = t.TagValue
	}
	return out, nil
}

// ListWorkUnits calls wms_listWorkUnits for one outcome.
func (c *hubClient) ListWorkUnits(ctx context.Context, outcomeID string) ([]WorkUnit, error) {
	raw, err := c.WMSCall(ctx, "wms_listWorkUnits", map[string]interface{}{"outcomeID": outcomeID})
	if err != nil {
		return nil, err
	}
	var units []WorkUnit
	if err := json.Unmarshal(raw, &units); err != nil {
		return nil, err
	}
	return units, nil
}

// activityEventMsg carries one parsed SSE activity record.
type activityEventMsg render.Record

// sseStateMsg reports an SSE connect/disconnect transition.
type sseStateMsg struct{ connected bool }

// Subscribe connects to /health/stream?format=json and delivers parsed
// records + connection-state changes on the returned channel. It owns
// reconnection: on error/EOF it emits sseStateMsg{false}, backs off (1s
// doubling to a 30s cap), reconnects with history=0 (no duplicate backfill),
// and emits sseStateMsg{true} on the next successful connect. Runs until ctx
// is canceled.
func (c *hubClient) Subscribe(ctx context.Context, history int) <-chan tea.Msg {
	out := make(chan tea.Msg, 64)
	go func() {
		defer close(out)
		backoff := time.Second
		first := true
		for {
			if ctx.Err() != nil {
				return
			}
			h := 0
			if first {
				h = history
			}
			err := c.streamOnce(ctx, h, out)
			if ctx.Err() != nil {
				return
			}
			first = false
			select {
			case out <- sseStateMsg{connected: false}:
			case <-ctx.Done():
				return
			}
			_ = err // reconnect regardless of error shape
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}()
	return out
}

// streamOnce holds a single SSE connection open, emitting activityEventMsg
// for each parsed frame. Returns when the connection drops or ctx is done.
func (c *hubClient) streamOnce(ctx context.Context, history int, out chan<- tea.Msg) error {
	path := fmt.Sprintf("%s/health/stream?format=json&history=%d", c.base, history)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	client := &http.Client{} // no timeout — long-lived stream
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &apiError{status: resp.StatusCode}
	}

	select {
	case out <- sseStateMsg{connected: true}:
	case <-ctx.Done():
		return ctx.Err()
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var rec render.Record
		if err := json.Unmarshal([]byte(payload), &rec); err != nil {
			continue // skip unparseable frames silently, same tolerance as feed
		}
		select {
		case out <- activityEventMsg(rec):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return scanner.Err()
}

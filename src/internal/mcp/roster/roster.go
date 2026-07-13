// Package roster implements the roster MCP tool handlers. It is
// transport-agnostic: no imports from internal/server or net/http.
package roster

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	rosterutil "github.com/bmjdotnet/teamster/internal/roster"
	"github.com/bmjdotnet/teamster/internal/store"
)

// callParams holds the parsed tools/call params.
type callParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
	Meta      meta                   `json:"_meta"`
}

// meta carries request identity from params._meta. Mirrors internal/mcp/wms's
// Meta shape; duplicated rather than imported to keep this package
// transport-agnostic and decoupled from the wms package (same rationale as
// the duplicated CallError/Result types below).
type meta struct {
	SessionID string         `json:"session_id"`
	AgentType string         `json:"agent_type"`
	CodexTurn *codexTurnMeta `json:"x-codex-turn-metadata"`
}

// codexTurnMeta is the subset of Codex's per-turn MCP metadata this package
// uses. See internal/mcp/wms.CodexTurnMeta for the full rationale.
type codexTurnMeta struct {
	SessionID string `json:"session_id"`
}

// resolveSessionID fills in m.SessionID, in priority order: Codex's native
// turn metadata, an explicit _meta.session_id, or (for anything that isn't
// positively Codex) the ~/.claude/current-session-id fallback file written by
// the hook client — see internal/mcp/wms.resolveSessionID for the full
// rationale; this mirrors it without the ConnectionClientName plumbing, since
// roster-mcp/health-mcp don't track clientInfo (equivalent to it always being
// "", which is fallback-eligible there too).
func resolveSessionID(m *meta) {
	if m.CodexTurn != nil && m.CodexTurn.SessionID != "" {
		m.SessionID = m.CodexTurn.SessionID
		return
	}
	if m.SessionID != "" {
		return
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TEAMSTER_RUNTIME")), "codex") {
		m.SessionID = "unknown-codex"
		return
	}
	m.SessionID = readCurrentSessionID()
}

func readCurrentSessionID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "current-session-id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// resolveCallerTeam resolves the calling agent's team_name via its roster
// entry (the only field with a live write path — registerPeer). Best-effort:
// any failure (unresolvable session, agent not yet registered) yields "",
// which callers treat as "no team" and fall back to session-scoping.
func resolveCallerTeam(ctx context.Context, s store.Store, sessionID, agentName string) string {
	if sessionID == "" {
		return ""
	}
	rosterID, err := s.ResolveRosterID(ctx, sessionID, agentName)
	if err != nil {
		return ""
	}
	entry, err := s.GetRosterEntry(ctx, rosterID)
	if err != nil {
		return ""
	}
	return entry.TeamName
}

// scopeRosterEntries restricts entries to what the caller is allowed to see:
// same team_name if the caller belongs to one, else just the caller's own
// session (solo mode / before /teamster:bootstrap). If the caller's identity
// could not be resolved at all (no session_id — e.g. a remote HTTP caller
// with no hookd identity injection wired for this endpoint), entries pass
// through unscoped rather than going dark for a caller we can't identify.
func scopeRosterEntries(entries []store.RosterEntry, callerSessionID, callerTeam string) []store.RosterEntry {
	if callerTeam != "" {
		var out []store.RosterEntry
		for _, e := range entries {
			if e.TeamName == callerTeam {
				out = append(out, e)
			}
		}
		return out
	}
	if callerSessionID != "" {
		var out []store.RosterEntry
		for _, e := range entries {
			if e.SessionID != nil && *e.SessionID == callerSessionID {
				out = append(out, e)
			}
		}
		return out
	}
	return entries
}

// CallError represents a JSON-RPC error for a tools/call.
type CallError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
}

func (e *CallError) Error() string { return e.Message }

// Result is the MCP tools/call success result.
type Result struct {
	Content []map[string]interface{} `json:"content"`
}

// TextResult wraps a text string in the MCP content envelope.
func TextResult(text string) Result {
	return Result{Content: []map[string]interface{}{{"type": "text", "text": text}}}
}

// JSONResult wraps a marshaled value in the MCP content envelope.
func JSONResult(v interface{}) Result {
	data, _ := json.Marshal(v)
	return TextResult(string(data))
}

func validationErr(msg string) *CallError {
	return &CallError{Code: -32602, Message: msg, Reason: "VALIDATION_ERROR"}
}

func notFoundErr(msg string) *CallError {
	return &CallError{Code: -32000, Message: msg, Reason: "NOT_FOUND"}
}

func conflictErr(msg string) *CallError {
	return &CallError{Code: -32000, Message: msg, Reason: "CONFLICT"}
}

func internalErr(msg string) *CallError {
	return &CallError{Code: -32000, Message: msg}
}

func classifyStoreErr(err error) *CallError {
	if errors.Is(err, store.ErrNotFound) {
		return notFoundErr(err.Error())
	}
	if errors.Is(err, store.ErrConflict) {
		return conflictErr(err.Error())
	}
	return internalErr(err.Error())
}

// HandleToolCall dispatches a tools/call request to the appropriate handler.
func HandleToolCall(s store.Store, rawParams json.RawMessage) (Result, *CallError) {
	var p callParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return Result{}, validationErr("invalid params")
	}

	strArg := func(key string) string {
		v, _ := p.Arguments[key].(string)
		return strings.TrimSpace(v)
	}

	ctx := context.Background()

	resolveSessionID(&p.Meta)
	callerTeam := resolveCallerTeam(ctx, s, p.Meta.SessionID, p.Meta.AgentType)

	switch p.Name {
	case "roster_listAgents":
		return handleListAgents(ctx, s, p.Arguments, p.Meta.SessionID, callerTeam)
	case "roster_getAgent":
		return handleGetAgent(ctx, s, strArg("roster_id"))
	case "roster_resolveId":
		return handleResolveID(ctx, s, strArg("session_id"), strArg("agent_name"))
	case "registerPeer":
		return handleRegisterPeer(ctx, s, p.Arguments, strArg)
	case "verifyToken":
		return handleVerifyToken(ctx, s, strArg("token"))
	case "roster_bindSession":
		return handleBindSession(ctx, s, strArg("roster_id"), strArg("session_id"))
	case "getRosterEntry":
		return handleGetRosterEntry(ctx, s, p.Arguments)
	default:
		return Result{}, &CallError{Code: -32601, Message: "unknown tool: " + p.Name}
	}
}

// --- agentView is the response shape shared by list/get/getEntry ---

type agentView struct {
	RosterID     string  `json:"roster_id"`
	AgentName    string  `json:"agent_name"`
	SessionID    *string `json:"session_id"`
	Host         string  `json:"host"`
	Runtime      string  `json:"runtime"`
	Model        string  `json:"model"`
	TeamName     string  `json:"team_name"`
	BusTeam      string  `json:"bus_team"`
	Relationship string  `json:"relationship"`
	ParentRef    *string `json:"parent_ref"`
	Liveness     string  `json:"liveness"`
	LastSeen     *string `json:"last_seen,omitempty"`
	RevokedAt    *string `json:"revoked_at,omitempty"`
	CurrentFocus string  `json:"current_focus,omitempty"`
}

func buildView(entry store.RosterEntry, session *store.Session, tok *store.AgentToken) agentView {
	v := agentView{
		RosterID:     entry.RosterID,
		AgentName:    entry.AgentName,
		SessionID:    entry.SessionID,
		Host:         entry.Host,
		Runtime:      entry.Runtime,
		Model:        entry.Model,
		TeamName:     entry.TeamName,
		BusTeam:      entry.BusTeam,
		Relationship: entry.Relationship,
		ParentRef:    entry.ParentRef,
		Liveness:     ComputeLiveness(entry, session),
	}
	if session != nil {
		ls := session.LastSeen.UTC().Format("2006-01-02T15:04:05Z")
		v.LastSeen = &ls
		if session.Focus != "" {
			v.CurrentFocus = session.Focus
		}
	}
	if tok != nil && tok.RevokedAt != nil {
		ra := tok.RevokedAt.UTC().Format("2006-01-02T15:04:05Z")
		v.RevokedAt = &ra
	}
	return v
}

// enrichEntry fetches the session and token for a roster entry.
func enrichEntry(ctx context.Context, s store.Store, entry store.RosterEntry) (agentView, error) {
	var session *store.Session
	if entry.SessionID != nil {
		sess, err := s.GetSession(ctx, store.SessionKey{
			SessionID: *entry.SessionID,
			AgentName: entry.AgentName,
		})
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return agentView{}, err
		}
		if err == nil {
			session = &sess
		}
	}

	var tok *store.AgentToken
	hashHex := rosterutil.HashToken("") // won't match, but we need to query by roster_id
	// Use VerifyToken with the hash — but we need to look up by roster_id, not hash.
	// Instead, try the store's VerifyToken and catch not-found. We don't have the raw
	// token here. We query by fetching via the roster_id join.
	// For revoked_at, we need to check the token table. Let's look up via store.
	stTok, _, err := lookupTokenByRosterID(ctx, s, entry.RosterID)
	_ = hashHex
	if err == nil {
		tok = &stTok
	}

	return buildView(entry, session, tok), nil
}

// lookupTokenByRosterID finds the token record for a roster entry by querying
// agent_tokens. Since the store interface only has VerifyToken(hash), we need
// to get the token info another way. For the view layer, we query the roster
// entry's token via a list on the roster_id. Since RosterStore doesn't expose
// a "get token by roster_id" method, we check revocation status via the
// RevokeToken path's awareness — for now, we return nil token if we can't look
// it up, which means revoked_at won't show. This is acceptable for v1 since
// the primary consumer (P1's teardown sweep) will call verifyToken directly.
//
// TODO: Add GetTokenByRosterID to RosterStore interface for clean roster views.
func lookupTokenByRosterID(_ context.Context, _ store.Store, _ string) (store.AgentToken, store.RosterEntry, error) {
	return store.AgentToken{}, store.RosterEntry{}, store.NotFound("lookupTokenByRosterID", "token", "")
}

// --- tool handlers ---

func handleListAgents(ctx context.Context, s store.Store, args map[string]interface{}, callerSessionID, callerTeam string) (Result, *CallError) {
	filter := store.RosterFilter{}
	if v, ok := args["host"].(string); ok {
		filter.Host = strings.TrimSpace(v)
	}
	if v, ok := args["bus_team"].(string); ok {
		filter.BusTeam = strings.TrimSpace(v)
	}
	if v, ok := args["runtime"].(string); ok {
		filter.Runtime = strings.TrimSpace(v)
	}
	if v, ok := args["relationship"].(string); ok {
		filter.Relationship = strings.TrimSpace(v)
	}

	// Parse liveness filter — accepts string or []string.
	livenessFilter := parseLivenessFilter(args)

	entries, err := s.ListRosterEntries(ctx, filter)
	if err != nil {
		return Result{}, classifyStoreErr(err)
	}
	entries = scopeRosterEntries(entries, callerSessionID, callerTeam)

	var views []agentView
	for _, entry := range entries {
		var session *store.Session
		if entry.SessionID != nil {
			sess, err := s.GetSession(ctx, store.SessionKey{
				SessionID: *entry.SessionID,
				AgentName: entry.AgentName,
			})
			if err == nil {
				session = &sess
			}
		}

		liveness := ComputeLiveness(entry, session)

		// Apply liveness filter.
		if len(livenessFilter) > 0 {
			if !livenessFilter[liveness] {
				continue
			}
		} else if !DefaultLivenessSet[liveness] {
			continue
		}

		views = append(views, buildView(entry, session, nil))
	}

	return JSONResult(views), nil
}

func handleGetAgent(ctx context.Context, s store.Store, rosterID string) (Result, *CallError) {
	if rosterID == "" {
		return Result{}, validationErr("roster_id is required")
	}

	entry, err := s.GetRosterEntry(ctx, rosterID)
	if err != nil {
		return Result{}, classifyStoreErr(err)
	}

	view, err := enrichEntry(ctx, s, entry)
	if err != nil {
		return Result{}, internalErr(err.Error())
	}
	return JSONResult(view), nil
}

func handleResolveID(ctx context.Context, s store.Store, sessionID, agentName string) (Result, *CallError) {
	if sessionID == "" {
		return Result{}, validationErr("session_id is required")
	}

	rosterID, err := s.ResolveRosterID(ctx, sessionID, agentName)
	if err != nil {
		return Result{}, classifyStoreErr(err)
	}

	return JSONResult(map[string]string{"roster_id": rosterID}), nil
}

func handleRegisterPeer(ctx context.Context, s store.Store, args map[string]interface{}, strArg func(string) string) (Result, *CallError) {
	agentName := strArg("agent_name")
	runtime := strArg("runtime")
	relationship := strArg("relationship")

	// agent_name is normally required, but "" is the canonical identity for
	// the team lead (mirrors roster_resolveId's "empty string for lead"
	// convention) — only reject it for non-lead relationships.
	if agentName == "" && relationship != "lead" {
		return Result{}, validationErr("agent_name is required")
	}
	if runtime == "" {
		return Result{}, validationErr("runtime is required")
	}
	if relationship == "" {
		return Result{}, validationErr("relationship is required")
	}

	parentRef := strArg("parent_ref")
	if parentRef != "" {
		_, err := s.GetRosterEntry(ctx, parentRef)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return Result{}, &CallError{Code: -32000, Message: "parent_ref not found: " + parentRef, Reason: "PARENT_NOT_FOUND"}
			}
			return Result{}, internalErr(err.Error())
		}
	}

	sessionID := strArg("session_id")
	teamName := strArg("team_name")
	busTeam := strArg("bus_team")
	model := strArg("model")

	// Dedup: hookd auto-registers a roster entry for (session_id, agent_name)
	// on the first hook event, before /teamster:bootstrap ever calls
	// registerPeer. Without this check, bootstrap's call would create a
	// second roster entry for the same agent instead of naming the team on
	// the one that already exists.
	if sessionID != "" {
		if existingID, err := s.ResolveRosterID(ctx, sessionID, agentName); err == nil {
			entry, err := s.GetRosterEntry(ctx, existingID)
			if err != nil {
				return Result{}, classifyStoreErr(err)
			}
			if teamName != "" {
				entry.TeamName = teamName
			}
			if busTeam != "" {
				entry.BusTeam = busTeam
			}
			if model != "" {
				entry.Model = model
			}
			entry.Relationship = relationship
			if err := s.UpsertRosterEntry(ctx, entry); err != nil {
				return Result{}, classifyStoreErr(err)
			}

			bearerToken, err := rosterutil.MintToken(ctx, s, existingID)
			if err != nil {
				return Result{}, internalErr(err.Error())
			}

			if teamName != "" {
				propagateSessionTeam(ctx, s, sessionID, teamName, existingID)
			}

			return JSONResult(map[string]string{
				"roster_id":    existingID,
				"bearer_token": bearerToken,
			}), nil
		}
	}

	opts := rosterutil.RegisterOpts{
		SessionID:    sessionID,
		AgentName:    agentName,
		Host:         strArg("host"),
		Runtime:      runtime,
		Model:        model,
		Relationship: relationship,
		TeamName:     teamName,
		BusTeam:      busTeam,
	}
	if parentRef != "" {
		opts.ParentRef = &parentRef
	}

	rosterID, bearerToken, err := rosterutil.RegisterPeer(ctx, s, opts)
	if err != nil {
		return Result{}, classifyStoreErr(err)
	}

	if opts.TeamName != "" && opts.SessionID != "" {
		propagateSessionTeam(ctx, s, opts.SessionID, opts.TeamName, rosterID)
	}

	return JSONResult(map[string]string{
		"roster_id":    rosterID,
		"bearer_token": bearerToken,
	}), nil
}

// propagateSessionTeam mirrors a newly-set team_name onto the session row
// (sessions.team_name is unscoped by agent_name, so this covers every
// agent_name sharing sessionID in one write) and onto any sibling
// agent_roster entries for the same session_id that were created before this
// team assignment (e.g. auto-registered teammates that joined before
// /teamster:bootstrap named the team). Best-effort: a failed lookup or update
// here must not fail the registerPeer call that already succeeded.
func propagateSessionTeam(ctx context.Context, s store.Store, sessionID, teamName, newRosterID string) {
	_ = s.SetSessionTeam(ctx, sessionID, teamName)

	entries, err := s.ListRosterEntries(ctx, store.RosterFilter{})
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.RosterID == newRosterID || e.TeamName == teamName {
			continue
		}
		if e.SessionID == nil || *e.SessionID != sessionID {
			continue
		}
		e.TeamName = teamName
		_ = s.UpsertRosterEntry(ctx, e)
	}
}

func handleVerifyToken(ctx context.Context, s store.Store, token string) (Result, *CallError) {
	if token == "" {
		return Result{}, validationErr("token is required")
	}

	tok, entry, err := rosterutil.VerifyToken(ctx, s, token)
	if err != nil {
		return JSONResult(map[string]interface{}{"valid": false}), nil
	}
	_ = tok

	resp := map[string]interface{}{
		"valid":        true,
		"roster_id":    entry.RosterID,
		"session_id":   entry.SessionID,
		"agent_name":   entry.AgentName,
		"team_name":    entry.TeamName,
		"bus_team":     entry.BusTeam,
		"relationship": entry.Relationship,
	}
	return JSONResult(resp), nil
}

func handleBindSession(ctx context.Context, s store.Store, rosterID, sessionID string) (Result, *CallError) {
	if rosterID == "" {
		return Result{}, validationErr("roster_id is required")
	}
	if sessionID == "" {
		return Result{}, validationErr("session_id is required")
	}

	err := s.BindRosterSession(ctx, rosterID, sessionID)
	if err != nil {
		return Result{}, classifyStoreErr(err)
	}

	return JSONResult(map[string]interface{}{
		"roster_id": rosterID,
		"bound":     true,
	}), nil
}

func handleGetRosterEntry(ctx context.Context, s store.Store, args map[string]interface{}) (Result, *CallError) {
	rosterID, _ := args["roster_id"].(string)
	busTeam, _ := args["bus_team"].(string)
	rosterID = strings.TrimSpace(rosterID)
	busTeam = strings.TrimSpace(busTeam)

	if rosterID == "" && busTeam == "" {
		return Result{}, validationErr("roster_id or bus_team is required")
	}

	if rosterID != "" {
		entry, err := s.GetRosterEntry(ctx, rosterID)
		if err != nil {
			return Result{}, classifyStoreErr(err)
		}
		view, err := enrichEntry(ctx, s, entry)
		if err != nil {
			return Result{}, internalErr(err.Error())
		}
		return JSONResult(view), nil
	}

	// bus_team query — returns array with default liveness scope.
	livenessFilter := parseLivenessFilter(args)

	entries, err := s.ListRosterEntries(ctx, store.RosterFilter{BusTeam: busTeam})
	if err != nil {
		return Result{}, classifyStoreErr(err)
	}

	var views []agentView
	for _, entry := range entries {
		var session *store.Session
		if entry.SessionID != nil {
			sess, err := s.GetSession(ctx, store.SessionKey{
				SessionID: *entry.SessionID,
				AgentName: entry.AgentName,
			})
			if err == nil {
				session = &sess
			}
		}

		liveness := ComputeLiveness(entry, session)

		if len(livenessFilter) > 0 {
			if !livenessFilter[liveness] {
				continue
			}
		} else if !DefaultLivenessSet[liveness] {
			continue
		}

		views = append(views, buildView(entry, session, nil))
	}

	return JSONResult(views), nil
}

// --- helpers ---

func parseLivenessFilter(args map[string]interface{}) map[string]bool {
	raw, ok := args["liveness"]
	if !ok {
		return nil
	}

	result := make(map[string]bool)

	switch v := raw.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v != "" {
			result[v] = true
		}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					result[s] = true
				}
			}
		}
	}

	return result
}

// ToolDefs is the MCP tools/list payload for the roster server.
var ToolDefs = []map[string]interface{}{
	{
		"name":        "roster_listAgents",
		"description": "List registered agents with optional filters. Default scope: live/idle/unbound (excludes closed).",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"host":         map[string]interface{}{"type": "string", "description": "Filter by host"},
				"bus_team":     map[string]interface{}{"type": "string", "description": "Filter by bus team"},
				"runtime":      map[string]interface{}{"type": "string", "description": "Filter by runtime (claude_code, codex)"},
				"relationship": map[string]interface{}{"type": "string", "description": "Filter by relationship (lead, teammate, subagent, peer, service)"},
				"liveness": map[string]interface{}{
					"description": "Filter by liveness tier(s). String or array of strings. Values: live, idle, stale, closed, unbound.",
					"oneOf": []map[string]interface{}{
						{"type": "string"},
						{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
			},
		},
	},
	{
		"name":        "roster_getAgent",
		"description": "Get detailed info for a single agent by roster_id, including token metadata (never the raw token).",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"roster_id": map[string]interface{}{"type": "string"},
			},
			"required": []string{"roster_id"},
		},
	},
	{
		"name":        "roster_resolveId",
		"description": "Resolve a (session_id, agent_name) pair to a roster_id.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"session_id": map[string]interface{}{"type": "string"},
				"agent_name": map[string]interface{}{"type": "string", "description": "Empty string for lead"},
			},
			"required": []string{"session_id"},
		},
	},
	{
		"name":        "registerPeer",
		"description": "Register a new agent and mint a bearer token. session_id is optional (omit for spawn-time unbound registration).",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent_name":   map[string]interface{}{"type": "string"},
				"runtime":      map[string]interface{}{"type": "string", "description": "claude_code or codex"},
				"model":        map[string]interface{}{"type": "string"},
				"host":         map[string]interface{}{"type": "string"},
				"relationship": map[string]interface{}{"type": "string", "description": "lead, teammate, subagent, peer, or service"},
				"parent_ref":   map[string]interface{}{"type": "string", "description": "roster_id of the spawning parent"},
				"bus_team":     map[string]interface{}{"type": "string"},
				"team_name":    map[string]interface{}{"type": "string"},
				"session_id":   map[string]interface{}{"type": "string", "description": "Omit for unbound spawn-time registration"},
			},
			"required": []string{"agent_name", "runtime", "relationship"},
		},
	},
	{
		"name":        "verifyToken",
		"description": "Verify a raw bearer token. Returns valid:true with identity fields, or valid:false. session_id is null for unbound tokens.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"token": map[string]interface{}{"type": "string"},
			},
			"required": []string{"token"},
		},
	},
	{
		"name":        "roster_bindSession",
		"description": "Bind a session_id to a pending (unbound) roster entry. Idempotent for same session_id; rejects different session_id.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"roster_id":  map[string]interface{}{"type": "string"},
				"session_id": map[string]interface{}{"type": "string"},
			},
			"required": []string{"roster_id", "session_id"},
		},
	},
	{
		"name":        "getRosterEntry",
		"description": "Get roster entry by roster_id (single) or bus_team (array). Same default scope as roster_listAgents.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"roster_id": map[string]interface{}{"type": "string"},
				"bus_team":  map[string]interface{}{"type": "string"},
				"liveness": map[string]interface{}{
					"description": "Liveness filter for bus_team queries. String or array.",
					"oneOf": []map[string]interface{}{
						{"type": "string"},
						{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
				},
			},
		},
	},
}

// MCPTool* constants for tool name matching (used by server.go's hook stash if needed).
const (
	MCPToolListAgents    = "roster_listAgents"
	MCPToolGetAgent      = "roster_getAgent"
	MCPToolResolveID     = "roster_resolveId"
	MCPToolRegisterPeer  = "registerPeer"
	MCPToolVerifyToken   = "verifyToken"
	MCPToolBindSession   = "roster_bindSession"
	MCPToolGetRosterEntry = "getRosterEntry"
)

// Reason constants for error shapes (P0 §3.1a).
const (
	ReasonValidationError  = "VALIDATION_ERROR"
	ReasonParentNotFound   = "PARENT_NOT_FOUND"
	ReasonIdentityConflict = "IDENTITY_CONFLICT"
	ReasonUnauthorized     = "UNAUTHORIZED"
	ReasonConflict         = "CONFLICT"
	ReasonNotFound         = "NOT_FOUND"
)

// ErrorWithReason returns a CallError with a structured reason field for
// JSON-RPC error responses.
func ErrorWithReason(code int, message, reason string) *CallError {
	return &CallError{Code: code, Message: message, Reason: reason}
}

// FormatError renders a CallError as a map for JSON-RPC error responses.
// Used by the server to include the reason field in the error object.
func FormatError(e *CallError) map[string]interface{} {
	result := map[string]interface{}{
		"code":    e.Code,
		"message": e.Message,
	}
	if e.Reason != "" {
		result["reason"] = e.Reason
	}
	return result
}

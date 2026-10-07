// Package wms implements the WMS MCP tool handlers. It is transport-agnostic:
// no imports from internal/server or cmd/hookd.
package wms

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	storeTypes "github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// Meta carries request identity from params._meta.
type Meta struct {
	Host      string `json:"host"`
	SessionID string `json:"session_id"`
	AgentType string `json:"agent_type"`
	// CodexTurn carries Codex's native per-call session identity, sent
	// automatically (no prompting, independent of hooks) under the key
	// "x-codex-turn-metadata" on every tools/call. Claude Code never sends
	// this key. See resolveSessionID for how it's used.
	CodexTurn *CodexTurnMeta `json:"x-codex-turn-metadata"`
}

// CodexTurnMeta is the subset of Codex's per-turn MCP metadata wms-mcp uses.
// Codex sends additional fields (sandbox, turn_started_at_unix_ms, and a
// conditional user_input_requested_during_turn present only on turns that hit
// an interactive approval prompt) — this struct parses only what it needs;
// json.Unmarshal ignores unrecognized fields and zero-values missing ones, so
// it tolerates the field set drifting on Codex's side (x-codex-turn-metadata
// is undocumented internal surface, not a stable contract).
type CodexTurnMeta struct {
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id"`
	Model     string `json:"model"`
}

// ConnectionClientName is the MCP clientInfo.name reported at initialize.
// Wired in by cmd/wms-mcp/main.go for the stdio transport, where one process
// serves exactly one connection, so process-level state is connection-level
// state. Confirmed empirically (2026-07-07, isolated echo-probe capture):
// Claude Code's own MCP client sends clientInfo.name == "claude-code"; Codex
// sends "codex-mcp-client" (kit evidence). Left at its zero value "" by
// hookd's HTTP /mcp/wms transport (server.go), which never sets it — that
// transport's identity comes from a different, pre-existing mechanism
// (injectMCPIdentity's hook-payload stash) and is unaffected by this var;
// Codex-over-HTTP is out of v1 scope (hub-local stdio only). An empty value
// here is treated as "no clientInfo received", which fallbackEligible also
// covers, so hookd's path replicates its pre-existing behavior unchanged.
var ConnectionClientName string

const (
	claudeCodeClientName = "claude-code"
	codexClientName      = "codex-mcp-client"
)

// callParams holds the parsed tools/call params.
type callParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
	Meta      Meta                   `json:"_meta"`
}

// CallError represents a JSON-RPC error for a tools/call.
type CallError struct {
	Code    int
	Message string
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

// ActiveClassifier is the classifier wired in by main.go. When nil, the
// wms_classifyEntity tool returns a "not configured" stub response.
var ActiveClassifier wms.Classifier

// CreatorUser is the OS user the wms-mcp process runs as (TEAMSTER_USER else
// os/user.Current() else $USER), wired in by main.go from Config.User. When
// non-empty, every outcome/workunit is auto-tagged user:<CreatorUser> at
// creation so work can be faceted by who created it.
//
// LIMITATION: in the current architecture this is the wms-mcp PROCESS user (the
// hub operator), not necessarily the user of the session that issued the create.
// Correct for the single-user homelab; refine when multi-user remotes land and
// the creating session's user is carried on the MCP call.
var CreatorUser string

// readCurrentSessionID reads ~/.claude/current-session-id written by the hook
// client on every event. Used as a fallback when no other session identity is
// available (Claude Code's own MCP client sends neither _meta.session_id nor
// x-codex-turn-metadata). Only ever consulted when fallbackEligible says the
// calling connection is Claude Code or unidentified-but-legacy — see
// resolveSessionID.
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

// fallbackEligible reports whether this connection may fall back to the
// shared ~/.claude/current-session-id file when no explicit session id is
// present. Eligible: no clientInfo was ever sent at initialize (older/minimal
// clients — back-compat so existing Claude Code installs are unchanged), or
// clientInfo.name is Claude Code's own. Never eligible when TEAMSTER_RUNTIME=
// codex (installer-set env, belt-and-suspenders — wins even if clientInfo
// itself drifts or vanishes in a future Codex release, satisfying the
// fail-safe requirement that a Codex connection never falls through to a live
// Claude Code session id).
func fallbackEligible() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TEAMSTER_RUNTIME")), "codex") {
		return false
	}
	return ConnectionClientName == "" || ConnectionClientName == claudeCodeClientName
}

// runtimeTag classifies the calling connection into one of three buckets for
// tagging freshly created entities: "codex", "claude_code", or "unknown" (a
// third MCP client we can't positively identify as either — inspector tools,
// another agent CLI, etc.). Independent of whether Codex's turn metadata was
// present on this particular call; a Codex connection that dropped turn
// metadata for one call is still "codex" here even though resolveSessionID
// buckets its session id as unknown-codex rather than trusting a stolen file.
func runtimeTag() string {
	switch {
	case strings.EqualFold(strings.TrimSpace(os.Getenv("TEAMSTER_RUNTIME")), "codex"):
		return "codex"
	case ConnectionClientName == codexClientName:
		return "codex"
	case ConnectionClientName == claudeCodeClientName, ConnectionClientName == "":
		return "claude_code"
	default:
		return "unknown"
	}
}

// resolveSessionID fills in m.SessionID, in priority order:
//  1. Codex's native x-codex-turn-metadata.session_id (PRIMARY — sent on
//     every tools/call, independent of hooks, most specific signal available).
//  2. An explicit generic _meta.session_id, if some future client populates
//     it directly — trusted as-is, unaffected by this change (no client does
//     this today; Claude Code and Codex both leave it empty).
//  3. The ~/.claude/current-session-id file, but ONLY when fallbackEligible
//     (Claude Code, or a connection indistinguishable from it).
//  4. Otherwise, an explicit "unknown-<runtime>" bucket — never the fallback
//     file. This is the generalized refusal: any connection that isn't
//     positively Claude Code lands here rather than silently stealing
//     whatever Claude Code session happens to be active on the host.
func resolveSessionID(m *Meta) {
	if m.CodexTurn != nil && m.CodexTurn.SessionID != "" {
		m.SessionID = m.CodexTurn.SessionID
		return
	}
	if m.SessionID != "" {
		return
	}
	if fallbackEligible() {
		m.SessionID = readCurrentSessionID()
		return
	}
	m.SessionID = "unknown-" + runtimeTag()
}

// applyRuntimeTag auto-applies runtime:<codex|claude_code|unknown> to a
// freshly created entity, mirroring applyCreatorUserTag's pattern:
// best-effort, never blocks the create. "unknown" means the call arrived
// over a connection that could not be positively identified as Claude Code
// or Codex.
func applyRuntimeTag(ctx context.Context, store wms.Store, entityType, entityID, runtime string) {
	if err := store.TagEntity(ctx, entityType, entityID, "runtime", runtime, "classifier", ""); err != nil {
		slog.Warn("wms-mcp: auto runtime-tag failed",
			"entity_type", entityType, "entity_id", entityID, "runtime", runtime, "err", err)
	}
}

// parseInlineTagValue accepts either a plain string (the tag value, no
// description) or an object {"value": "...", "description": "..."} — the
// latter lets an inline tags map carry the same create-only description
// semantics as a direct wms_tagEntity call.
func parseInlineTagValue(v interface{}) (value, description string, ok bool) {
	switch t := v.(type) {
	case string:
		return t, "", t != ""
	case map[string]interface{}:
		value, _ = t["value"].(string)
		description, _ = t["description"].(string)
		return value, description, value != ""
	default:
		return "", "", false
	}
}

// inlineTagPair is one (value, description) element parsed out of a `tags`
// argument entry — either the entry itself (scalar form) or one element of
// its array form.
type inlineTagPair struct{ value, description string }

// parseInlineTagValues accepts a plain string, an object {"value",
// "description"}, or an array mixing either form — the array case is how a
// multi-cardinality key (e.g. github.issue) carries more than one value in
// one inline-tags map, which a single key→value(+description) entry cannot
// express (a JSON object can't repeat a key). badIndex names the array
// position of the first malformed element so the caller's error can point at
// it directly; for the scalar (non-array) form there is no index to report,
// so badIndex is always -1 on failure, matching parseInlineTagValue's own
// error shape.
func parseInlineTagValues(v interface{}) (pairs []inlineTagPair, badIndex int, ok bool) {
	if arr, isArr := v.([]interface{}); isArr {
		if len(arr) == 0 {
			return nil, -1, false
		}
		for i, elem := range arr {
			value, description, elemOK := parseInlineTagValue(elem)
			if !elemOK {
				return nil, i, false
			}
			pairs = append(pairs, inlineTagPair{value, description})
		}
		return pairs, -1, true
	}
	value, description, elemOK := parseInlineTagValue(v)
	if !elemOK {
		return nil, -1, false
	}
	return []inlineTagPair{{value, description}}, -1, true
}

// singleCardinalityKey reports whether tagKey is already declared
// cardinality:single. TagEntity's cardinality guard REPLACES any other value
// of a single-cardinality key on the entity (store.go's TagEntity doc
// comment) — applying an array of more than one value to such a key would
// silently keep only the last one with no error, the exact silent-data-loss
// footgun WP2 exists to close. Best-effort: a lookup failure is treated as
// "not single" (the caller's TagEntity call surfaces any real store problem).
//
// Deliberately uses ListTags, not SearchTags: SearchTags filters `retired =
// 0`, but TagEntity's own cardinality resolution
// (`SELECT cardinality FROM tags WHERE tag_key = ? AND cardinality =
// 'single' LIMIT 1`) carries no such filter — it sees a retired row. A key
// whose only single-cardinality row has since been retired (e.g. via
// `teamster tags delete-value`) would disagree between the two: this guard
// would say "not single" and let an array through, while TagEntity would
// still say "single" underneath it and silently replace value after value —
// reintroducing the exact silent-data-loss bug this guard exists to close,
// caught in adversarial review. ListTags returns every row including
// retired ones, matching TagEntity's actual predicate exactly.
func singleCardinalityKey(ctx context.Context, store wms.Store, tagKey string) bool {
	tags, err := store.ListTags(ctx)
	if err != nil {
		return false
	}
	for _, t := range tags {
		if t.Key == tagKey && t.Cardinality == "single" {
			return true
		}
	}
	return false
}

// applyInlineTags applies the optional `tags` argument (key→value,
// key→{value, description}, or key→array of either form) to a freshly
// created entity, one wms.TagEntity call per value. Best-effort like
// applyCreatorUserTag/applyRuntimeTag: the entity is already created by the
// time this runs, so a tag failure never unwinds it — failures are collected
// and returned for the caller to surface in its response rather than fail
// the whole create call. Requirements (c)/(d) from #17 (per-tag error
// reporting, no whole-batch abort) extend to the array case: one malformed
// key never blocks any other key in the same map.
func applyInlineTags(ctx context.Context, store wms.Store, entityType, entityID string, tagsArg interface{}) []string {
	tagsMap, ok := tagsArg.(map[string]interface{})
	if !ok || len(tagsMap) == 0 {
		return nil
	}
	var errs []string
	for tagKey, raw := range tagsMap {
		pairs, badIndex, ok := parseInlineTagValues(raw)
		if !ok {
			switch {
			case badIndex >= 0:
				errs = append(errs, fmt.Sprintf("tag %q[%d]: value must be a non-empty string or {value, description} object", tagKey, badIndex))
			default:
				if arr, isArr := raw.([]interface{}); isArr && len(arr) == 0 {
					// Distinct from the generic message below: the value WAS
					// an array, it just had nothing in it — naming the
					// actual problem instead of the generic type mismatch
					// (adversarial-review NOTE).
					errs = append(errs, fmt.Sprintf("tag %q: array must not be empty", tagKey))
				} else {
					errs = append(errs, fmt.Sprintf("tag %q: value must be a non-empty string, {value, description} object, or an array of either", tagKey))
				}
			}
			continue
		}
		if len(pairs) > 1 && singleCardinalityKey(ctx, store, tagKey) {
			errs = append(errs, fmt.Sprintf("tag %q: %d values given but %q is cardinality:single — pass one value, not an array", tagKey, len(pairs), tagKey))
			continue
		}
		for _, pair := range pairs {
			if err := store.TagEntity(ctx, entityType, entityID, tagKey, pair.value, "manual", pair.description); err != nil {
				errs = append(errs, fmt.Sprintf("tag %q=%q: %s", tagKey, pair.value, err.Error()))
			}
		}
	}
	return errs
}

// closeoutNudge is the permanent tool-response nudge decided by the operator
// (ANALYSIS.md §5, amendment 1): once WP7 removes the auto-close cascade,
// nothing else tells an agent that finishing the last sibling WorkUnit does
// NOT also close its Outcome. Called from the WorkUnit-terminal branches of
// wms_updateStatus and ToolUpdateWorkUnitStatus, after the transition and
// eng.OnStatusChange both succeed. Silent unless every WorkUnit under the
// Outcome is now terminal and the Outcome itself is still open — it never
// walks up to parent Outcomes and never writes anything.
func closeoutNudge(ctx context.Context, store wms.Store, outcomeID string) string {
	if outcomeID == "" {
		return ""
	}
	units, err := store.ListWorkUnits(ctx, outcomeID)
	if err != nil || len(units) == 0 {
		return ""
	}
	for _, u := range units {
		if u == nil || !wms.IsTerminal(wms.EntityWorkUnit, u.Status) {
			return ""
		}
	}
	outcome, err := store.GetOutcome(ctx, outcomeID)
	if err != nil || wms.IsTerminal(wms.EntityOutcome, outcome.Status) {
		return ""
	}
	return fmt.Sprintf(
		"\n\nall %d work units under outcome %s are now terminal — it does not close automatically; run the close-out consideration (session-protocol Step 9b) and call wms_updateOutcomeStatus when ready.",
		len(units), outcomeID)
}

// relationStore is the typed-relations capability (WP3 stage 2,
// outcome_relations) — not part of wms.Store, so the wms_*Relation* handlers
// reach it via type assertion on the concrete store, same pattern as
// ListRelatedEntities above.
type relationStore interface {
	AddRelation(ctx context.Context, kind, fromType, fromID, toType, toID, createdBy, source, note string) error
	RemoveRelation(ctx context.Context, kind, fromType, fromID, toType, toID string) error
	ListRelations(ctx context.Context, entityType, entityID, direction, kind string) ([]storeTypes.Relation, error)
	ListRelationKinds(ctx context.Context) ([]storeTypes.RelationKind, error)
}

// HandleToolCall dispatches a tools/call request to the appropriate store method.
// meta is captured from params._meta and stored on mutations.
func HandleToolCall(store wms.Store, eng wms.Engine, rawParams json.RawMessage) (Result, *CallError) {
	var p callParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return Result{}, &CallError{Code: -32602, Message: "invalid params"}
	}

	resolveSessionID(&p.Meta)
	runtime := runtimeTag()

	strArg := func(key string) string {
		v, _ := p.Arguments[key].(string)
		return strings.TrimSpace(v)
	}
	strArgDefault := func(key, def string) string {
		v := strArg(key)
		if v == "" {
			return def
		}
		return v
	}

	ctx := context.Background()

	switch p.Name {
	case "wms_updateStatus", "wms.updateStatus":
		entityType := strArg("entityType")
		entityID := strArg("entityID")
		newStatus := strArg("status")

		var oldStatus, outcomeID string
		var err error
		switch entityType {
		case wms.EntityOutcome:
			o, e := store.GetOutcome(ctx, entityID)
			if e != nil {
				return Result{}, &CallError{Code: -32000, Message: e.Error()}
			}
			oldStatus = o.Status
		case wms.EntityWorkUnit:
			wu, e := store.GetWorkUnit(ctx, entityID)
			if e != nil {
				return Result{}, &CallError{Code: -32000, Message: e.Error()}
			}
			oldStatus = wu.Status
			outcomeID = wu.OutcomeID
		default:
			return Result{}, &CallError{Code: -32602, Message: "unknown entityType: " + entityType}
		}

		if !wms.ValidTransition(entityType, oldStatus, newStatus) {
			return Result{}, &CallError{
				Code:    -32000,
				Message: fmt.Sprintf("invalid transition %s: %s → %s", entityType, oldStatus, newStatus),
			}
		}

		role := p.Meta.AgentType
		allowed, err := store.RoleAllowed(ctx, entityType, oldStatus, newStatus, role)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if !allowed {
			return Result{}, &CallError{
				Code:    -32000,
				Message: fmt.Sprintf("role %q not allowed: %s %s→%s", role, entityType, oldStatus, newStatus),
			}
		}

		// Persist the status change before recording the temporal event, so a
		// transient event-record failure never blocks the actual transition.
		// UpdateXStatus is the authoritative write (it also saves prior_status
		// on blocked); the event record is observability only.
		switch entityType {
		case wms.EntityOutcome:
			if err := store.UpdateOutcomeStatus(ctx, entityID, newStatus); err != nil {
				return Result{}, &CallError{Code: -32000, Message: err.Error()}
			}
		case wms.EntityWorkUnit:
			if err := store.UpdateWorkUnitStatus(ctx, entityID, newStatus); err != nil {
				return Result{}, &CallError{Code: -32000, Message: err.Error()}
			}
		}
		if err := store.TransitionEventRecord(ctx, entityType, entityID, newStatus, p.Meta.SessionID, p.Meta.AgentType, p.Meta.Host); err != nil {
			// Status already persisted by UpdateXStatus above; log, don't fail.
			slog.Warn("wms-mcp: transition event record failed",
				"entity_type", entityType, "entity_id", entityID, "status", newStatus, "err", err)
		}
		// WP10 path 1: never leave the journal's notes column blank-by-omission
		// — synthesize an honest default naming the tool when the caller didn't
		// pass one.
		notes := strArg("notes")
		if notes == "" {
			notes = fmt.Sprintf("status change via %s (no notes provided)", p.Name)
		}
		eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
			EntityType: entityType,
			EntityID:   entityID,
			OldStatus:  oldStatus,
			NewStatus:  newStatus,
			SessionID:  p.Meta.SessionID,
			AgentName:  p.Meta.AgentType,
			Host:       p.Meta.Host,
			Notes:      notes,
		})
		msg := fmt.Sprintf("Updated %s %s: %s → %s", entityType, entityID, oldStatus, newStatus)
		if entityType == wms.EntityOutcome {
			msg += wms.FormatCloseoutWarnings(wms.CloseoutWarnings(ctx, store, entityID, newStatus))
		}
		if entityType == wms.EntityWorkUnit && wms.IsTerminal(entityType, newStatus) {
			msg += closeoutNudge(ctx, store, outcomeID)
		}
		return TextResult(msg), nil

	case "wms_addDependency", "wms.addDependency":
		d := &wms.Dependency{
			BlockerID:   strArg("blockerID"),
			BlockedID:   strArg("blockedID"),
			BlockerType: strArg("blockerType"),
			BlockedType: strArg("blockedType"),
		}
		if err := store.AddEntityDependency(ctx, d); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Added dependency: %s → %s", d.BlockerID, d.BlockedID)), nil

	case "wms_removeDependency", "wms.removeDependency":
		blockerType := strArg("blockerType")
		blockerID := strArg("blockerID")
		blockedType := strArg("blockedType")
		blockedID := strArg("blockedID")
		if err := store.RemoveEntityDependency(ctx, blockerType, blockerID, blockedType, blockedID); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Removed dependency: %s → %s", blockerID, blockedID)), nil

	case "wms_listBlockers", "wms.listBlockers":
		deps, err := store.ListEntityDependencyBlockers(ctx, strArg("entityType"), strArg("entityID"))
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(deps), nil

	case "wms_listDependents", "wms.listDependents":
		deps, err := store.ListEntityDependencyDependents(ctx, strArg("entityType"), strArg("entityID"))
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(deps), nil

	case "wms_setFocus", "wms.setFocus":
		entityType := strings.ToLower(strArg("entityType"))
		entityID := strArg("entityID")
		focus := strArg("focus")
		var err error
		switch entityType {
		case wms.EntityOutcome:
			err = store.UpdateOutcomeFocus(ctx, entityID, focus)
		case wms.EntityWorkUnit:
			err = store.UpdateWorkUnitFocus(ctx, entityID, focus)
		default:
			return Result{}, &CallError{Code: -32602, Message: "setFocus not supported for entityType: " + entityType}
		}
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Focus set on %s %s", entityType, entityID)), nil

	case ToolSetPhase, "wms.setPhase":
		// Work-item-level phase declaration (OD-4): the agent declares the phase
		// of a work unit; we land it on the unit's currently-OPEN interval as a
		// 'declared' write (declared wins over classifier). Workunit-only this
		// increment (BP-1); outcomes revisit at B3.
		entityType := strArg("entityType")
		entityID := strArg("entityID")
		phase := strArg("phase")
		if entityType != wms.EntityWorkUnit {
			return Result{}, &CallError{Code: -32602, Message: "setPhase supports entityType=workunit only"}
		}
		if phase == "" {
			return Result{}, &CallError{Code: -32602, Message: "phase is required"}
		}
		switch phase {
		case "design", "build", "test", "review", "iterate", "admin":
		default:
			return Result{}, &CallError{Code: -32602, Message: fmt.Sprintf("invalid phase %q: must be one of design, build, test, review, iterate, admin", phase)}
		}
		// Gate on the entity's own status (LF-skills-2/LF-VER-5), not on whether a
		// state interval happens to be open: hookd's per-turn Stop drain (or any
		// sweep) can close a still-active workunit's interval, which the old gate
		// misdiagnosed as "not active" — false for an active unit, and it returned
		// a silent TextResult a caller not reading the body would miss entirely.
		//
		// active AND review both count as in-progress: wms_deliverResult moves a
		// unit active -> review and it stays there through the whole
		// VALIDATE/ADVERSARIAL-REVIEW/send-back loop (BRIEF-COMMON hard rule 7;
		// execution-loop.md maps that loop to phase=test/review/iterate) — a
		// review-status unit is exactly when phase declaration is most useful, not
		// a case to reject. Narrowing this to active-only was an adversarial-
		// review-caught overreach: the fix advice it would have given, transition
		// back to active, corrupts the very status the review loop depends on.
		wu, err := store.GetWorkUnit(ctx, entityID)
		if err != nil {
			if errors.Is(err, storeTypes.ErrNotFound) {
				return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("workunit %s not found", entityID)}
			}
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if wu.Status != wms.StatusActive && wu.Status != wms.StatusReview {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("workunit %s is not active or in review (status=%s); phase can only be declared while work is in progress", entityID, wu.Status)}
		}
		rec, err := store.GetOpenEventRecord(ctx, wms.EntityWorkUnit, entityID)
		if err != nil && !storeTypes.IsNotFound(err) {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if rec == nil {
			// The workunit IS active or in review but carries no open state
			// interval — a sweep or drain closed it without a status change
			// reopening one. This is a real failure to record the phase, not a
			// caller mistake, so it errors instead of silently no-op'ing with
			// advice ("transition it active first") that would be false here.
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("workunit %s is %s but has no open state interval to record the phase against — an interval-draining sweep may have closed it; retry, or report if this persists", entityID, wu.Status)}
		}
		if err := store.UpdateEventRecordPhase(ctx, rec.ID, phase, "declared"); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Declared phase %q on workunit %s (interval %d)", phase, entityID, rec.ID)), nil

	case "wms_getFocus", "wms.getFocus":
		entityType := strArg("entityType")
		entityID := strArg("entityID")
		var focus string
		switch entityType {
		case wms.EntityOutcome:
			o, err := store.GetOutcome(ctx, entityID)
			if err != nil {
				return Result{}, &CallError{Code: -32000, Message: err.Error()}
			}
			focus = o.Focus
		case wms.EntityWorkUnit:
			wu, err := store.GetWorkUnit(ctx, entityID)
			if err != nil {
				return Result{}, &CallError{Code: -32000, Message: err.Error()}
			}
			focus = wu.Focus
		default:
			return Result{}, &CallError{Code: -32602, Message: "getFocus not supported for entityType: " + entityType}
		}
		return TextResult(focus), nil

	case ToolTagEntity, "wms.tagEntity":
		entityType := strArg("entityType")
		entityID := strArg("entityID")
		tagKey := strArg("tagKey")
		tagValue := strArg("tagValue")
		source := strArgDefault("source", "manual")
		description := strArg("description")
		if err := store.TagEntity(ctx, entityType, entityID, tagKey, tagValue, source, description); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Tagged %s %s: %s=%s", entityType, entityID, tagKey, tagValue)), nil

	case ToolListTags, "wms.listTags":
		tagKey := strArg("tagKey")
		query := strArg("query")

		if tagKey == "" && query == "" {
			tags, err := store.ListTags(ctx)
			if err != nil {
				return Result{}, &CallError{Code: -32000, Message: err.Error()}
			}
			manifest := buildTagManifest(tags)
			return JSONResult(splitRitualManaged(manifest)), nil
		}

		tags, err := store.SearchTags(ctx, tagKey, query)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(tags), nil

	case ToolDefineTag, "wms.defineTag":
		spec := wms.TagSpec{
			Key:         strArg("tagKey"),
			Category:    strArg("category"),
			Cardinality: strArg("cardinality"),
			Description: strArg("description"),
		}
		if vals, ok := p.Arguments["values"].([]interface{}); ok {
			for _, v := range vals {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					spec.Values = append(spec.Values, strings.TrimSpace(s))
				}
			}
		}
		if req, ok := p.Arguments["required"].(bool); ok {
			spec.Required = &req
		}
		if v := strArg("scope"); v != "" {
			spec.Scope = &v
		}
		if v := strArg("exclusionGroup"); v != "" {
			spec.ExclusionGroup = &v
		}
		if v := strArg("autoExtract"); v != "" {
			spec.AutoExtract = &v
		}
		if v := strArg("interview"); v != "" {
			spec.Interview = &v
		}
		if v := strArg("facetSource"); v != "" {
			spec.FacetSource = &v
		}
		if err := store.DefineTag(ctx, spec); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Defined tag %q (category=%s, cardinality=%s)", spec.Key, spec.Category, spec.Cardinality)), nil

	case ToolRetireTag, "wms.retireTag":
		tagKey := strArg("tagKey")
		if err := store.RetireTag(ctx, tagKey); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Retired tag %q (demoted: is_seed=0; bindings kept)", tagKey)), nil

	case ToolDescribeTag, "wms.describeTag":
		tagKey := strArg("tagKey")
		tagValue := strArg("tagValue")
		description := strArg("description")
		if tagKey == "" || tagValue == "" || description == "" {
			return Result{}, &CallError{Code: -32602, Message: "tagKey, tagValue, and description are required"}
		}
		// Overwrites the description of an EXISTING (tagKey, tagValue) in place —
		// including system-managed lifecycle keys (work-type/phase/resolution),
		// which DefineTag refuses and TagEntity's create-only description write
		// can't touch. The store surfaces a not-found error when the value does
		// not exist; pass it through so the steward can correct the call.
		if err := store.UpdateTagValueDescription(ctx, tagKey, tagValue, description); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Updated description for %s:%s", tagKey, tagValue)), nil

	case ToolUntagEntity, "wms.untagEntity":
		entityType := strArg("entityType")
		entityID := strArg("entityID")
		tagKey := strArg("tagKey")
		tagValue := strArg("tagValue") // optional: empty = remove all bindings for the key
		if entityType == "" || entityID == "" || tagKey == "" {
			return Result{}, &CallError{Code: -32602, Message: "entityType, entityID, and tagKey are required"}
		}
		path, removed, err := untagEntity(ctx, store, entityType, entityID, tagKey, tagValue)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if removed == 0 {
			// Nothing matched — idempotent no-op, no snapshot written.
			return JSONResult(map[string]interface{}{"removed": 0, "snapshot": ""}), nil
		}
		return JSONResult(map[string]interface{}{"removed": removed, "snapshot": path}), nil

	case ToolGetEntityTags:
		entityType := strArg("entityType")
		entityID := strArg("entityID")
		if entityType != wms.EntityOutcome && entityType != wms.EntityWorkUnit {
			return Result{}, &CallError{Code: -32602, Message: "getEntityTags supports entityType=outcome|workunit only"}
		}
		if entityID == "" {
			return Result{}, &CallError{Code: -32602, Message: "entityID is required"}
		}
		tags, err := resolveEntityTags(ctx, store, entityType, entityID)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(tags), nil

	case ToolGetHistory:
		limit := 50
		if v, ok := p.Arguments["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		entries, err := store.GetJournalEntries(ctx, strArg("entityType"), strArg("entityID"), limit)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if entries == nil {
			entries = []wms.JournalEntry{}
		}
		return JSONResult(entries), nil

	case ToolGetTimeline:
		limit := 50
		if v, ok := p.Arguments["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		records, err := store.ListEventRecords(ctx, strArg("entityType"), strArg("entityID"), limit)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if records == nil {
			records = []wms.EventRecord{}
		}
		return JSONResult(records), nil

	// --- v2 Outcome tools ---

	case ToolCreateOutcome:
		o := &wms.Outcome{
			ID:            strArg("id"),
			Title:         strArg("title"),
			Description:   strArg("description"),
			Status:        strArgDefault("status", wms.StatusPending),
			OriginHost:    p.Meta.Host,
			OriginSession: p.Meta.SessionID,
			OriginAgent:   p.Meta.AgentType,
		}
		if err := store.CreateOutcome(ctx, o); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		// WP10/WP12: a create with a non-default initial status is itself a
		// pending→status transition (WP1b/WP1 site 1/WP12 Site B all fold
		// status="active" into this call). Route it through the engine so it
		// reaches JournalObserver/HookObserver like every other transition —
		// this was a sixth uncataloged WP10 mutation path, silently skipping
		// the audit trail. Store-side event-record bookkeeping is unaffected:
		// OpenEventRecord below opens the entity's first interval directly at
		// its actual status (there is no prior interval to close), and
		// OnStatusChange itself never touches wms_intervals/
		// TransitionEventRecord (confirmed by reading engine.go), so this
		// cannot double-record an interval.
		if o.Status != wms.StatusPending {
			eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
				EntityType: wms.EntityOutcome, EntityID: o.ID,
				OldStatus: wms.StatusPending, NewStatus: o.Status,
				SessionID: p.Meta.SessionID, AgentName: p.Meta.AgentType, Host: p.Meta.Host,
				Notes: fmt.Sprintf("created with initial status %s via %s", o.Status, p.Name),
			})
		}
		if parents, ok := p.Arguments["parentOutcomeIDs"].([]interface{}); ok {
			for _, pid := range parents {
				if s, ok := pid.(string); ok && s != "" {
					if err := store.AddOutcomeEdge(ctx, s, o.ID); err != nil {
						return Result{}, &CallError{Code: -32000, Message: err.Error()}
					}
				}
			}
		}
		store.OpenEventRecord(ctx, wms.EntityOutcome, o.ID, o.Status, p.Meta.SessionID, p.Meta.AgentType, p.Meta.Host) //nolint:errcheck
		applyCreatorUserTag(ctx, store, wms.EntityOutcome, o.ID)
		applyRuntimeTag(ctx, store, wms.EntityOutcome, o.ID, runtime)
		tagErrors := applyInlineTags(ctx, store, wms.EntityOutcome, o.ID, p.Arguments["tags"])
		if len(tagErrors) > 0 {
			return JSONResult(map[string]interface{}{
				"message":   "Created outcome: " + o.Title,
				"tagErrors": tagErrors,
			}), nil
		}
		return TextResult("Created outcome: " + o.Title), nil

	case ToolGetOutcome:
		o, err := store.GetOutcome(ctx, strArg("id"))
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		parents, _ := store.GetOutcomeParents(ctx, o.ID)
		result := map[string]interface{}{
			"id": o.ID, "title": o.Title, "description": o.Description,
			"status": o.Status, "prior_status": o.PriorStatus, "focus": o.Focus,
			"origin_host": o.OriginHost, "origin_session": o.OriginSession, "origin_agent": o.OriginAgent,
			"created_at": o.CreatedAt, "updated_at": o.UpdatedAt, "parent_ids": parents,
		}
		return JSONResult(result), nil

	case ToolListOutcomes:
		tagFilters := map[string]string{}
		if tf, ok := p.Arguments["tagFilters"].(map[string]interface{}); ok {
			for k, v := range tf {
				if s, ok := v.(string); ok {
					tagFilters[k] = s
				}
			}
		}
		outcomes, err := store.ListOutcomes(ctx, strArg("parentOutcomeID"), tagFilters, strArg("status"), strArg("query"))
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(outcomes), nil

	case ToolUpdateOutcomeStatus:
		entityID := strArg("id")
		newStatus := strArg("status")
		o, err := store.GetOutcome(ctx, entityID)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		oldStatus := o.Status
		if !wms.ValidTransition(wms.EntityOutcome, oldStatus, newStatus) {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("invalid transition outcome: %s → %s", oldStatus, newStatus)}
		}
		role := p.Meta.AgentType
		allowed, err := store.RoleAllowed(ctx, wms.EntityOutcome, oldStatus, newStatus, role)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if !allowed {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("role %q not allowed: outcome %s→%s", role, oldStatus, newStatus)}
		}
		if err := store.UpdateOutcomeStatus(ctx, entityID, newStatus); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		// Status already persisted above; the event record is observability only,
		// so log a failure rather than failing the call — but never swallow it.
		if err := store.TransitionEventRecord(ctx, wms.EntityOutcome, entityID, newStatus, p.Meta.SessionID, p.Meta.AgentType, p.Meta.Host); err != nil {
			slog.Warn("wms-mcp: transition event record failed",
				"entity_type", wms.EntityOutcome, "entity_id", entityID, "status", newStatus, "err", err)
		}
		// WP10 path 1: never leave the journal's notes column blank-by-omission.
		outcomeNotes := strArg("notes")
		if outcomeNotes == "" {
			outcomeNotes = fmt.Sprintf("status change via %s (no notes provided)", p.Name)
		}
		eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
			EntityType: wms.EntityOutcome, EntityID: entityID,
			OldStatus: oldStatus, NewStatus: newStatus,
			SessionID: p.Meta.SessionID, AgentName: p.Meta.AgentType, Host: p.Meta.Host,
			Notes: outcomeNotes,
		})
		// Close-out backstop: surface discipline misses inline in the response.
		// Advisory only — the transition already succeeded above.
		msg := fmt.Sprintf("Updated outcome %s: %s → %s", entityID, oldStatus, newStatus)
		msg += wms.FormatCloseoutWarnings(wms.CloseoutWarnings(ctx, store, entityID, newStatus))
		return TextResult(msg), nil

	case ToolRenameOutcome:
		entityID := strArg("id")
		newTitle := strArg("title")
		if newTitle == "" {
			return Result{}, &CallError{Code: -32602, Message: "title is required"}
		}
		o, err := store.GetOutcome(ctx, entityID)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		oldTitle := o.Title
		if err := store.UpdateOutcomeTitle(ctx, entityID, newTitle); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Renamed outcome %s: %q → %q", entityID, oldTitle, newTitle)), nil

	case ToolAddOutcomeParent, "wms.addOutcomeParent":
		parentID := strArg("parentID")
		childID := strArg("childID")
		if parentID == "" || childID == "" {
			return Result{}, &CallError{Code: -32602, Message: "parentID and childID are required"}
		}
		// Pre-flight existence checks on BOTH sides: AddOutcomeEdge's INSERT
		// IGNORE silently swallows a foreign-key violation for an unknown
		// outcome id, which would otherwise report success for an edge that
		// was never actually written.
		if _, err := store.GetOutcome(ctx, parentID); err != nil {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("parent outcome %s: %s", parentID, err.Error())}
		}
		if _, err := store.GetOutcome(ctx, childID); err != nil {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("child outcome %s: %s", childID, err.Error())}
		}
		if err := store.AddOutcomeEdge(ctx, parentID, childID); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Added outcome edge: %s → %s", parentID, childID)), nil

	case ToolRemoveOutcomeParent, "wms.removeOutcomeParent":
		parentID := strArg("parentID")
		childID := strArg("childID")
		if parentID == "" || childID == "" {
			return Result{}, &CallError{Code: -32602, Message: "parentID and childID are required"}
		}
		if err := store.RemoveOutcomeEdge(ctx, parentID, childID); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Removed outcome edge: %s → %s", parentID, childID)), nil

	// --- v2 WorkUnit tools ---

	case ToolCreateWorkUnit:
		outcomeID := strArg("outcomeID")
		// WorkUnit-under-terminal-Outcome guard (operator ruling, ANALYSIS.md
		// §5/§2.1.3): work used to land silently under an already-`done`
		// Outcome, leaving it permanently invisible to any close-out review.
		// Reject with a message naming the reopen edge — no auto-reopen. Also
		// checks existence directly here (LF-VER-3) rather than falling
		// through to CreateWorkUnit's own error: an unknown outcomeID used to
		// hit the INSERT's FK constraint and leak a raw driver string
		// ("Error 1452 ... FOREIGN KEY (outcome_id) REFERENCES outcomes
		// (id)"), unlike the clean sentence right above it. Both checks now
		// resolve before any backend-specific INSERT runs, so the message is
		// identical on every backend. Any OTHER GetOutcome error (a real
		// store failure) fails the call outright rather than silently
		// letting creation proceed past a check that could not actually run
		// (adversarial-review finding).
		if outcomeID != "" {
			outcome, e := store.GetOutcome(ctx, outcomeID)
			switch {
			case e == nil:
				if wms.IsTerminal(wms.EntityOutcome, outcome.Status) {
					return Result{}, &CallError{
						Code:    -32000,
						Message: fmt.Sprintf("outcome %s is %s; reopen it first (wms_updateOutcomeStatus → review) or create the work unit under an open outcome", outcomeID, outcome.Status),
					}
				}
			case errors.Is(e, storeTypes.ErrNotFound):
				return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("outcome %s does not exist — create it first or pass a valid outcomeID", outcomeID)}
			default:
				return Result{}, &CallError{Code: -32000, Message: e.Error()}
			}
		}
		wu := &wms.WorkUnit{
			ID:            strArg("id"),
			OutcomeID:     outcomeID,
			Title:         strArg("title"),
			Description:   strArg("description"),
			AgentID:       strArg("agentID"),
			Status:        strArgDefault("status", wms.StatusPending),
			Brief:         strArg("brief"),
			OriginHost:    p.Meta.Host,
			OriginSession: p.Meta.SessionID,
			OriginAgent:   p.Meta.AgentType,
		}
		if err := store.CreateWorkUnit(ctx, wu); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		// WP10/WP12: same fix as ToolCreateOutcome above — a create with a
		// non-default initial status must still reach the engine so the
		// implicit pending→status transition gets a journal row and observer
		// calls, not silence. See that case's comment for why this cannot
		// double-record an interval against OpenEventRecord below.
		if wu.Status != wms.StatusPending {
			eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
				EntityType: wms.EntityWorkUnit, EntityID: wu.ID,
				OldStatus: wms.StatusPending, NewStatus: wu.Status,
				SessionID: p.Meta.SessionID, AgentName: p.Meta.AgentType, Host: p.Meta.Host,
				Notes: fmt.Sprintf("created with initial status %s via %s", wu.Status, p.Name),
			})
		}
		store.OpenEventRecord(ctx, wms.EntityWorkUnit, wu.ID, wu.Status, p.Meta.SessionID, p.Meta.AgentType, p.Meta.Host) //nolint:errcheck
		applyCreatorUserTag(ctx, store, wms.EntityWorkUnit, wu.ID)
		applyRuntimeTag(ctx, store, wms.EntityWorkUnit, wu.ID, runtime)
		tagErrors := applyInlineTags(ctx, store, wms.EntityWorkUnit, wu.ID, p.Arguments["tags"])
		// Dispatch-time reminder (W3): surface any required tag key still
		// missing after direct + inherited (parent outcome) tags are
		// considered, as an advisory hint appended to the success response —
		// never a block. If the required-keys lookup itself fails, the
		// creation still succeeds; the reminder is observability, not a gate.
		// Computed AFTER inline tags apply, so a required key satisfied via
		// `tags` doesn't also warn.
		warnings := missingRequiredTagWarnings(ctx, store, wms.EntityWorkUnit, wu.ID)
		if len(warnings) > 0 || len(tagErrors) > 0 {
			result := map[string]interface{}{
				"message": "Created work unit: " + wu.Title,
			}
			if len(warnings) > 0 {
				result["warnings"] = warnings
			}
			if len(tagErrors) > 0 {
				result["tagErrors"] = tagErrors
			}
			return JSONResult(result), nil
		}
		return TextResult("Created work unit: " + wu.Title), nil

	case ToolGetWorkUnit:
		wu, err := store.GetWorkUnit(ctx, strArg("id"))
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(wu), nil

	case ToolListWorkUnits:
		var units []*wms.WorkUnit
		var err error
		if ready, _ := p.Arguments["ready"].(bool); ready {
			units, err = store.ListReadyWorkUnits(ctx, strArg("outcomeID"))
		} else {
			units, err = store.ListWorkUnits(ctx, strArg("outcomeID"))
		}
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if units == nil {
			units = []*wms.WorkUnit{}
		}
		return JSONResult(units), nil

	case ToolUpdateWorkUnitStatus:
		entityID := strArg("id")
		newStatus := strArg("status")
		wu, err := store.GetWorkUnit(ctx, entityID)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		oldStatus := wu.Status
		if !wms.ValidTransition(wms.EntityWorkUnit, oldStatus, newStatus) {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("invalid transition workunit: %s → %s", oldStatus, newStatus)}
		}
		role := p.Meta.AgentType
		allowed, err := store.RoleAllowed(ctx, wms.EntityWorkUnit, oldStatus, newStatus, role)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if !allowed {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("role %q not allowed: workunit %s→%s", role, oldStatus, newStatus)}
		}
		if err := store.UpdateWorkUnitStatus(ctx, entityID, newStatus); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		// Status already persisted above; the event record is observability only,
		// so log a failure rather than failing the call — but never swallow it.
		if err := store.TransitionEventRecord(ctx, wms.EntityWorkUnit, entityID, newStatus, p.Meta.SessionID, p.Meta.AgentType, p.Meta.Host); err != nil {
			slog.Warn("wms-mcp: transition event record failed",
				"entity_type", wms.EntityWorkUnit, "entity_id", entityID, "status", newStatus, "err", err)
		}
		// WP10 path 1: never leave the journal's notes column blank-by-omission.
		wuNotes := strArg("notes")
		if wuNotes == "" {
			wuNotes = fmt.Sprintf("status change via %s (no notes provided)", p.Name)
		}
		eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
			EntityType: wms.EntityWorkUnit, EntityID: entityID,
			OldStatus: oldStatus, NewStatus: newStatus,
			SessionID: p.Meta.SessionID, AgentName: p.Meta.AgentType, Host: p.Meta.Host,
			Notes: wuNotes,
		})
		msg := fmt.Sprintf("Updated workunit %s: %s → %s", entityID, oldStatus, newStatus)
		if wms.IsTerminal(wms.EntityWorkUnit, newStatus) {
			msg += closeoutNudge(ctx, store, wu.OutcomeID)
		}
		return TextResult(msg), nil

	case ToolRenameWorkUnit:
		entityID := strArg("id")
		newTitle := strArg("title")
		if newTitle == "" {
			return Result{}, &CallError{Code: -32602, Message: "title is required"}
		}
		wu, err := store.GetWorkUnit(ctx, entityID)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		oldTitle := wu.Title
		if err := store.UpdateWorkUnitTitle(ctx, entityID, newTitle); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Renamed workunit %s: %q → %q", entityID, oldTitle, newTitle)), nil

	case ToolAssignWorkUnit:
		if err := store.AssignWorkUnit(ctx, strArg("id"), strArg("agentID")); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult("Assigned work unit " + strArg("id") + " to " + strArg("agentID")), nil

	case ToolClaimWorkUnit:
		id := strArg("id")
		agentID := p.Meta.AgentType
		preClaimStatus, err := store.ClaimWorkUnit(ctx, id, agentID)
		if err != nil {
			var alreadyClaimed *storeTypes.AlreadyClaimedError
			if errors.As(err, &alreadyClaimed) {
				return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("work unit '%s' already claimed by @%s — ask the lead for another work unit", id, alreadyClaimed.Owner)}
			}
			var notClaimable *storeTypes.NotClaimableError
			if errors.As(err, &notClaimable) {
				return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("work unit '%s' not claimable — status is %s", id, notClaimable.Status)}
			}
			if errors.Is(err, storeTypes.ErrNotFound) {
				return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("work unit '%s' not found", id)}
			}
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		// Only a real pending→active transition gets a status-change event —
		// adopt (was active, no owner) and idempotent re-claim (was active,
		// already owned by this agent) don't change status, so firing one
		// would be a phantom event with a fabricated OldStatus. This also
		// gates focusInterval below: only a real transition sends hookd the
		// WMSStatusChange POST that triggers its (async, best-effort) attempt
		// to open a focus interval.
		focusInterval := "unchanged"
		if preClaimStatus == wms.StatusPending {
			focusInterval = "requested"
			// agentID (p.Meta.AgentType) is empty by design for a lead claim
			// (CLAUDE.md's "claimed WorkUnit agent_id" convention) — leaving
			// it in the fmt.Sprintf below would degrade to a note that reads
			// like a truncation ("claimed by "), caught in adversarial
			// review. The claim's own claimed_by/agentID fields (below) are
			// unaffected — this substitution is for the journal note only.
			claimNoteAgent := agentID
			if claimNoteAgent == "" {
				claimNoteAgent = "the lead (no agent type)"
			}
			if err := store.TransitionEventRecord(ctx, wms.EntityWorkUnit, id, wms.StatusActive, p.Meta.SessionID, p.Meta.AgentType, p.Meta.Host); err != nil {
				slog.Warn("wms-mcp: transition event record failed",
					"entity_type", wms.EntityWorkUnit, "entity_id", id, "status", wms.StatusActive, "err", err)
			}
			eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
				EntityType: wms.EntityWorkUnit, EntityID: id,
				OldStatus: wms.StatusPending, NewStatus: wms.StatusActive,
				SessionID: p.Meta.SessionID, AgentName: p.Meta.AgentType, Host: p.Meta.Host,
				Notes: fmt.Sprintf("claimed by %s", claimNoteAgent),
			})
		}

		wu, err := store.GetWorkUnit(ctx, id)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		entityTags, err := store.GetEntityTags(ctx, wms.EntityWorkUnit, id)
		if err != nil {
			slog.Warn("wms-mcp: get entity tags failed after claim", "workunit_id", id, "err", err)
		}
		tags := map[string]string{}
		for _, t := range entityTags {
			tags[t.TagKey] = t.TagValue
		}
		resp := map[string]interface{}{
			"id":             wu.ID,
			"title":          wu.Title,
			"outcome_id":     wu.OutcomeID,
			"brief":          wu.Brief,
			"description":    wu.Description,
			"tags":           tags,
			"claimed_by":     agentID,
			"focus_interval": focusInterval,
		}
		if wu.ClaimedAt != nil {
			resp["claimed_at"] = wu.ClaimedAt.UTC().Format(time.RFC3339)
		}
		return JSONResult(resp), nil

	case ToolDeliverResult:
		id := strArg("id")
		summary := strArg("summary")
		result := strArg("result")
		if id == "" || summary == "" || result == "" {
			return Result{}, &CallError{Code: -32602, Message: "id, summary, and result are required"}
		}
		wu, err := store.GetWorkUnit(ctx, id)
		if err != nil {
			if errors.Is(err, storeTypes.ErrNotFound) {
				return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("work unit '%s' not found", id)}
			}
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if wu.Status != wms.StatusActive && wu.Status != wms.StatusReview {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("work unit '%s' is not active or in review (status: %s) — cannot deliver a result", id, wu.Status)}
		}
		caller := p.Meta.AgentType
		if caller != "" && wu.AgentID != caller {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("work unit '%s' is owned by %q, not %q — only the owner or the lead may deliver", id, wu.AgentID, caller)}
		}

		var artifactPaths string
		if raw, ok := p.Arguments["artifact_paths"].([]interface{}); ok {
			var paths []string
			for _, v := range raw {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					paths = append(paths, strings.TrimSpace(s))
				}
			}
			artifactPaths = strings.Join(paths, "\n")
		}
		d := wms.Deliverable{
			EntityType:    wms.EntityWorkUnit,
			EntityID:      id,
			AgentID:       caller,
			SessionID:     p.Meta.SessionID,
			Summary:       summary,
			Result:        result,
			ArtifactPaths: artifactPaths,
		}
		if err := store.InsertDeliverable(ctx, d); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}

		if wu.Status == wms.StatusReview {
			// Redelivery while already in review (MCP-KG-2): the tool's own
			// description has always said redelivery is allowed, and
			// wms_listDeliverables documents "take the last row" — the code just
			// never actually permitted it, forcing a pointless review→active→
			// review round-trip before every re-delivery. No status transition
			// here: the WU is already where it needs to be for the reviewer to
			// re-read it. Journaled directly since OnStatusChange's transition
			// journaling below never fires for this path (there is no transition).
			if err := wms.RecordMutation(ctx, store, wms.JournalEntry{
				EntityType: wms.EntityWorkUnit, EntityID: id,
				Field: "deliverable", Notes: fmt.Sprintf("redelivered while in review: %s", summary),
				SessionID: p.Meta.SessionID, AgentID: caller, Host: p.Meta.Host,
			}); err != nil {
				slog.Warn("wms-mcp: journal redelivery failed", "workunit_id", id, "err", err)
			}
			return TextResult(fmt.Sprintf("Redelivered result for work unit %s (status unchanged: review)", id)), nil
		}

		oldStatus := wu.Status
		newStatus := wms.StatusReview
		if !wms.ValidTransition(wms.EntityWorkUnit, oldStatus, newStatus) {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("invalid transition workunit: %s → %s", oldStatus, newStatus)}
		}
		role := p.Meta.AgentType
		allowed, err := store.RoleAllowed(ctx, wms.EntityWorkUnit, oldStatus, newStatus, role)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if !allowed {
			return Result{}, &CallError{Code: -32000, Message: fmt.Sprintf("role %q not allowed: workunit %s→%s", role, oldStatus, newStatus)}
		}
		if err := store.UpdateWorkUnitStatus(ctx, id, newStatus); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if err := store.TransitionEventRecord(ctx, wms.EntityWorkUnit, id, newStatus, p.Meta.SessionID, p.Meta.AgentType, p.Meta.Host); err != nil {
			slog.Warn("wms-mcp: transition event record failed",
				"entity_type", wms.EntityWorkUnit, "entity_id", id, "status", newStatus, "err", err)
		}
		eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
			EntityType: wms.EntityWorkUnit, EntityID: id,
			OldStatus: oldStatus, NewStatus: newStatus,
			SessionID: p.Meta.SessionID, AgentName: p.Meta.AgentType, Host: p.Meta.Host,
			Notes: summary,
		})
		return TextResult(fmt.Sprintf("Delivered result for work unit %s: %s → %s", id, oldStatus, newStatus)), nil

	case ToolListDeliverables:
		id := strArg("entityID")
		if id == "" {
			return Result{}, &CallError{Code: -32602, Message: "entityID is required"}
		}
		limit := 50
		if v, ok := p.Arguments["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		deliverables, err := store.ListDeliverables(ctx, wms.EntityWorkUnit, id, limit)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if deliverables == nil {
			deliverables = []wms.Deliverable{}
		}
		return JSONResult(deliverables), nil

	case ToolClassifyEntity:
		if ActiveClassifier == nil {
			return JSONResult(map[string]interface{}{
				"applied": []interface{}{},
				"skipped": []interface{}{map[string]interface{}{"tag_key": "*", "reason": "classifier not configured"}},
			}), nil
		}
		result, err := ActiveClassifier.Classify(ctx, strArg("entityType"), strArg("entityID"))
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(result), nil

	case ToolListRelated, "wms.listRelated":
		relStore, ok := store.(interface {
			ListRelatedEntities(context.Context, storeTypes.ListRelatedOpts) ([]storeTypes.RelatedEntity, error)
		})
		if !ok {
			return Result{}, &CallError{Code: -32000, Message: "store does not support listRelated"}
		}
		tagFilters := map[string]string{}
		if tf, ok := p.Arguments["tagFilters"].(map[string]interface{}); ok {
			for k, v := range tf {
				if s, ok := v.(string); ok {
					tagFilters[k] = s
				}
			}
		}
		includeTerminal, _ := p.Arguments["includeTerminal"].(bool)
		staleHours := 4
		if v, ok := p.Arguments["staleHours"].(float64); ok && v > 0 {
			staleHours = int(v)
		}
		related, err := relStore.ListRelatedEntities(ctx, storeTypes.ListRelatedOpts{
			Query:           strArg("query"),
			TagFilters:      tagFilters,
			IncludeTerminal: includeTerminal,
			StaleHours:      staleHours,
		})
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if related == nil {
			related = []storeTypes.RelatedEntity{}
		}
		return JSONResult(related), nil

	case ToolSearch, "wms.search":
		query := strArg("query")
		if query == "" {
			return Result{}, &CallError{Code: -32602, Message: "query is required"}
		}
		q := wms.SearchQuery{
			Query:   query,
			User:    strArg("user"),
			Host:    strArg("host"),
			Status:  strArg("status"),
			Session: strArg("session"),
		}
		if v := strArg("type"); v != "" {
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					q.Types = append(q.Types, t)
				}
			}
		}
		if tags, ok := p.Arguments["tag"].([]interface{}); ok {
			for _, t := range tags {
				if s, ok := t.(string); ok && strings.TrimSpace(s) != "" {
					q.Tags = append(q.Tags, strings.TrimSpace(s))
				}
			}
		}
		if v := strArg("since"); v != "" {
			// Accept either a relative duration ("72h", matching the CLI --since
			// convention) or an absolute RFC3339 timestamp. A value that parses as
			// neither must not silently fall through as "no filter" — that would
			// return unfiltered results as if the filter had applied.
			switch d, derr := time.ParseDuration(v); {
			case derr == nil:
				q.Since = time.Now().UTC().Add(-d)
			default:
				t, terr := time.Parse(time.RFC3339, v)
				if terr != nil {
					return Result{}, &CallError{Code: -32602, Message: `since: want a duration ("72h") or RFC3339 timestamp`}
				}
				q.Since = t
			}
		}
		if v, ok := p.Arguments["limit"].(float64); ok && v > 0 {
			q.Limit = int(v)
		}
		hits, err := store.Search(ctx, q)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if hits == nil {
			hits = []wms.Hit{}
		}
		return JSONResult(hits), nil

	case ToolSnapshotEntityTags, "wms.snapshotEntityTags":
		tagKey := strArg("tagKey")
		batchID := strArg("batchID")
		if tagKey == "" || batchID == "" {
			return Result{}, &CallError{Code: -32602, Message: "tagKey and batchID are required"}
		}

		// Exactly one of the two input forms is accepted: the new `entities`
		// array (mixed entity types in one call — the fix for the truncation
		// footgun, see snapshotEntityTags's doc comment), or the original
		// entityType + entityIDs pair (single type, kept for backward
		// compatibility with existing callers). Presence, not non-emptiness,
		// decides which form the caller picked — an empty `entities` array
		// still selects that form and is rejected below, same as an empty
		// entityIDs is under the legacy form. Presence is checked separately
		// from the type assertion: a caller who supplies `entities` with the
		// wrong JSON type (e.g. a string) must get a "wrong shape" error, not
		// silently fall through to the legacy branch and see the misleading
		// "supply exactly one of..." message when they did supply exactly one,
		// just malformed.
		rawEntities, entitiesGiven := p.Arguments["entities"]
		if entitiesGiven && rawEntities == nil {
			// JSON `null` decodes into map[string]interface{} as a PRESENT key
			// with a nil value — so a caller (or a client library) that pads an
			// unused optional with an explicit null must be treated the same as
			// omitting it entirely, matching entityIDs' tolerant null handling
			// below (`p.Arguments["entityIDs"].([]interface{})` fails cleanly to
			// "absent" for a nil value). Without this, "entities": null would
			// wrongly reject an otherwise-valid legacy entityType+entityIDs call.
			entitiesGiven = false
		}
		entitiesArg, entitiesOK := rawEntities.([]interface{})
		if entitiesGiven && !entitiesOK {
			return Result{}, &CallError{Code: -32602, Message: "entities must be an array of {entityType, entityID} objects"}
		}
		entityType := strArg("entityType")
		entityIDsArg, entityIDsGiven := p.Arguments["entityIDs"].([]interface{})
		legacyGiven := entityType != "" || entityIDsGiven
		if entitiesGiven == legacyGiven {
			return Result{}, &CallError{Code: -32602, Message: "supply exactly one of: entities ([{entityType, entityID}, ...]), or entityType + entityIDs"}
		}

		var refs []stewardEntityRef
		if entitiesGiven {
			if len(entitiesArg) == 0 {
				return Result{}, &CallError{Code: -32602, Message: "entities must be a non-empty array"}
			}
			for i, raw := range entitiesArg {
				obj, ok := raw.(map[string]interface{})
				if !ok {
					return Result{}, &CallError{Code: -32602, Message: fmt.Sprintf("entities[%d] must be an object with entityType and entityID", i)}
				}
				et, _ := obj["entityType"].(string)
				et = strings.TrimSpace(et)
				if et != wms.EntityOutcome && et != wms.EntityWorkUnit {
					return Result{}, &CallError{Code: -32602, Message: fmt.Sprintf("entities[%d].entityType must be outcome or workunit", i)}
				}
				eid, _ := obj["entityID"].(string)
				eid = strings.TrimSpace(eid)
				if eid == "" {
					return Result{}, &CallError{Code: -32602, Message: fmt.Sprintf("entities[%d].entityID is required", i)}
				}
				refs = append(refs, stewardEntityRef{EntityType: et, EntityID: eid})
			}
		} else {
			if entityType != wms.EntityOutcome && entityType != wms.EntityWorkUnit {
				return Result{}, &CallError{Code: -32602, Message: "entityType must be outcome or workunit"}
			}
			// Validate every entry rather than silently dropping the bad ones —
			// entityIDs: ["wu-1", null, "wu-3"] used to write 2 lines and report
			// success, indistinguishable from a caller who genuinely meant only
			// 2 entities. Same class of bug as a malformed `entities` item
			// (above), same fix: name the bad index and refuse the call.
			var entityIDs []string
			for i, v := range entityIDsArg {
				s, ok := v.(string)
				if !ok {
					return Result{}, &CallError{Code: -32602, Message: fmt.Sprintf("entityIDs[%d] must be a string", i)}
				}
				s = strings.TrimSpace(s)
				if s == "" {
					return Result{}, &CallError{Code: -32602, Message: fmt.Sprintf("entityIDs[%d] must not be blank", i)}
				}
				entityIDs = append(entityIDs, s)
			}
			if len(entityIDs) == 0 {
				return Result{}, &CallError{Code: -32602, Message: "entityIDs must be a non-empty array"}
			}
			for _, id := range entityIDs {
				refs = append(refs, stewardEntityRef{EntityType: entityType, EntityID: id})
			}
		}

		path, err := snapshotEntityTags(ctx, store, refs, tagKey, batchID)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(map[string]interface{}{"path": path}), nil

	case ToolRollbackTags, "wms.rollbackTags":
		batchID := strArg("batchID")
		if batchID == "" {
			return Result{}, &CallError{Code: -32602, Message: "batchID is required"}
		}
		reverted, skipped, notFound, failed, err := rollbackTags(ctx, store, batchID)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(map[string]interface{}{
			"reverted": reverted, "skipped": skipped, "notFound": notFound, "failed": failed,
		}), nil

	case ToolAddRelation, "wms.addRelation":
		relStore, ok := store.(relationStore)
		if !ok {
			return Result{}, &CallError{Code: -32000, Message: "store does not support relations"}
		}
		kind := strArg("kind")
		fromType := strArg("fromType")
		fromID := strArg("fromID")
		toType := strArg("toType")
		toID := strArg("toID")
		note := strArg("note")
		if kind == "" || fromType == "" || fromID == "" || toType == "" || toID == "" {
			return Result{}, &CallError{Code: -32602, Message: "kind, fromType, fromID, toType, and toID are required"}
		}

		kinds, err := relStore.ListRelationKinds(ctx)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		validKind := false
		validKinds := make([]string, 0, len(kinds))
		for _, k := range kinds {
			validKinds = append(validKinds, k.Kind)
			if k.Kind == kind {
				validKind = true
			}
		}
		if !validKind {
			return Result{}, &CallError{Code: -32602, Message: fmt.Sprintf("unknown relation kind %q — valid kinds: %s", kind, strings.Join(validKinds, ", "))}
		}

		if err := relStore.AddRelation(ctx, kind, fromType, fromID, toType, toID, p.Meta.AgentType, "manual", note); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Added relation: %s %s %s", fromID, kind, toID)), nil

	case ToolRemoveRelation, "wms.removeRelation":
		relStore, ok := store.(relationStore)
		if !ok {
			return Result{}, &CallError{Code: -32000, Message: "store does not support relations"}
		}
		kind := strArg("kind")
		fromType := strArg("fromType")
		fromID := strArg("fromID")
		toType := strArg("toType")
		toID := strArg("toID")
		if kind == "" || fromType == "" || fromID == "" || toType == "" || toID == "" {
			return Result{}, &CallError{Code: -32602, Message: "kind, fromType, fromID, toType, and toID are required"}
		}
		if err := relStore.RemoveRelation(ctx, kind, fromType, fromID, toType, toID); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult(fmt.Sprintf("Removed relation: %s %s %s", fromID, kind, toID)), nil

	case ToolListRelations, "wms.listRelations":
		relStore, ok := store.(relationStore)
		if !ok {
			return Result{}, &CallError{Code: -32000, Message: "store does not support relations"}
		}
		entityType := strArg("entityType")
		entityID := strArg("entityID")
		if entityType == "" || entityID == "" {
			return Result{}, &CallError{Code: -32602, Message: "entityType and entityID are required"}
		}
		direction := strArgDefault("direction", "both")
		kind := strArg("kind")
		relations, err := relStore.ListRelations(ctx, entityType, entityID, direction, kind)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if relations == nil {
			relations = []storeTypes.Relation{}
		}
		return JSONResult(relations), nil

	case ToolListRelationKinds, "wms.listRelationKinds":
		relStore, ok := store.(relationStore)
		if !ok {
			return Result{}, &CallError{Code: -32000, Message: "store does not support relations"}
		}
		kinds, err := relStore.ListRelationKinds(ctx)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if kinds == nil {
			kinds = []storeTypes.RelationKind{}
		}
		return JSONResult(kinds), nil

	default:
		return Result{}, &CallError{Code: -32601, Message: "unknown tool: " + p.Name}
	}
}

// missingRequiredTagWarnings returns one advisory warning per required tag key
// that has no binding on the entity, direct or inherited from a parent outcome.
// It is best-effort: if either the required-keys lookup or the entity-tags
// lookup fails, it returns no warnings (the caller's primary operation must
// not be blocked by an observability hint).
func missingRequiredTagWarnings(ctx context.Context, store wms.Store, entityType, entityID string) []string {
	requiredKeys, err := store.ListRequiredTagKeys(ctx)
	if err != nil {
		slog.Warn("wms-mcp: required-key lookup failed; skipping dispatch reminder",
			"entity_type", entityType, "entity_id", entityID, "err", err)
		return nil
	}
	if len(requiredKeys) == 0 {
		return nil
	}
	tags, err := resolveEntityTags(ctx, store, entityType, entityID)
	if err != nil {
		slog.Warn("wms-mcp: entity-tags lookup failed; skipping dispatch reminder",
			"entity_type", entityType, "entity_id", entityID, "err", err)
		return nil
	}
	present := make(map[string]bool, len(tags))
	for _, t := range tags {
		present[t.TagKey] = true
	}
	var warnings []string
	for _, key := range requiredKeys {
		if !present[key] {
			warnings = append(warnings, fmt.Sprintf(
				"required tag %q not yet applied — set it before dispatching", key))
		}
	}
	return warnings
}

// applyCreatorUserTag auto-applies user:<CreatorUser> to a freshly created
// entity so work can be faceted by who created it (forward-only — no historical
// backfill; the creator of older entities is unknown). No-op when CreatorUser is
// unset. Best-effort: the entity was already created, so a tag failure is logged
// and swallowed rather than failing the create. source="classifier" marks it as
// engine-applied (not a manual operator tag). The `user` tag key is seeded by a
// migration; the empty-value default lets TagEntity attach this value to it.
func applyCreatorUserTag(ctx context.Context, store wms.Store, entityType, entityID string) {
	if CreatorUser == "" {
		return
	}
	if err := store.TagEntity(ctx, entityType, entityID, "user", CreatorUser, "classifier", ""); err != nil {
		slog.Warn("wms-mcp: auto user-tag failed",
			"entity_type", entityType, "entity_id", entityID, "user", CreatorUser, "err", err)
	}
}

// tagStewardDir returns the tag-steward snapshot directory under the install's
// var dir, creating it on first use. It resolves the var dir the same way
// config.go derives DataDir: prefer TEAMSTER_DATA_DIR (which the installer sets
// in settings.json and equals $BASEDIR/var), else fall back to
// TEAMSTER_BASEDIR/var (the shell-profile master override). A stdio wms-mcp
// inherits settings.json env (so it has DATA_DIR) but not always BASEDIR, so
// DATA_DIR must win. With neither set we error rather than writing snapshots to
// the process cwd, where the steward could never find the rollback state again.
func tagStewardDir() (string, error) {
	var dir string
	switch {
	case strings.TrimSpace(os.Getenv("TEAMSTER_DATA_DIR")) != "":
		dir = filepath.Join(strings.TrimSpace(os.Getenv("TEAMSTER_DATA_DIR")), "tag-steward")
	case strings.TrimSpace(os.Getenv("TEAMSTER_BASEDIR")) != "":
		dir = filepath.Join(strings.TrimSpace(os.Getenv("TEAMSTER_BASEDIR")), "var", "tag-steward")
	default:
		return "", fmt.Errorf("neither TEAMSTER_DATA_DIR nor TEAMSTER_BASEDIR is set; cannot locate the tag-steward snapshot directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating tag-steward dir %s: %w", dir, err)
	}
	return dir, nil
}

// stewardOldBinding is one prior (value, source) pair a multi-cardinality tag
// key held at snapshot time. A key like work-type may hold several values at
// once (e.g. {bug, infra}); rollback must restore ALL of them, not just one.
type stewardOldBinding struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

// stewardSnapshotKind marks a snapshot line as written by this schema
// version, for FUTURE validation to key off exactly rather than structurally
// (see validateSnapshotLine). Omitted (empty) on every snapshot written
// before this field existed — those must keep validating and rolling back
// correctly via structural checks alone, so nothing here may require it yet.
const stewardSnapshotKind = "steward-snapshot-v1"

// stewardSnapshotLine is one JSONL record in a steward batch snapshot. It
// captures enough of the pre-change binding to restore it: an empty old_value
// (and no OldValues) means the tag key was absent before the steward applied
// it. new_value is left empty at snapshot time — the apply has not happened
// yet.
//
// OldValues carries every prior binding for a multi-cardinality key;
// OldValue/OldSource are kept in sync with OldValues[0] (when non-empty) so
// a snapshot THIS binary writes can still be read by an OLDER one (it would
// only restore the first binding, exactly its pre-existing behavior, rather
// than choking on or misreading an unrecognized field). Real snapshots
// written before OldValues existed carry only the scalar pair — see
// oldBindings for the dual-read that keeps those files rolling back
// correctly forever, not just until the next fold.
type stewardSnapshotLine struct {
	EntityType string              `json:"entity_type"`
	EntityID   string              `json:"entity_id"`
	TagKey     string              `json:"tag_key"`
	OldValue   string              `json:"old_value"`
	OldSource  string              `json:"old_source"`
	OldValues  []stewardOldBinding `json:"old_values,omitempty"`
	NewValue   string              `json:"new_value,omitempty"`
	Batch      string              `json:"batch"`
	Kind       string              `json:"kind,omitempty"`
}

// oldBindings returns every prior (value, source) pair line records,
// preferring OldValues and falling back to the legacy scalar OldValue/
// OldSource for a snapshot written before multi-cardinality support existed.
// A nil result means the key was absent before the steward touched it.
func (l stewardSnapshotLine) oldBindings() []stewardOldBinding {
	if len(l.OldValues) > 0 {
		return l.OldValues
	}
	if l.OldValue == "" {
		return nil
	}
	return []stewardOldBinding{{Value: l.OldValue, Source: l.OldSource}}
}

// validateSnapshotLine reports whether line has the shape of a genuine
// steward snapshot record, structurally — not by Kind (most real snapshots
// on disk predate that field) but by the fields every steward snapshot has
// always carried. A file that isn't a steward snapshot at all (e.g. a
// hand-rolled plan/review document that happens to be valid JSONL) decodes
// into an all-zero-value stewardSnapshotLine — no error from json.Unmarshal,
// since unknown fields are ignored and missing ones zero — so this is the
// check that catches it instead: entity_type must actually be outcome or
// workunit, and entity_id/tag_key must actually be present. Called per line
// by rollbackTags, which counts a violation as failed (never skipped) and
// refuses the whole batch if every line fails it.
func validateSnapshotLine(line stewardSnapshotLine) error {
	if line.EntityType != wms.EntityOutcome && line.EntityType != wms.EntityWorkUnit {
		return fmt.Errorf("entity_type must be %q or %q, got %q", wms.EntityOutcome, wms.EntityWorkUnit, line.EntityType)
	}
	if strings.TrimSpace(line.EntityID) == "" {
		return fmt.Errorf("entity_id is required")
	}
	if strings.TrimSpace(line.TagKey) == "" {
		return fmt.Errorf("tag_key is required")
	}
	return nil
}

// entityExists reports whether entityID exists as entityType. Used only to
// tell apart two very different reasons no CURRENT steward-sourced binding
// remains for a rollback line: a human or the classifier legitimately
// overrode it since (benign, entity still there — skip), versus the entity
// itself no longer exists at all (the steward tag, and everything else on
// it, is simply gone). GetEntityTags alone can't distinguish these — it is a
// bare WHERE entity_type=? AND entity_id=? with no existence check, so it
// returns zero rows with a nil error for a nonexistent entity exactly as it
// would for one that legitimately carries no tags for the key.
func entityExists(ctx context.Context, store wms.Store, entityType, entityID string) bool {
	var err error
	if entityType == wms.EntityWorkUnit {
		_, err = store.GetWorkUnit(ctx, entityID)
	} else {
		_, err = store.GetOutcome(ctx, entityID)
	}
	return err == nil
}

// stewardEntityRef names one entity (of either type) to snapshot. Letting a
// single snapshotEntityTags call carry a mix of outcome and workunit refs is
// what makes one logical steward batch — however many entity types it spans —
// a single call: see the ToolSnapshotEntityTags handler.
type stewardEntityRef struct {
	EntityType string
	EntityID   string
}

// snapshotEntityTags records the current binding for tagKey on each ref to
// <batchID>.jsonl in the tag-steward dir (see tagStewardDir), one line per
// entity, and returns the absolute file path. Entities with no current binding
// for the key are recorded with empty old_value/old_source so rollback knows to
// DELETE the steward tag rather than restore a prior value.
//
// A second call reusing a batchID intentionally REPLACES the prior snapshot
// rather than appending to it (see the tool description) — the
// ToolSnapshotEntityTags handler guarantees one call per logical batch (refs
// may span both outcomes and workunits in a single call), so there is never a
// need for a caller to split one batch across multiple calls under the same
// batchID. But that replace must only happen on SUCCESS: writing straight to
// <batchID>.jsonl would let a failure partway through (a GetEntityTags error
// on entity N, a full disk) truncate a prior good snapshot and leave a short,
// useless one in its place while the caller sees an error and reasonably
// assumes nothing happened. So the whole snapshot is built in a private
// temp file (os.CreateTemp, unique per call — also what keeps two concurrent
// calls sharing a batchID from interleaving writes into one file; each writes
// its own, and whichever os.Rename below lands last simply wins, cleanly) and
// only renamed onto <batchID>.jsonl — an atomic replace on the same
// filesystem — once every line is written and the file is closed without
// error. Any failure along the way removes the temp file and leaves whatever
// was already at <batchID>.jsonl (if anything) untouched.
func snapshotEntityTags(ctx context.Context, store wms.Store, refs []stewardEntityRef, tagKey, batchID string) (string, error) {
	dir, err := tagStewardDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, batchID+".jsonl")

	tmp, err := os.CreateTemp(dir, batchID+".jsonl.tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating snapshot tmp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	abort := func(cause error) (string, error) {
		tmp.Close()        //nolint:errcheck // best-effort; we're already failing
		os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of the partial temp file
		return "", cause
	}
	// os.CreateTemp defaults to 0600; widen to a deliberate, fixed 0644 so the
	// rollback snapshot isn't less readable than any other file this package
	// writes to the same directory. NOTE this is Chmod (fchmod(2) on the open
	// fd), which is NOT filtered by umask the way os.Create's open(2) is — on
	// a host with a stricter umask (e.g. 0077), this file ends up 0644 while
	// sibling untagEntity's os.Create-written snapshots in the same directory
	// end up 0600. Content here is entity IDs and tag values, not secrets, so
	// that widening is low severity, but it's real: this is an owned choice
	// of a fixed mode, not umask-equivalent behavior.
	if err := tmp.Chmod(0o644); err != nil {
		return abort(fmt.Errorf("setting permissions on snapshot tmp file %s: %w", tmpPath, err))
	}

	enc := json.NewEncoder(tmp)
	for _, ref := range refs {
		tags, err := store.GetEntityTags(ctx, ref.EntityType, ref.EntityID)
		if err != nil {
			return abort(fmt.Errorf("reading tags for %s %s: %w", ref.EntityType, ref.EntityID, err))
		}
		line := stewardSnapshotLine{
			EntityType: ref.EntityType,
			EntityID:   ref.EntityID,
			TagKey:     tagKey,
			Batch:      batchID,
			Kind:       stewardSnapshotKind,
		}
		// Collect EVERY current binding for tagKey, not just the first — a
		// multi-cardinality key (e.g. work-type) may hold several at once,
		// and a snapshot that only remembers one of them loses the rest on
		// rollback. See stewardSnapshotLine's OldValues doc comment.
		for _, t := range tags {
			if t.TagKey == tagKey {
				line.OldValues = append(line.OldValues, stewardOldBinding{Value: t.TagValue, Source: t.Source})
			}
		}
		if len(line.OldValues) > 0 {
			line.OldValue = line.OldValues[0].Value
			line.OldSource = line.OldValues[0].Source
		}
		if err := enc.Encode(&line); err != nil {
			return abort(fmt.Errorf("writing snapshot line for %s: %w", ref.EntityID, err))
		}
	}
	// Close (not deferred-and-discarded) so a delayed flush error — full disk,
	// an NFS mount going away — surfaces as a failure instead of silently
	// returning success with a short or empty file on disk.
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of the partial temp file
		return "", fmt.Errorf("closing snapshot tmp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup; path (if it existed) is untouched
		return "", fmt.Errorf("finalizing snapshot %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return path, nil //nolint:nilerr // path is usable even if Abs fails
	}
	return abs, nil
}

// EntityTagView is one tag binding as surfaced by wms_getEntityTags: the
// underlying wms.EntityTag fields (tag_key, tag_value, category, source,
// description, applied_at), embedded and promoted flat into the JSON, plus
// Inherited and Origin. Direct bindings have Inherited=false and Origin equal
// to the queried entity's own ID; inherited bindings have Inherited=true and
// Origin set to the parent outcome ID the binding actually lives on.
type EntityTagView struct {
	wms.EntityTag
	Inherited bool   `json:"inherited"`
	Origin    string `json:"origin"`
}

// resolveEntityTags returns entityType/entityID's direct tag bindings plus,
// for a workunit, tags inherited from its parent outcome — mirroring the
// mysql entity_tags_resolved view's per-key-override union (a workunit's own
// binding for a key always shadows the outcome's inherited row for that same
// key) without requiring that view to exist on every backend. entityType must
// already be validated by the caller; existence of entityID is checked via
// GetOutcome/GetWorkUnit inside wms.ResolveEntityTags, so an unknown entity
// surfaces the store's not-found error rather than silently returning an
// empty list. Delegates the actual walk to wms.ResolveEntityTags — the same
// helper close-out enforcement uses — so this view and the enforcement path
// can't drift apart on the inheritance rule again.
func resolveEntityTags(ctx context.Context, store wms.Store, entityType, entityID string) ([]EntityTagView, error) {
	resolved, err := wms.ResolveEntityTags(ctx, store, entityType, entityID)
	if err != nil {
		return nil, err
	}
	out := make([]EntityTagView, len(resolved))
	for i, rt := range resolved {
		out[i] = EntityTagView{EntityTag: rt.EntityTag, Inherited: rt.Inherited, Origin: rt.Origin}
	}
	return out, nil
}

// untagEntity surgically removes one entity's tag binding(s) for tagKey: a
// single (entity, key, value) when tagValue is non-empty, or ALL of the key's
// bindings on the entity when tagValue is empty (a multi-cardinality key such as
// work-type may hold several). It is REVERSIBLE: before deleting, it snapshots
// each removed binding (entity_type, entity_id, tag_key, old_value, old_source)
// to a one-line-per-binding JSONL batch in the tag-steward dir, so the operator
// can restore by re-applying via wms_tagEntity. Returns the snapshot path and
// the number of bindings removed. A no-op (nothing matched) writes no snapshot
// and removes nothing — not an error.
func untagEntity(ctx context.Context, store wms.Store, entityType, entityID, tagKey, tagValue string) (string, int, error) {
	tags, err := store.GetEntityTags(ctx, entityType, entityID)
	if err != nil {
		return "", 0, fmt.Errorf("reading tags for %s %s: %w", entityType, entityID, err)
	}
	// Collect the binding(s) to remove: the matching key, narrowed to one value
	// when tagValue is given.
	var victims []wms.EntityTag
	for _, t := range tags {
		if t.TagKey != tagKey {
			continue
		}
		if tagValue != "" && t.TagValue != tagValue {
			continue
		}
		victims = append(victims, t)
	}
	if len(victims) == 0 {
		return "", 0, nil
	}

	// Snapshot every binding we're about to remove, one line each, BEFORE any
	// delete — so a mid-batch failure still leaves a complete restore record for
	// what was already gone.
	dir, err := tagStewardDir()
	if err != nil {
		return "", 0, err
	}
	batchID := fmt.Sprintf("untag-%s-%s", tagKey, time.Now().UTC().Format("20060102-150405"))
	path := filepath.Join(dir, batchID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		return "", 0, fmt.Errorf("creating untag snapshot %s: %w", path, err)
	}
	enc := json.NewEncoder(f)
	for _, v := range victims {
		line := stewardSnapshotLine{
			EntityType: entityType,
			EntityID:   entityID,
			TagKey:     tagKey,
			OldValue:   v.TagValue,
			OldSource:  v.Source,
			Batch:      batchID,
		}
		if err := enc.Encode(&line); err != nil {
			f.Close() //nolint:errcheck
			return "", 0, fmt.Errorf("writing untag snapshot line for %s:%s: %w", tagKey, v.TagValue, err)
		}
	}
	if err := f.Close(); err != nil {
		return "", 0, fmt.Errorf("closing untag snapshot %s: %w", path, err)
	}

	// Remove the bindings. DeleteEntityTag is idempotent (0 rows = nil), so a
	// concurrent removal is harmless.
	removed := 0
	for _, v := range victims {
		if err := store.DeleteEntityTag(ctx, entityType, entityID, tagKey, v.TagValue); err != nil {
			return "", removed, fmt.Errorf("removing %s:%s from %s %s (snapshot at %s): %w",
				tagKey, v.TagValue, entityType, entityID, path, err)
		}
		removed++
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return path, removed, nil //nolint:nilerr // path is usable even if Abs fails
	}
	return abs, removed, nil
}

// rollbackTags reverts the steward-applied changes recorded in a batch snapshot.
// For each entity it looks at the CURRENT steward-sourced bindings of the
// snapshot's tag key (a multi-cardinality key may have several):
//   - if none remain AND the entity still exists, a human or the classifier
//     has since removed or overridden the steward's value — SKIP (never
//     clobber a non-steward tag). Benign, logged at Info.
//   - if none remain because the entity no longer exists at all, that is a
//     different, more alarming case — counted as notFound, not skipped, so
//     it can't hide in a bucket meant for "someone made a deliberate call."
//   - otherwise delete the steward-applied value(s), then restore EVERY
//     prior value the snapshot recorded (a multi-cardinality key may have
//     held several at once — see stewardSnapshotLine.oldBindings), each
//     with its own recorded source.
//
// Every line is validated structurally first (validateSnapshotLine) — a
// violation counts as failed, is logged with the file and line number, and
// is never treated as skipped, since "this line doesn't look like a
// snapshot record" and "a human overrode this" are not the same finding. If
// EVERY line in the file fails that check, the whole rollback is refused
// with an error instead of returning a confident-looking all-zero-or-all-
// skipped result for what is very likely the wrong file entirely (a plan or
// review document, not a steward snapshot).
//
// One failing entity does not abort the batch — failures are counted and the
// rest proceed. Returns (reverted, skipped, notFound, failed). This grew a
// field (notFound) beyond the tool's original {reverted, skipped, failed}
// shape — additive, so an existing caller reading only the fields it already
// knows about is unaffected.
func rollbackTags(ctx context.Context, store wms.Store, batchID string) (reverted, skipped, notFound, failed int, err error) {
	dir, err := tagStewardDir()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	path := filepath.Join(dir, batchID+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("opening snapshot %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	// Snapshot lines are short, but allow generous room for long entity IDs.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNum := 0
	totalLines, validLines := 0, 0
	for scanner.Scan() {
		lineNum++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		totalLines++
		var line stewardSnapshotLine
		if jerr := json.Unmarshal([]byte(raw), &line); jerr != nil {
			failed++
			slog.Warn("wms-mcp: rollback skipping malformed snapshot line",
				"batch", batchID, "file", path, "line", lineNum, "err", jerr)
			continue
		}
		if verr := validateSnapshotLine(line); verr != nil {
			failed++
			slog.Warn("wms-mcp: rollback skipping structurally invalid snapshot line — this file may not be a steward snapshot",
				"batch", batchID, "file", path, "line", lineNum, "err", verr)
			continue
		}
		validLines++

		// Find the steward-sourced value(s) currently bound for the key. A key may
		// be multi-cardinality (e.g. work-type), so there can be more than one
		// binding; we only ever revert what the steward itself applied. Anything a
		// human or the classifier set (source != "steward") is left untouched.
		tags, gerr := store.GetEntityTags(ctx, line.EntityType, line.EntityID)
		if gerr != nil {
			failed++
			slog.Warn("wms-mcp: rollback failed to read current tags",
				"entity_type", line.EntityType, "entity_id", line.EntityID, "err", gerr)
			continue
		}
		var stewardValues []string
		for _, t := range tags {
			if t.TagKey == line.TagKey && t.Source == "steward" {
				stewardValues = append(stewardValues, t.TagValue)
			}
		}

		// Nothing of ours remains: either the steward value was already removed or
		// overridden (benign — a deliberate human/classifier call, skip), or the
		// entity itself no longer exists (worth surfacing on its own, not the
		// same finding). GetEntityTags can't tell these apart by itself.
		if len(stewardValues) == 0 {
			if !entityExists(ctx, store, line.EntityType, line.EntityID) {
				notFound++
				slog.Warn("wms-mcp: rollback found no such entity",
					"entity_type", line.EntityType, "entity_id", line.EntityID,
					"tag_key", line.TagKey, "batch", batchID)
				continue
			}
			skipped++
			slog.Info("wms-mcp: rollback skipped — no steward-sourced binding remains (human or classifier override since the snapshot)",
				"entity_type", line.EntityType, "entity_id", line.EntityID,
				"tag_key", line.TagKey, "batch", batchID)
			continue
		}

		// Remove the steward-applied value(s) for the key.
		var aerr error
		for _, v := range stewardValues {
			if e := store.DeleteEntityTag(ctx, line.EntityType, line.EntityID, line.TagKey, v); e != nil {
				aerr = e
				break
			}
		}
		// Restore EVERY prior binding the snapshot recorded — a multi-cardinality
		// key may have held several at once, and restoring only one would lose
		// the rest permanently. (An empty result means the key was absent before
		// the steward touched it — the delete above is the whole revert.) Each
		// binding's own recorded source is restored; default to "manual" when the
		// snapshot did not record one.
		if aerr == nil {
			for _, ob := range line.oldBindings() {
				oldSource := ob.Source
				if oldSource == "" {
					oldSource = "manual"
				}
				if e := store.TagEntity(ctx, line.EntityType, line.EntityID, line.TagKey, ob.Value, oldSource, ""); e != nil {
					aerr = e
					break
				}
			}
		}
		if aerr != nil {
			failed++
			slog.Warn("wms-mcp: rollback revert failed",
				"entity_type", line.EntityType, "entity_id", line.EntityID,
				"tag_key", line.TagKey, "err", aerr)
			continue
		}
		reverted++
	}
	if serr := scanner.Err(); serr != nil {
		return reverted, skipped, notFound, failed, fmt.Errorf("reading snapshot %s: %w", path, serr)
	}
	if totalLines > 0 && validLines == 0 {
		return 0, 0, 0, totalLines, fmt.Errorf(
			"refusing to roll back %s: none of its %d line(s) look like a steward snapshot "+
				"(entity_type/entity_id/tag_key missing or malformed) — this is very likely the wrong file",
			path, totalLines)
	}
	return reverted, skipped, notFound, failed, nil
}

// ritualManagedKeys names keys that buildTagManifest's derivation
// (Interview == "skip" && !Required) sorts into EngineManaged, but which a
// documented ritual actually sets by hand — not the engine. `resolution` is
// the confirmed case (LF-RR-1): the close-out ritual (session-protocol.md
// Step 9b / teamster-solo SKILL.md) instructs `wms_tagEntity(resolution,
// achieved|abandoned)`, directly contradicting EngineManaged's own "do not
// set" guidance. The derivation conflates "not offered during the
// vocabulary-setup interview" with "the engine, and only the engine, writes
// it" — true together for `user` (auto-applied by applyCreatorUserTag,
// verified engine-only) but false together for `resolution`. Not
// generalized into the derivation itself: `lifecycle` and `component` also
// skip the interview with required=0 and have no confirmed engine writer in
// this codebase either, but neither has resolution's smoking gun — a
// documented, shipped ritual instructing an agent to set it by hand — so
// they are reported as an open question rather than reclassified on
// suspicion alone (see the deliverable).
var ritualManagedKeys = map[string]bool{"resolution": true}

// splitRitualManaged pulls ritualManagedKeys out of a freshly-built
// TagManifest's EngineManaged list into their own top-level field, without
// altering wms.TagManifest's definition (owned outside this WU's file
// scope): an anonymous embed keeps every existing field verbatim in the JSON
// output and adds exactly one.
func splitRitualManaged(m wms.TagManifest) interface{} {
	var ritual []string
	kept := make([]string, 0, len(m.EngineManaged))
	for _, k := range m.EngineManaged {
		if ritualManagedKeys[k] {
			ritual = append(ritual, k)
			continue
		}
		kept = append(kept, k)
	}
	m.EngineManaged = kept
	return struct {
		wms.TagManifest
		RitualManaged []string `json:"ritualManaged,omitempty"`
	}{TagManifest: m, RitualManaged: ritual}
}

func buildTagManifest(tags []wms.Tag) wms.TagManifest {
	const inlineThreshold = 10

	type keyInfo struct {
		first  wms.Tag
		values []string
	}
	keys := make(map[string]*keyInfo)
	var order []string

	for _, t := range tags {
		if t.Retired {
			continue
		}
		ki, ok := keys[t.Key]
		if !ok {
			ki = &keyInfo{first: t}
			keys[t.Key] = ki
			order = append(order, t.Key)
		} else if t.IsSeed && !ki.first.IsSeed {
			// The is_seed=1 row's metadata (description, scope, cardinality,
			// exclusion group, facetOf, ...) is authoritative for the key.
			// Without this, whichever row the DB happens to return first —
			// non-deterministic across values — wins instead.
			ki.first = t
		}
		if t.Value != "" {
			ki.values = append(ki.values, t.Value)
		}
	}

	m := wms.TagManifest{
		Propose:           make(map[string]wms.ProposeEntry),
		AutoExtract:       make(map[string]string),
		RequiredLifecycle: make(map[string]wms.ProposeEntry),
	}

	for _, key := range order {
		ki := keys[key]
		f := ki.first

		switch f.Interview {
		case "propose":
			entry := wms.ProposeEntry{Desc: f.Description}
			if len(ki.values) <= inlineThreshold {
				entry.Values = ki.values
			} else {
				entry.N = len(ki.values)
			}
			if f.Scope == "outcome" {
				entry.Scope = "outcome"
			}
			if f.ExclusionGroup != "" {
				entry.Exclusive = f.ExclusionGroup
			}
			if f.Cardinality == "single" {
				entry.Cardinality = "single"
			}
			if f.FacetSource != "" {
				entry.FacetOf = f.FacetSource
			}
			m.Propose[key] = entry
			if f.Required {
				m.Required = append(m.Required, key)
			}

		case "auto":
			source := f.AutoExtract
			if source == "" {
				source = "manual"
			}
			m.AutoExtract[key] = source
			if f.Required {
				m.Required = append(m.Required, key)
			}

		case "skip":
			if f.Required {
				// Required lifecycle key: expose full values so the lead knows valid
				// options at dispatch time without a separate drill-down call.
				entry := wms.ProposeEntry{Desc: f.Description, Values: ki.values}
				if f.Cardinality == "single" {
					entry.Cardinality = "single"
				}
				m.RequiredLifecycle[key] = entry
			} else {
				m.EngineManaged = append(m.EngineManaged, key)
			}
		}
	}

	for key, entry := range m.Propose {
		if entry.FacetOf != "" {
			if src, ok := m.RequiredLifecycle[entry.FacetOf]; ok {
				src.FacetKeys = append(src.FacetKeys, key)
				m.RequiredLifecycle[entry.FacetOf] = src
			}
		}
	}

	return m
}

// inlineTagsSchema is the shared `tags` parameter schema for
// wms_createOutcome and wms_createWorkUnit — applies each key immediately
// after creation via the same path as wms_tagEntity, collapsing
// create-then-N-tag-calls into one round-trip. Each value is either a plain
// string (the tag value) or an object carrying a description, matching
// parseInlineTagValue's two accepted shapes.
var inlineTagsSchema = map[string]interface{}{
	"type":        "object",
	"description": "Optional inline tags to apply immediately after creation — one wms_tagEntity call per value, in the same request. Each value is a plain tag-value string, an object {\"value\": \"...\", \"description\": \"...\"}, or an array of either form to apply multiple values to one multi-cardinality key in this same call (e.g. {\"github.issue\": [\"17\", \"11\"]}) — single-cardinality keys should still pass one value, not an array. A description records the classification rubric when introducing a NEW (tagKey, tagValue) — same create-only semantics as wms_tagEntity's `description` (ignored if the value already exists). Applied best-effort after the entity is created: a failed tag never unwinds the create; failures are reported back in the response's `tagErrors`.",
	"additionalProperties": map[string]interface{}{
		"anyOf": []interface{}{
			map[string]interface{}{"type": "string", "description": "The tag value, e.g. \"build\", \"feature\"."},
			map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"value":       map[string]interface{}{"type": "string"},
					"description": map[string]interface{}{"type": "string", "maxLength": 1024},
				},
				"required": []string{"value"},
			},
			map[string]interface{}{
				"type":        "array",
				"description": "Multiple values for one multi-cardinality key in this same call, e.g. {\"github.issue\": [\"17\", \"11\"]}. Do not use for a single-cardinality key — that errors in tagErrors.",
				"items": map[string]interface{}{
					"anyOf": []interface{}{
						map[string]interface{}{"type": "string"},
						map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"value":       map[string]interface{}{"type": "string"},
								"description": map[string]interface{}{"type": "string", "maxLength": 1024},
							},
							"required": []string{"value"},
						},
					},
				},
			},
		},
	},
}

// ToolDefs is the MCP tools/list payload for this server.
// Tool names use underscore form (wms_*) matching the MCP tool name convention,
// but the handler also accepts dot form (wms.*) for backwards compat with the
// stdio binary.
var ToolDefs = []map[string]interface{}{
	{
		"name":        "wms_updateStatus",
		"description": "Transition an entity to a new status. Validates the transition before applying. `done → review` is the reopen edge; follow it with the intended next transition in the same turn.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string"},
				"entityID":   map[string]interface{}{"type": "string"},
				"status":     map[string]interface{}{"type": "string"},
				"notes":      map[string]interface{}{"type": "string", "description": "Optional reason for the transition, recorded on the audit-trail journal row. When omitted, an honest default naming this tool is recorded instead of leaving the column blank."},
			},
			"required": []string{"entityType", "entityID", "status"},
		},
	},
	{
		"name":        "wms_addDependency",
		"description": "Add a blocker→blocked dependency between two entities.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"blockerID":   map[string]interface{}{"type": "string"},
				"blockedID":   map[string]interface{}{"type": "string"},
				"blockerType": map[string]interface{}{"type": "string"},
				"blockedType": map[string]interface{}{"type": "string"},
			},
			"required": []string{"blockerID", "blockedID", "blockerType", "blockedType"},
		},
	},
	{
		"name":        "wms_removeDependency",
		"description": "Remove a blocker→blocked dependency.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"blockerID":   map[string]interface{}{"type": "string"},
				"blockedID":   map[string]interface{}{"type": "string"},
				"blockerType": map[string]interface{}{"type": "string"},
				"blockedType": map[string]interface{}{"type": "string"},
			},
			"required": []string{"blockerID", "blockedID", "blockerType", "blockedType"},
		},
	},
	{
		"name":        "wms_listBlockers",
		"description": "List all dependencies where entityID is the blocked side.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string"},
				"entityID":   map[string]interface{}{"type": "string"},
			},
			"required": []string{"entityType", "entityID"},
		},
	},
	{
		"name":        "wms_listDependents",
		"description": "List all dependencies where entityID is the blocker side.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string"},
				"entityID":   map[string]interface{}{"type": "string"},
			},
			"required": []string{"entityType", "entityID"},
		},
	},
	{
		"name":        "wms_setFocus",
		"description": "Set the focus string for an outcome or workunit.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string"},
				"entityID":   map[string]interface{}{"type": "string"},
				"focus":      map[string]interface{}{"type": "string"},
			},
			"required": []string{"entityType", "entityID", "focus"},
		},
	},
	{
		"name":        ToolSetPhase,
		"description": "Declare the phase of a work unit (e.g. design, build, test, review). Lands the phase on the work unit's currently-open interval as a 'declared' value, which takes precedence over classifier-derived phase. The work unit must be active or in review (in-progress); errors (rather than silently no-op'ing) if in progress but its interval was closed by a sweep. entityType must be 'workunit'.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string", "description": "Must be 'workunit'."},
				"entityID":   map[string]interface{}{"type": "string"},
				"phase":      map[string]interface{}{"type": "string", "description": "One of: design, build, test, review, iterate, admin."},
			},
			"required": []string{"entityType", "entityID", "phase"},
		},
	},
	{
		"name":        "wms_getFocus",
		"description": "Get the focus string for an entity.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string"},
				"entityID":   map[string]interface{}{"type": "string"},
			},
			"required": []string{"entityType", "entityID"},
		},
	},
	{
		"name":        ToolTagEntity,
		"description": "Apply a key:value classifier tag to an entity (outcome or workunit). FIRST call wms_listTags to see the key manifest; for the key you intend to tag, call wms_listTags(tagKey=<key>) to see existing values (unless the manifest already includes them). Reuse an existing (tagKey, tagValue) rather than inventing near-duplicates. The vocabulary is dynamic: applying a NEW (tagKey, tagValue) creates it — pass `description` (max 1024 chars) to record what it means and when to apply it, so the next caller's wms_listTags sees it. An existing tag's description is never overwritten.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType":  map[string]interface{}{"type": "string", "description": "outcome or workunit"},
				"entityID":    map[string]interface{}{"type": "string"},
				"tagKey":      map[string]interface{}{"type": "string", "description": "e.g. phase, work-type, project, priority"},
				"tagValue":    map[string]interface{}{"type": "string", "description": "e.g. build, feature, v0.1, p1"},
				"source":      map[string]interface{}{"type": "string", "description": "manual | classifier | inherited (default manual)"},
				"description": map[string]interface{}{"type": "string", "maxLength": 1024, "description": "Semantics — what this tag means and when to apply it. Stored only when introducing a NEW (tagKey, tagValue); ignored for existing tags. Max 1024 characters."},
			},
			"required": []string{"entityType", "entityID", "tagKey", "tagValue"},
		},
	},
	{
		"name":        ToolListTags,
		"description": "Discover the tag vocabulary. Default (no args): returns a role-shaped manifest — propose (keys to offer the operator, with values/scope/exclusion), autoExtract (key→source map for silent extraction), requiredLifecycle (lifecycle keys the lead MUST apply to every WorkUnit at dispatch time; values included), required (non-lifecycle required keys), engineManaged (skips the setup interview, not required, and the engine is confirmed the only writer — do not set), ritualManaged (also skips the interview, but a documented ritual sets these by hand, e.g. resolution at close-out — set them when the ritual calls for it). Within propose, respect exclusive (at most one key per group) and scope. With tagKey: returns all values for that key. With query: case-insensitive substring search across values and descriptions.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"tagKey": map[string]interface{}{
					"type":        "string",
					"description": "Drill into one key's values instead of the key manifest.",
				},
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Case-insensitive substring search across tag_value and description. Use to find tags matching a concept or check for near-duplicates before creating.",
				},
			},
		},
	},
	{
		"name":        ToolDefineTag,
		"description": "Seed a key into the declared tag vocabulary (is_seed=1) — the runtime equivalent of a yaml `tags:` entry, used during the bootstrap interview to capture vocabulary from the user. Idempotent: re-defining a key converges (category/cardinality refreshed; an existing description is preserved). Omit `values` for create-on-apply keys (e.g. project) whose values are minted on first tag; pass `values` to pre-seed an enumerated set (e.g. priority p0..p3). `description` is capped at 1024 characters.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"tagKey":         map[string]interface{}{"type": "string", "description": "The vocabulary key, e.g. project, priority."},
				"category":       map[string]interface{}{"type": "string", "description": "'context' (durable metadata, inherited down the DAG) or 'lifecycle' (execution tracking). Defaults to context."},
				"cardinality":    map[string]interface{}{"type": "string", "description": "'single' (key holds at most one value per entity; a new value replaces the old) or 'multi' (values accumulate). Defaults to multi."},
				"values":         map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Optional enumerated values to pre-seed (e.g. p0, p1, p2, p3). Omit for create-on-apply keys whose values are minted on first use."},
				"description":    map[string]interface{}{"type": "string", "maxLength": 1024, "description": "Semantics — what this key means and when to apply it. Max 1024 characters."},
				"required":       map[string]interface{}{"type": "boolean", "description": "Optional. When true, marks this key as required on every workunit (set across all the key's values). When false, clears the required flag. Omit to leave the key's required flag unchanged."},
				"scope":          map[string]interface{}{"type": "string", "description": "'outcome' | 'workunit' | '' — where this key should be applied."},
				"exclusionGroup": map[string]interface{}{"type": "string", "description": "Mutual exclusion group slug. Keys sharing a group are exclusive on an entity."},
				"autoExtract":    map[string]interface{}{"type": "string", "description": "'git' | 'env' | '' — source for auto-extraction (skip interview)."},
				"interview":      map[string]interface{}{"type": "string", "description": "'propose' | 'auto' | 'skip' — how this key behaves in the context-tag interview."},
				"facetSource":    map[string]interface{}{"type": "string", "description": "The key this tag is a facet of (e.g. 'work-type'). Facet keys dynamically track their source's values."},
			},
			"required": []string{"tagKey"},
		},
	},
	{
		"name":        ToolRetireTag,
		"description": "DEMOTE a key out of the declared vocabulary (is_seed=0). NON-DESTRUCTIVE: the tag rows and ALL existing entity_tags bindings survive, and the key can be re-promoted later via wms_defineTag or the yaml vocabulary. Only user-vocabulary keys (e.g. project, priority, scope, team, release) can be retired; writer-coupled lifecycle keys (phase, work-type, resolution, lifecycle) are owned by migrations and the store REJECTS retiring them with an error.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"tagKey": map[string]interface{}{"type": "string", "description": "The vocabulary key to demote, e.g. scope."},
			},
			"required": []string{"tagKey"},
		},
	},
	{
		"name":        ToolDescribeTag,
		"description": "Refine the description of an EXISTING tag value, overwriting it in place. The description is the classification rubric — the 'when to apply' guidance the steward and classifier read. Works for ANY key, INCLUDING system-managed lifecycle keys (work-type, phase, resolution, lifecycle) that wms_defineTag refuses to touch. Contrast: wms_tagEntity only records a description when a (tagKey, tagValue) is first created (never overwrites); wms_defineTag manages vocabulary/required at the KEY level and rejects lifecycle keys. Use this to sharpen an ambiguous value description so classification becomes obvious. Errors if the (tagKey, tagValue) does not already exist. `description` is capped at 1024 characters.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"tagKey":      map[string]interface{}{"type": "string", "description": "The existing tag's key, e.g. work-type."},
				"tagValue":    map[string]interface{}{"type": "string", "description": "The existing tag's value, e.g. bug."},
				"description": map[string]interface{}{"type": "string", "maxLength": 1024, "description": "The new description — the classification rubric for this value. Replaces the prior description in place. Max 1024 characters."},
			},
			"required": []string{"tagKey", "tagValue", "description"},
		},
	},
	{
		"name":        ToolUntagEntity,
		"description": "Surgically remove a tag binding from ONE entity, reversibly. With tagValue set, removes that single (entity, key, value) binding; omit tagValue to remove ALL of the key's bindings on the entity (a multi-cardinality key like work-type may hold several). BEFORE deleting, it snapshots each removed binding to a JSONL batch in the tag-steward dir and returns {removed, snapshot}, so the operator can restore by re-applying via wms_tagEntity. Contrast: `teamster tags delete-value` is value-WIDE and destructive (cascades to every entity bound to that value); wms_rollbackTags only reverts steward-sourced rows from a prior snapshot. This is the sanctioned single-binding untag — use it instead of a raw DELETE. A no-op (nothing matched) removes nothing and writes no snapshot.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string", "description": "outcome or workunit"},
				"entityID":   map[string]interface{}{"type": "string"},
				"tagKey":     map[string]interface{}{"type": "string", "description": "The tag key to remove, e.g. work-type."},
				"tagValue":   map[string]interface{}{"type": "string", "description": "Optional. The specific value to remove; omit to remove ALL of the key's bindings on this entity."},
			},
			"required": []string{"entityType", "entityID", "tagKey"},
		},
	},
	{
		"name":        ToolGetEntityTags,
		"description": "Read the tags bound to one entity (outcome or workunit): direct bindings PLUS, for a workunit, tags inherited from its parent outcome (outcomes do not inherit further). Each row is tag_key/tag_value/category/source/description/applied_at plus inherited (bool) and origin (the entity_id the binding actually lives on — the queried entity itself when direct, the parent outcome's ID when inherited). A workunit's own binding for a key always shadows the outcome's inherited row for that key. Read-only — does not modify wms_getOutcome or wms_getWorkUnit's response shape. Returns an empty array (not an error) when the entity has no tags; errors if the entity does not exist.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string", "description": "outcome or workunit"},
				"entityID":   map[string]interface{}{"type": "string"},
			},
			"required": []string{"entityType", "entityID"},
		},
	},
	{
		"name":        ToolGetHistory,
		"description": "Get audit history for an entity, ordered newest first.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string"},
				"entityID":   map[string]interface{}{"type": "string"},
				"limit":      map[string]interface{}{"type": "integer", "description": "Maximum entries to return (default 50)."},
			},
			"required": []string{"entityType", "entityID"},
		},
	},
	{
		"name":        ToolGetTimeline,
		"description": "Get temporal event records for an entity, ordered newest first. Each record shows a state the entity was in, with started_at, ended_at, and duration_ms.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string", "description": "outcome or workunit"},
				"entityID":   map[string]interface{}{"type": "string"},
				"limit":      map[string]interface{}{"type": "integer", "description": "Maximum records to return (default 50)."},
			},
			"required": []string{"entityType", "entityID"},
		},
	},

	// --- v2 tools ---

	{
		"name":        ToolCreateOutcome,
		"description": "Create a new outcome. Top-level outcomes have no parent; nested outcomes set parentOutcomeIDs to establish DAG edges.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":               map[string]interface{}{"type": "string"},
				"title":            map[string]interface{}{"type": "string"},
				"description":      map[string]interface{}{"type": "string"},
				"parentOutcomeIDs": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Parent outcome ID(s). Omit for top-level (root) outcomes."},
				"status":           map[string]interface{}{"type": "string", "description": "Initial status (default: pending)"},
				"tags":             inlineTagsSchema,
			},
			"required": []string{"id", "title"},
		},
	},
	{
		"name":        ToolGetOutcome,
		"description": "Retrieve a single outcome by ID. Returns all fields including parent_ids.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string"}},
			"required":   []string{"id"},
		},
	},
	{
		"name":        ToolListOutcomes,
		"description": "List outcomes. Omit parentOutcomeID for root outcomes; set it to list children. Use tagFilters for AND-filtered tag lookup. Use status to filter by lifecycle state; the special value \"open\" returns non-terminal outcomes (pending, active, review, blocked, on_hold). Use query for case-insensitive substring search on title and description — combine with status=\"open\" to find existing outcomes matching a focus.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"parentOutcomeID": map[string]interface{}{"type": "string", "description": "Filter to children of this outcome. Omit or empty for root outcomes."},
				"tagFilters":      map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}, "description": "Key-value tag filters (AND semantics). E.g. {\"project\": \"teamster\"}."},
				"status":          map[string]interface{}{"type": "string", "description": "Filter by status. Pass a specific status (pending, active, review, done, blocked, on_hold, abandoned) or \"open\" to return all non-terminal outcomes."},
				"query":           map[string]interface{}{"type": "string", "description": "Case-insensitive substring search on outcome title and description. Combine with status=\"open\" to find resumable outcomes."},
			},
		},
	},
	{
		"name":        ToolUpdateOutcomeStatus,
		"description": "Transition an outcome to a new status. Validates against the state machine and role permissions. `done → review` is the reopen edge; follow it with the intended next transition in the same turn.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":     map[string]interface{}{"type": "string"},
				"status": map[string]interface{}{"type": "string"},
				"notes":  map[string]interface{}{"type": "string", "description": "Optional reason for the transition, recorded on the audit-trail journal row. When omitted, an honest default naming this tool is recorded instead of leaving the column blank."},
			},
			"required": []string{"id", "status"},
		},
	},
	{
		"name":        ToolRenameOutcome,
		"description": "Rename an outcome — updates its title. No state-machine validation; use to correct naming mistakes or scope changes.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":    map[string]interface{}{"type": "string"},
				"title": map[string]interface{}{"type": "string", "description": "New title for the outcome."},
			},
			"required": []string{"id", "title"},
		},
	},
	{
		"name":        ToolAddOutcomeParent,
		"description": "Add a parent→child DAG edge between two existing outcomes, without recreating either one. Use this to decompose a large outcome after the fact: create the child outcomes first, then attach each to its parent with this tool. Rejects a self-loop (parentID == childID) and any edge that would create a cycle (childID is already an ancestor of parentID) — fix by choosing a different parent or removing the conflicting edge first with wms_removeOutcomeParent. Idempotent: re-adding an edge that already exists succeeds without creating a duplicate. Both outcomes must already exist — a typo’d ID errors here rather than silently no-op’ing.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"parentID": map[string]interface{}{"type": "string", "description": "ID of the parent outcome."},
				"childID":  map[string]interface{}{"type": "string", "description": "ID of the child outcome."},
			},
			"required": []string{"parentID", "childID"},
		},
	},
	{
		"name":        ToolRemoveOutcomeParent,
		"description": "Remove a parent→child DAG edge between two outcomes. Idempotent: removing an edge that does not exist succeeds as a no-op — use this to correct a mistaken wms_addOutcomeParent call or detach a child being re-parented elsewhere.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"parentID": map[string]interface{}{"type": "string", "description": "ID of the parent outcome."},
				"childID":  map[string]interface{}{"type": "string", "description": "ID of the child outcome."},
			},
			"required": []string{"parentID", "childID"},
		},
	},
	{
		"name":        ToolCreateWorkUnit,
		"description": "Create a new work unit under an outcome.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":          map[string]interface{}{"type": "string"},
				"title":       map[string]interface{}{"type": "string"},
				"outcomeID":   map[string]interface{}{"type": "string"},
				"description": map[string]interface{}{"type": "string"},
				"agentID":     map[string]interface{}{"type": "string", "description": "Agent to assign (omit for unassigned)"},
				"status":      map[string]interface{}{"type": "string", "description": "Initial status (default: pending)"},
				"tags":        inlineTagsSchema,
				"brief":       map[string]interface{}{"type": "string", "description": "Optional full markdown dispatch brief. Returned by wms_getWorkUnit and wms_claimWorkUnit, but not in list responses."},
			},
			"required": []string{"id", "title", "outcomeID"},
		},
	},
	{
		"name":        ToolGetWorkUnit,
		"description": "Retrieve a single work unit by ID.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string"}},
			"required":   []string{"id"},
		},
	},
	{
		"name":        ToolListWorkUnits,
		"description": "List work units under an outcome. When ready=true, returns only non-terminal work units with no incomplete blockers.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"outcomeID": map[string]interface{}{"type": "string"},
				"ready":     map[string]interface{}{"type": "boolean", "description": "If true, return only work units that are not terminal and have no incomplete blockers."},
			},
			"required": []string{"outcomeID"},
		},
	},
	{
		"name":        ToolUpdateWorkUnitStatus,
		"description": "Transition a work unit to a new status. Validates against the state machine and role permissions. `done → review` is the reopen edge; follow it with the intended next transition in the same turn.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":     map[string]interface{}{"type": "string"},
				"status": map[string]interface{}{"type": "string"},
				"notes":  map[string]interface{}{"type": "string", "description": "Optional reason for the transition, recorded on the audit-trail journal row. When omitted, an honest default naming this tool is recorded instead of leaving the column blank."},
			},
			"required": []string{"id", "status"},
		},
	},
	{
		"name":        ToolRenameWorkUnit,
		"description": "Rename a work unit — updates its title. No state-machine validation; use to correct naming mistakes or scope changes.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":    map[string]interface{}{"type": "string"},
				"title": map[string]interface{}{"type": "string", "description": "New title for the work unit."},
			},
			"required": []string{"id", "title"},
		},
	},
	{
		"name":        ToolAssignWorkUnit,
		"description": "Assign a work unit to an agent (lead-initiated). Sets agent_id but does not change status.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":      map[string]interface{}{"type": "string"},
				"agentID": map[string]interface{}{"type": "string"},
			},
			"required": []string{"id", "agentID"},
		},
	},
	{
		"name":        ToolClaimWorkUnit,
		"description": "Agent self-assigns a work unit, atomically transitioning pending → active. AgentID is read from _meta. On success, returns the full work unit — including its dispatch brief and tags — as the assignment payload. focus_interval reports intent, not a confirmed result: \"requested\" means this call triggered hookd's attempt to open a focus interval for cost attribution — asynchronous, best-effort, and only made at all when a hook server is configured; \"unchanged\" means adopting an already-active unit or an idempotent re-claim, which requests nothing new. hookd may still decline the attempt (e.g. two agents claiming within the same few seconds, where identity is ambiguous) — when it does, a follow-up warning is queued telling you to call wms_setFocus yourself.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string"}},
			"required":   []string{"id"},
		},
	},
	{
		"name":        ToolDeliverResult,
		"description": "Submit a work unit's deliverable. From active, transitions it to review. Callable again while already in review — a redelivery — which appends a new deliverable row without changing status (wms_listDeliverables returns every row; take the last as current). The caller must own the work unit (or be the lead — no agent_type).",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":      map[string]interface{}{"type": "string", "description": "Work unit ID."},
				"summary": map[string]interface{}{"type": "string", "description": "Headline summary of the result, <=1KB."},
				"result":  map[string]interface{}{"type": "string", "description": "Full markdown deliverable."},
				"artifact_paths": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional file paths produced by this work.",
				},
			},
			"required": []string{"id", "summary", "result"},
		},
	},
	{
		"name":        ToolListDeliverables,
		"description": "Read back the deliverable rows submitted via wms_deliverResult for a work unit, oldest first. Each row has summary, result, artifact_paths, agent_id, session_id, created_at. Redelivery is allowed — for a single answer, take the last row.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityID": map[string]interface{}{"type": "string", "description": "Work unit ID."},
				"limit":    map[string]interface{}{"type": "integer", "description": "Maximum rows to return (default 50)."},
			},
			"required": []string{"entityID"},
		},
	},
	{
		"name":        ToolClassifyEntity,
		"description": "Trigger tag classification on an entity. Runs the rule-based classifier and applies derived tags with source=classifier.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string", "description": "outcome or workunit"},
				"entityID":   map[string]interface{}{"type": "string"},
			},
			"required": []string{"entityType", "entityID"},
		},
	},
	{
		"name":        ToolListRelated,
		"description": "Find outcomes and workunits that may relate to new work — dangling (adoptable) entities or terminal (potential rework). Use at session startup to detect overlap with prior work before creating new entities.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query":           map[string]interface{}{"type": "string", "description": "Title substring to match against."},
				"tagFilters":      map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}, "description": "Key-value tag filters (AND semantics). E.g. {\"product\": \"teamster\"}."},
				"includeTerminal": map[string]interface{}{"type": "boolean", "description": "Also return done/archived entities (default false)."},
				"staleHours":      map[string]interface{}{"type": "integer", "description": "Consider entities with no interval activity in this many hours as stale (default 4)."},
			},
		},
	},
	{
		"name":        ToolSearch,
		"description": "Search across outcomes, workunits, and session focus strings for a query substring. Returns granular []Hit rows (one per matching entity or focus string), each carrying full attribution (user, host, session, agent) and the reason(s) it matched. Multi-operator by default: with no user/host filter, spans every operator and host reporting to the store.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query":   map[string]interface{}{"type": "string", "description": "Substring to search for across titles, descriptions, tags, and focus strings."},
				"type":    map[string]interface{}{"type": "string", "description": "Comma list restricting which surfaces are searched: outcomes,workunits,focus,all. Defaults to all."},
				"user":    map[string]interface{}{"type": "string", "description": "Filter to hits attributed to this user."},
				"host":    map[string]interface{}{"type": "string", "description": "Filter to hits attributed to this host."},
				"status":  map[string]interface{}{"type": "string", "description": "Filter to hits whose session is in this status (active, idle, closed)."},
				"session": map[string]interface{}{"type": "string", "description": "Filter to hits from this session ID."},
				"tag":     map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Exact \"key=value\" tag filters, ANDed together. A focus-string hit with no backing entity cannot satisfy a tag filter and is dropped when one is given."},
				"since":   map[string]interface{}{"type": "string", "description": "Only return hits whose session was last active at or after this time. Accepts a relative duration (e.g. \"72h\") or an absolute RFC3339 timestamp."},
				"limit":   map[string]interface{}{"type": "integer", "description": "Maximum hits to return. Omit or 0 for unlimited."},
			},
			"required": []string{"query"},
		},
	},
	{
		"name":        ToolSnapshotEntityTags,
		"description": "Tag steward rollback plumbing: capture the current binding(s) of one tag key across a set of entities to a JSONL snapshot (<batchID>.jsonl in the tag-steward snapshot directory under the install's var dir), BEFORE applying steward tag changes. Records each entity's pre-change value(s) (or none if the key was absent — a multi-cardinality key like work-type may have held several at once, and ALL of them are recorded, not just one) so wms_rollbackTags can later restore all of them. Returns the absolute snapshot path. ONE LOGICAL BATCH IS ONE CALL: if the batch spans both outcomes and workunits, use `entities` (mixed types in one call) instead of calling this twice with the same batchID — a second call reusing a batchID REPLACES the snapshot file rather than adding to it, silently discarding the first call's rollback data. Batch ID convention: steward-<key>-<YYYYMMDD-HHMMSS>.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entities": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "object", "properties": map[string]interface{}{"entityType": map[string]interface{}{"type": "string", "description": "outcome or workunit"}, "entityID": map[string]interface{}{"type": "string"}}, "required": []string{"entityType", "entityID"}},
					"description": "Entities to snapshot, spanning any mix of outcomes and workunits — use this when the batch is not all one entity type. Mutually exclusive with entityType + entityIDs; supply one or the other, never both.",
				},
				"entityType": map[string]interface{}{"type": "string", "description": "outcome or workunit. Use with entityIDs when the batch is entirely one entity type; for a mixed-type batch use `entities` instead."},
				"entityIDs":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Entity IDs (all of entityType), each a non-blank string, whose current binding(s) for tagKey are snapshotted. Mutually exclusive with entities."},
				"tagKey":     map[string]interface{}{"type": "string", "description": "The single tag key being changed (one snapshot per key)."},
				"batchID":    map[string]interface{}{"type": "string", "description": "Batch identifier; the snapshot file is <batchID>.jsonl. Convention: steward-<key>-<YYYYMMDD-HHMMSS>. Reusing a batchID across calls REPLACES the prior snapshot — one logical batch must be one call (use `entities` for a mixed-type batch)."},
			},
			"required": []string{"tagKey", "batchID"},
		},
	},
	{
		"name":        ToolRollbackTags,
		"description": "Tag steward rollback: revert the steward-applied tag changes recorded in a batch snapshot (<batchID>.jsonl in the tag-steward snapshot directory under the install's var dir). For each entity, every current binding whose source is still 'steward' is deleted, and every prior binding the snapshot recorded is restored (a multi-cardinality key like work-type may have held several at once — all of them are restored, not just one). If no steward-sourced binding remains AND the entity still exists, a human or the classifier has since overridden it — skipped, never clobbered. If the entity named in a line no longer exists at all, that is reported separately as notFound rather than folded into skipped. Each line is validated structurally before being processed (entity_type must be outcome or workunit; entity_id and tag_key non-empty) — a line that fails this is counted as failed, not skipped, and if EVERY line in the file fails it the whole rollback is refused with an error (this is very likely the wrong file, e.g. a plan/review document rather than a steward snapshot) instead of returning a confident-looking result. Returns {reverted, skipped, notFound, failed} counts.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"batchID": map[string]interface{}{"type": "string", "description": "The batch identifier to roll back; reads <batchID>.jsonl from the snapshot dir."},
			},
			"required": []string{"batchID"},
		},
	},
	{
		"name":        ToolAddRelation,
		"description": "Record a typed relationship from new work to prior work. Reads as a sentence: fromID <kind> toID (e.g. 'auth-v2 remediates auth-v1'). Use at intake, when creating work that exists because of earlier work. For an antecedent that is not tracked in WMS (pre-WMS code, work never given its own Outcome), set toType='external' and put a short description in toID — the work still counts toward rework reporting. Call wms_listRelationKinds for the vocabulary; kind is validated against it and the call is rejected with the valid list on an unknown value.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"kind":     map[string]interface{}{"type": "string", "description": "One of the values from wms_listRelationKinds."},
				"fromType": map[string]interface{}{"type": "string", "description": "'outcome' or 'workunit'. The NEW work."},
				"fromID":   map[string]interface{}{"type": "string"},
				"toType":   map[string]interface{}{"type": "string", "description": "'outcome', 'workunit', or 'external'. The PRIOR work."},
				"toID":     map[string]interface{}{"type": "string", "description": "Entity ID, or free text when toType='external'."},
				"note":     map[string]interface{}{"type": "string", "description": "Optional: why this relationship holds."},
			},
			"required": []string{"kind", "fromType", "fromID", "toType", "toID"},
		},
	},
	{
		"name":        ToolRemoveRelation,
		"description": "Remove a typed relationship between two work items. Identity is the same five fields wms_addRelation used to create it (kind, fromType, fromID, toType, toID) — the uq_rel key makes it unambiguous. Idempotent: removing a relation that does not exist succeeds as a no-op.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"kind":     map[string]interface{}{"type": "string"},
				"fromType": map[string]interface{}{"type": "string", "description": "'outcome' or 'workunit'."},
				"fromID":   map[string]interface{}{"type": "string"},
				"toType":   map[string]interface{}{"type": "string", "description": "'outcome', 'workunit', or 'external'."},
				"toID":     map[string]interface{}{"type": "string"},
			},
			"required": []string{"kind", "fromType", "fromID", "toType", "toID"},
		},
	},
	{
		"name":        ToolListRelations,
		"description": "List typed relations touching one entity. Returns both directions by default so an agent can ask 'what reworked this' (direction=to) and 'what did this rework' (direction=from) in one call.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string", "description": "'outcome' or 'workunit'."},
				"entityID":   map[string]interface{}{"type": "string"},
				"direction":  map[string]interface{}{"type": "string", "description": "'from', 'to', or 'both' (default 'both')."},
				"kind":       map[string]interface{}{"type": "string", "description": "Optional: filter to a single relation kind."},
			},
			"required": []string{"entityType", "entityID"},
		},
	},
	{
		"name":        ToolListRelationKinds,
		"description": "List the relation_kinds vocabulary: kind, whether it counts toward rework tax (taxable), its miss_class (code/design/spec), whether it participates in cycle-checking/transitive closure (lineage — false for kinds like discovered-during and duplicate-of that record circumstance rather than derivation), and a description. Call this before wms_addRelation — kind is validated against exactly this set.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
}

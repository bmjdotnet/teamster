// Package wms implements the WMS MCP tool handlers. It is transport-agnostic:
// no imports from internal/server or cmd/hookd.
package wms

import (
	"bufio"
	"context"
	"encoding/json"
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
}

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
// client on every event. Used as a fallback when _meta.session_id is absent
// (Claude Code MCP clients don't send _meta).
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

// HandleToolCall dispatches a tools/call request to the appropriate store method.
// meta is captured from params._meta and stored on mutations.
func HandleToolCall(store wms.Store, eng wms.Engine, rawParams json.RawMessage) (Result, *CallError) {
	var p callParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return Result{}, &CallError{Code: -32602, Message: "invalid params"}
	}

	if p.Meta.SessionID == "" {
		p.Meta.SessionID = readCurrentSessionID()
	}

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

		var oldStatus string
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
		eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
			EntityType: entityType,
			EntityID:   entityID,
			OldStatus:  oldStatus,
			NewStatus:  newStatus,
			SessionID:  p.Meta.SessionID,
			AgentName:  p.Meta.AgentType,
			Host:       p.Meta.Host,
		})
		msg := fmt.Sprintf("Updated %s %s: %s → %s", entityType, entityID, oldStatus, newStatus)
		if entityType == wms.EntityOutcome {
			msg += wms.FormatCloseoutWarnings(wms.CloseoutWarnings(ctx, store, entityID, newStatus))
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
		entityType := strArg("entityType")
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
		rec, err := store.GetOpenEventRecord(ctx, wms.EntityWorkUnit, entityID)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		if rec == nil {
			// No open interval (e.g. the work unit is not active / not yet
			// transitioned). Graceful no-op — there is nothing to annotate yet, and
			// declaring a phase on a non-running unit is not an error the caller can
			// act on. Report it plainly instead of failing the call.
			return TextResult(fmt.Sprintf("No open interval for workunit %s; phase %q not applied (transition it active first)", entityID, phase)), nil
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
			return JSONResult(manifest), nil
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

	case ToolGetHistory:
		limit := 50
		if v, ok := p.Arguments["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		entries, err := store.GetJournalEntries(ctx, strArg("entityType"), strArg("entityID"), limit)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
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
		eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
			EntityType: wms.EntityOutcome, EntityID: entityID,
			OldStatus: oldStatus, NewStatus: newStatus,
			SessionID: p.Meta.SessionID, AgentName: p.Meta.AgentType, Host: p.Meta.Host,
		})
		// Close-out backstop: surface discipline misses inline in the response.
		// Advisory only — the transition already succeeded above.
		msg := fmt.Sprintf("Updated outcome %s: %s → %s", entityID, oldStatus, newStatus)
		msg += wms.FormatCloseoutWarnings(wms.CloseoutWarnings(ctx, store, entityID, newStatus))
		return TextResult(msg), nil

	// --- v2 WorkUnit tools ---

	case ToolCreateWorkUnit:
		wu := &wms.WorkUnit{
			ID:            strArg("id"),
			OutcomeID:     strArg("outcomeID"),
			Title:         strArg("title"),
			Description:   strArg("description"),
			AgentID:       strArg("agentID"),
			Status:        strArgDefault("status", wms.StatusPending),
			OriginHost:    p.Meta.Host,
			OriginSession: p.Meta.SessionID,
			OriginAgent:   p.Meta.AgentType,
		}
		if err := store.CreateWorkUnit(ctx, wu); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		store.OpenEventRecord(ctx, wms.EntityWorkUnit, wu.ID, wu.Status, p.Meta.SessionID, p.Meta.AgentType, p.Meta.Host) //nolint:errcheck
		applyCreatorUserTag(ctx, store, wms.EntityWorkUnit, wu.ID)
		// Dispatch-time reminder (W3): a freshly created work unit carries no
		// tags, so any required keys are by definition missing. Surface them as
		// an advisory hint appended to the success response — never a block. If
		// the required-keys lookup itself fails, the creation still succeeds;
		// the reminder is observability, not a gate.
		warnings := missingRequiredTagWarnings(ctx, store, wms.EntityWorkUnit, wu.ID)
		if len(warnings) > 0 {
			return JSONResult(map[string]interface{}{
				"message":  "Created work unit: " + wu.Title,
				"warnings": warnings,
			}), nil
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
		eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
			EntityType: wms.EntityWorkUnit, EntityID: entityID,
			OldStatus: oldStatus, NewStatus: newStatus,
			SessionID: p.Meta.SessionID, AgentName: p.Meta.AgentType, Host: p.Meta.Host,
		})
		return TextResult(fmt.Sprintf("Updated workunit %s: %s → %s", entityID, oldStatus, newStatus)), nil

	case ToolAssignWorkUnit:
		if err := store.AssignWorkUnit(ctx, strArg("id"), strArg("agentID")); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return TextResult("Assigned work unit " + strArg("id") + " to " + strArg("agentID")), nil

	case ToolClaimWorkUnit:
		id := strArg("id")
		agentID := p.Meta.AgentType
		if err := store.ClaimWorkUnit(ctx, id, agentID); err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		eng.OnStatusChange(ctx, wms.StatusChange{ //nolint:errcheck
			EntityType: wms.EntityWorkUnit, EntityID: id,
			OldStatus: wms.StatusPending, NewStatus: wms.StatusActive,
			SessionID: p.Meta.SessionID, AgentName: p.Meta.AgentType, Host: p.Meta.Host,
		})
		return TextResult(fmt.Sprintf("Claimed work unit %s for %s", id, agentID)), nil

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

	case ToolSnapshotEntityTags, "wms.snapshotEntityTags":
		entityType := strArg("entityType")
		tagKey := strArg("tagKey")
		batchID := strArg("batchID")
		if entityType == "" || tagKey == "" || batchID == "" {
			return Result{}, &CallError{Code: -32602, Message: "entityType, tagKey, and batchID are required"}
		}
		var entityIDs []string
		if ids, ok := p.Arguments["entityIDs"].([]interface{}); ok {
			for _, v := range ids {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					entityIDs = append(entityIDs, strings.TrimSpace(s))
				}
			}
		}
		if len(entityIDs) == 0 {
			return Result{}, &CallError{Code: -32602, Message: "entityIDs must be a non-empty array"}
		}
		path, err := snapshotEntityTags(ctx, store, entityType, entityIDs, tagKey, batchID)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(map[string]interface{}{"path": path}), nil

	case ToolRollbackTags, "wms.rollbackTags":
		batchID := strArg("batchID")
		if batchID == "" {
			return Result{}, &CallError{Code: -32602, Message: "batchID is required"}
		}
		reverted, skipped, failed, err := rollbackTags(ctx, store, batchID)
		if err != nil {
			return Result{}, &CallError{Code: -32000, Message: err.Error()}
		}
		return JSONResult(map[string]interface{}{
			"reverted": reverted, "skipped": skipped, "failed": failed,
		}), nil

	default:
		return Result{}, &CallError{Code: -32601, Message: "unknown tool: " + p.Name}
	}
}

// missingRequiredTagWarnings returns one advisory warning per required tag key
// that has no binding on the entity. It is best-effort: if either the required-
// keys lookup or the entity-tags lookup fails, it returns no warnings (the
// caller's primary operation must not be blocked by an observability hint).
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
	tags, err := store.GetEntityTags(ctx, entityType, entityID)
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

// stewardSnapshotLine is one JSONL record in a steward batch snapshot. It
// captures enough of the pre-change binding to restore it: an empty old_value
// (and old_source) means the tag key was absent before the steward applied it.
// new_value is left empty at snapshot time — the apply has not happened yet.
type stewardSnapshotLine struct {
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	TagKey     string `json:"tag_key"`
	OldValue   string `json:"old_value"`
	OldSource  string `json:"old_source"`
	NewValue   string `json:"new_value,omitempty"`
	Batch      string `json:"batch"`
}

// snapshotEntityTags records the current binding for tagKey on each entity to
// <batchID>.jsonl in the tag-steward dir (see tagStewardDir), one line per
// entity, and returns the absolute file path. Entities with no current binding
// for the key are recorded with empty old_value/old_source so rollback knows to
// DELETE the steward tag rather than restore a prior value.
func snapshotEntityTags(ctx context.Context, store wms.Store, entityType string, entityIDs []string, tagKey, batchID string) (string, error) {
	dir, err := tagStewardDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, batchID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("creating snapshot %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck
	enc := json.NewEncoder(f)
	for _, id := range entityIDs {
		tags, err := store.GetEntityTags(ctx, entityType, id)
		if err != nil {
			return "", fmt.Errorf("reading tags for %s %s: %w", entityType, id, err)
		}
		line := stewardSnapshotLine{
			EntityType: entityType,
			EntityID:   id,
			TagKey:     tagKey,
			Batch:      batchID,
		}
		for _, t := range tags {
			if t.TagKey == tagKey {
				line.OldValue = t.TagValue
				line.OldSource = t.Source
				break
			}
		}
		if err := enc.Encode(&line); err != nil {
			return "", fmt.Errorf("writing snapshot line for %s: %w", id, err)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, nil //nolint:nilerr // path is usable even if Abs fails
	}
	return abs, nil
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
//   - if none remain, a human or the classifier has since removed or overridden
//     the steward's value — SKIP (never clobber a non-steward tag).
//   - otherwise delete the steward-applied value(s), then, when the key held a
//     prior value before the steward touched it (old_value non-empty), restore
//     that value with its prior source.
//
// One failing entity does not abort the batch — failures are counted and the
// rest proceed. Returns (reverted, skipped, failed).
func rollbackTags(ctx context.Context, store wms.Store, batchID string) (reverted, skipped, failed int, err error) {
	dir, err := tagStewardDir()
	if err != nil {
		return 0, 0, 0, err
	}
	path := filepath.Join(dir, batchID+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("opening snapshot %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	// Snapshot lines are short, but allow generous room for long entity IDs.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var line stewardSnapshotLine
		if jerr := json.Unmarshal([]byte(raw), &line); jerr != nil {
			failed++
			slog.Warn("wms-mcp: rollback skipping malformed snapshot line",
				"batch", batchID, "err", jerr)
			continue
		}

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

		// Nothing of ours remains: either the steward value was already removed, or
		// a human/classifier has overridden it since. Respect that — skip.
		if len(stewardValues) == 0 {
			skipped++
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
		// If the steward had overwritten a prior value, restore it. (When old_value
		// is empty the tag was absent before — the delete above is the whole
		// revert.) The prior source is restored from the snapshot; default to
		// "manual" when the snapshot did not record one.
		if aerr == nil && line.OldValue != "" {
			oldSource := line.OldSource
			if oldSource == "" {
				oldSource = "manual"
			}
			aerr = store.TagEntity(ctx, line.EntityType, line.EntityID, line.TagKey, line.OldValue, oldSource, "")
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
		return reverted, skipped, failed, fmt.Errorf("reading snapshot %s: %w", path, serr)
	}
	return reverted, skipped, failed, nil
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
		}
		if t.Value != "" {
			ki.values = append(ki.values, t.Value)
		}
	}

	m := wms.TagManifest{
		Propose:     make(map[string]wms.ProposeEntry),
		AutoExtract: make(map[string]string),
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
			m.Propose[key] = entry

		case "auto":
			source := f.AutoExtract
			if source == "" {
				source = "manual"
			}
			m.AutoExtract[key] = source

		case "skip":
			m.EngineManaged = append(m.EngineManaged, key)
		}

		if f.Required {
			m.Required = append(m.Required, key)
		}
	}

	return m
}

// ToolDefs is the MCP tools/list payload for this server.
// Tool names use underscore form (wms_*) matching the MCP tool name convention,
// but the handler also accepts dot form (wms.*) for backwards compat with the
// stdio binary.
var ToolDefs = []map[string]interface{}{
	{
		"name":        "wms_updateStatus",
		"description": "Transition an entity to a new status. Validates the transition before applying.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string"},
				"entityID":   map[string]interface{}{"type": "string"},
				"status":     map[string]interface{}{"type": "string"},
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
		"description": "Declare the phase of a work unit (e.g. design, build, test, review). Lands the phase on the work unit's currently-open interval as a 'declared' value, which takes precedence over classifier-derived phase. The work unit must be active (have an open interval). entityType must be 'workunit'.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string", "description": "Must be 'workunit'."},
				"entityID":   map[string]interface{}{"type": "string"},
				"phase":      map[string]interface{}{"type": "string", "description": "e.g. design, build, test, review, rework"},
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
		"description": "Apply a key:value classifier tag to an entity (outcome or workunit). FIRST call wms_listTags to see the key manifest; for the key you intend to tag, call wms_listTags(tagKey=<key>) to see existing values (unless the manifest already includes them). Reuse an existing (tagKey, tagValue) rather than inventing near-duplicates. The vocabulary is dynamic: applying a NEW (tagKey, tagValue) creates it — pass `description` to record what it means and when to apply it, so the next caller's wms_listTags sees it. An existing tag's description is never overwritten.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType":  map[string]interface{}{"type": "string", "description": "outcome or workunit"},
				"entityID":    map[string]interface{}{"type": "string"},
				"tagKey":      map[string]interface{}{"type": "string", "description": "e.g. phase, work-type, project, priority"},
				"tagValue":    map[string]interface{}{"type": "string", "description": "e.g. build, feature, v0.1, p1"},
				"source":      map[string]interface{}{"type": "string", "description": "manual | classifier | inherited (default manual)"},
				"description": map[string]interface{}{"type": "string", "description": "Semantics — what this tag means and when to apply it. Stored only when introducing a NEW (tagKey, tagValue); ignored for existing tags."},
			},
			"required": []string{"entityType", "entityID", "tagKey", "tagValue"},
		},
	},
	{
		"name":        ToolListTags,
		"description": "Discover the tag vocabulary. Default (no args): returns a role-shaped manifest — propose (keys to offer the operator, with values/scope/exclusion), autoExtract (key→source map for silent extraction), required (must be set on workunits before close-out), engineManaged (do not touch). Within propose, respect exclusive (at most one key per group) and scope. With tagKey: returns all values for that key. With query: case-insensitive substring search across values and descriptions.",
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
		"description": "Seed a key into the declared tag vocabulary (is_seed=1) — the runtime equivalent of a yaml `tags:` entry, used during the bootstrap interview to capture vocabulary from the user. Idempotent: re-defining a key converges (category/cardinality refreshed; an existing description is preserved). Omit `values` for create-on-apply keys (e.g. project) whose values are minted on first tag; pass `values` to pre-seed an enumerated set (e.g. priority p0..p3).",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"tagKey":      map[string]interface{}{"type": "string", "description": "The vocabulary key, e.g. project, priority."},
				"category":    map[string]interface{}{"type": "string", "description": "'context' (durable metadata, inherited down the DAG) or 'lifecycle' (execution tracking). Defaults to context."},
				"cardinality": map[string]interface{}{"type": "string", "description": "'single' (key holds at most one value per entity; a new value replaces the old) or 'multi' (values accumulate). Defaults to multi."},
				"values":      map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Optional enumerated values to pre-seed (e.g. p0, p1, p2, p3). Omit for create-on-apply keys whose values are minted on first use."},
				"description":    map[string]interface{}{"type": "string", "description": "Semantics — what this key means and when to apply it."},
				"required":       map[string]interface{}{"type": "boolean", "description": "Optional. When true, marks this key as required on every workunit (set across all the key's values). When false, clears the required flag. Omit to leave the key's required flag unchanged."},
				"scope":          map[string]interface{}{"type": "string", "description": "'outcome' | 'workunit' | '' — where this key should be applied."},
				"exclusionGroup": map[string]interface{}{"type": "string", "description": "Mutual exclusion group slug. Keys sharing a group are exclusive on an entity."},
				"autoExtract":    map[string]interface{}{"type": "string", "description": "'git' | 'env' | '' — source for auto-extraction (skip interview)."},
				"interview":      map[string]interface{}{"type": "string", "description": "'propose' | 'auto' | 'skip' — how this key behaves in the context-tag interview."},
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
		"description": "Refine the description of an EXISTING tag value, overwriting it in place. The description is the classification rubric — the 'when to apply' guidance the steward and classifier read. Works for ANY key, INCLUDING system-managed lifecycle keys (work-type, phase, resolution, lifecycle) that wms_defineTag refuses to touch. Contrast: wms_tagEntity only records a description when a (tagKey, tagValue) is first created (never overwrites); wms_defineTag manages vocabulary/required at the KEY level and rejects lifecycle keys. Use this to sharpen an ambiguous value description so classification becomes obvious. Errors if the (tagKey, tagValue) does not already exist.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"tagKey":      map[string]interface{}{"type": "string", "description": "The existing tag's key, e.g. work-type."},
				"tagValue":    map[string]interface{}{"type": "string", "description": "The existing tag's value, e.g. bug."},
				"description": map[string]interface{}{"type": "string", "description": "The new description — the classification rubric for this value. Replaces the prior description in place."},
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
		"description": "List outcomes. Omit parentOutcomeID for root outcomes; set it to list children. Use tagFilters for AND-filtered tag lookup. Use status to filter by lifecycle state; the special value \"open\" returns non-terminal outcomes (pending, active, review, blocked). Use query for case-insensitive substring search on title and description — combine with status=\"open\" to find existing outcomes matching a focus.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"parentOutcomeID": map[string]interface{}{"type": "string", "description": "Filter to children of this outcome. Omit or empty for root outcomes."},
				"tagFilters":      map[string]interface{}{"type": "object", "additionalProperties": map[string]interface{}{"type": "string"}, "description": "Key-value tag filters (AND semantics). E.g. {\"project\": \"teamster\"}."},
				"status":          map[string]interface{}{"type": "string", "description": "Filter by status. Pass a specific status (pending, active, review, done, blocked) or \"open\" to return all non-terminal outcomes (status != done)."},
				"query":           map[string]interface{}{"type": "string", "description": "Case-insensitive substring search on outcome title and description. Combine with status=\"open\" to find resumable outcomes."},
			},
		},
	},
	{
		"name":        ToolUpdateOutcomeStatus,
		"description": "Transition an outcome to a new status. Validates against the state machine and role permissions.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":     map[string]interface{}{"type": "string"},
				"status": map[string]interface{}{"type": "string"},
			},
			"required": []string{"id", "status"},
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
		"description": "Transition a work unit to a new status. Validates against the state machine and role permissions.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id":     map[string]interface{}{"type": "string"},
				"status": map[string]interface{}{"type": "string"},
			},
			"required": []string{"id", "status"},
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
		"description": "Agent self-assigns a work unit, atomically transitioning pending → active. AgentID is read from _meta.",
		"inputSchema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string"}},
			"required":   []string{"id"},
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
		"name":        ToolSnapshotEntityTags,
		"description": "Tag steward rollback plumbing: capture the current binding of one tag key across a set of entities to a JSONL snapshot (<batchID>.jsonl in the tag-steward snapshot directory under the install's var dir), BEFORE applying steward tag changes. Records each entity's pre-change value (or empty if the key was absent) so wms_rollbackTags can later revert. Returns the absolute snapshot path. Batch ID convention: steward-<key>-<YYYYMMDD-HHMMSS>.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"entityType": map[string]interface{}{"type": "string", "description": "outcome or workunit"},
				"entityIDs":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Entity IDs whose current binding for tagKey is snapshotted."},
				"tagKey":     map[string]interface{}{"type": "string", "description": "The single tag key being changed (one snapshot per key)."},
				"batchID":    map[string]interface{}{"type": "string", "description": "Batch identifier; the snapshot file is <batchID>.jsonl. Convention: steward-<key>-<YYYYMMDD-HHMMSS>."},
			},
			"required": []string{"entityType", "entityIDs", "tagKey", "batchID"},
		},
	},
	{
		"name":        ToolRollbackTags,
		"description": "Tag steward rollback: revert the steward-applied tag changes recorded in a batch snapshot (<batchID>.jsonl in the tag-steward snapshot directory under the install's var dir). For each entity, if the current binding's source is still 'steward' it is reverted (deleted if the key was absent before, or restored to its prior value); if a human or the classifier has since overridden it (source != 'steward'), it is skipped. Returns {reverted, skipped, failed} counts.",
		"inputSchema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"batchID": map[string]interface{}{"type": "string", "description": "The batch identifier to roll back; reads <batchID>.jsonl from the snapshot dir."},
			},
			"required": []string{"batchID"},
		},
	},
}

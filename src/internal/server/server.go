// Package server implements the hookd HTTP event receiver.
package server

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/hook"
	mcpactivity "github.com/bmjdotnet/teamster/internal/mcp/activity"
	mcpwms "github.com/bmjdotnet/teamster/internal/mcp/wms"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/redact"
	"github.com/bmjdotnet/teamster/internal/store"
	storemysql "github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/version"
	"github.com/bmjdotnet/teamster/internal/web"
	"github.com/bmjdotnet/teamster/internal/wms"
	"github.com/prometheus/client_golang/prometheus"
)

const maxBodySize = 1 << 20 // 1 MB

const maxSSESubscribers = 100

const writeTimeout = 60 * time.Second

// eventBus fans out new event payloads to active SSE subscribers.
type eventBus struct {
	mu          sync.RWMutex
	subscribers map[uint64]chan []byte
	nextID      uint64
}

// subscribe registers a new subscriber and returns its ID and receive channel.
// Returns (0, nil) when the subscriber limit has been reached.
func (b *eventBus) subscribe() (uint64, chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subscribers) >= maxSSESubscribers {
		return 0, nil
	}
	id := b.nextID
	b.nextID++
	ch := make(chan []byte, 64)
	b.subscribers[id] = ch
	return id, ch
}

// unsubscribe removes a subscriber by ID.
func (b *eventBus) unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscribers, id)
}

// publish sends payload to every subscriber; drops silently if the channel is full.
func (b *eventBus) publish(payload []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers {
		select {
		case ch <- payload:
		default:
		}
	}
}

// mcpIdentity holds session/agent identity stashed from a PreToolUse hook event
// for injection into the subsequent MCP call that lacks those fields.
type mcpIdentity struct {
	SessionID string
	AgentType string
	ExpiresAt time.Time
}

// Server receives hook telemetry events via HTTP and writes them to a JSONL log.
type Server struct {
	cfg             config.Config
	logFile         *os.File
	mu              sync.Mutex
	bus             eventBus
	wmsDB           *sql.DB // read-only, for the /wms dashboard
	wmsStore        wms.Store
	wmsEng          *wms.EngineImpl
	obsStore        store.Store // new unified store (nil when store package not ready)
	sessions        *observability.SessionTracker
	metrics         *observability.Metrics
	promRegistry    *prometheus.Registry
	sweepStop       chan struct{}
	telemetry       *telemetryQueue
	telemetryAgents *agentCache
	telemetryCtx    context.Context
	telemetryCancel context.CancelFunc
	subagentNames   subagentNameMap
	pendingMCPMu    sync.Mutex
	pendingMCPIdent map[string]mcpIdentity // key: "toolSuffix:entityID"
	focusNudge      focusNudgeCache
}

// NewServer opens (or creates) the JSONL log file in append mode and returns a ready Server.
// If the store DSN is unset the /wms route will show an empty state.
func NewServer(cfg config.Config) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("opening log file %s: %w", cfg.LogFile, err)
	}

	// Prometheus registry and standard metric vecs.
	reg := observability.Registry
	metrics := observability.NewMetrics(reg)

	sessions := observability.NewSessionTracker(
		cfg.Host,
		cfg.SessionTimeout,
		cfg.SessionSweepInterval,
		func(reason string) {
			metrics.ActiveSessionsPruned.With(prometheus.Labels{"reason": reason}).Inc()
		},
	)

	// Register custom collectors.
	reg.MustRegister(
		observability.NewBridgeCollector(sessions),
		observability.NewEntitiesCollector(),
		observability.NewSweepCollector(filepath.Join(cfg.DataDir, "sweep-state.json")),
	)

	sweepStop := make(chan struct{})
	sessions.StartSweeper(sweepStop)

	s := &Server{
		cfg:             cfg,
		logFile:         f,
		sessions:        sessions,
		metrics:         metrics,
		promRegistry:    reg,
		sweepStop:       sweepStop,
		pendingMCPIdent: make(map[string]mcpIdentity),
	}
	s.bus.subscribers = make(map[uint64]chan []byte)

	if cfg.StoreDSN.Driver == config.StoreDriverMySQL {
		ms, storeErr := storemysql.New(cfg.StoreDSN.Primary)
		if storeErr == nil {
			s.obsStore = ms
			s.wmsStore = ms
			if initialCounts, hydErr := ms.CountEntitiesByStatus(context.Background()); hydErr == nil {
				observability.HydrateCounts(initialCounts)
			}
			s.wmsEng = wms.NewEngine(ms, nil)
			s.wmsEng.AddObserver(observability.NewInProcessObserver(observability.IncrementEntityCounts))
			s.wmsDB = ms.DB()
			reg.MustRegister(
				observability.NewUsageCollector(s.wmsDB),
				observability.NewTagCountsCollector(s.wmsDB),
				observability.NewAttributionCollector(s.wmsDB),
				observability.NewDependenciesCollector(s.wmsDB),
				observability.NewCostCollector(s.wmsDB),
				observability.NewIntervalPhaseCostCollector(s.wmsDB),
				observability.NewBacklogCollector(s.wmsDB),
			)

			tctx, tcancel := context.WithCancel(context.Background())
			s.telemetryCtx = tctx
			s.telemetryCancel = tcancel
			s.telemetry = &telemetryQueue{
				ch:       make(chan TelemetryRow, 1000),
				fallback: filepath.Join(cfg.DataDir, "telemetry-fallback.jsonl"),
			}
			s.telemetryAgents = &agentCache{cache: make(map[string]string)}
			go s.startTelemetryWriter()
		} else {
			slog.Error("WMS store unavailable — /wms dashboard disabled", "error", storeErr)
		}
	}

	s.startReaper()

	return s, nil
}

// RegisterRoutes attaches the server's handlers to mux. In read-only mode
// the MCP, telemetry, and drain write endpoints return 403; /event and all
// read/dashboard/SSE routes remain available.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	timed := func(h http.HandlerFunc) http.Handler {
		return http.TimeoutHandler(http.HandlerFunc(h), writeTimeout, "request timeout")
	}

	mux.Handle("/event", timed(s.handleEvent))
	mux.Handle("/health", timed(s.handleHealth))
	mux.HandleFunc("/events/stream", s.handleSSE)
	mux.Handle("/api/events", timed(s.handleEventsAPI))

	if s.cfg.ReadOnly {
		reject := func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "read-only mode", http.StatusForbidden)
		}
		mux.Handle("/mcp/activity", timed(reject))
		mux.Handle("/mcp/wms", timed(reject))
		mux.Handle("/telemetry", timed(reject))
		mux.Handle("/focus-timeline", timed(reject))
		mux.Handle("/wms/api/drain", timed(reject))
	} else {
		mux.Handle("/mcp/activity", timed(s.handleMCPActivity))
		mux.Handle("/mcp/wms", timed(s.handleMCPWMS))
		mux.Handle("/telemetry", timed(s.handleTelemetry))
		mux.Handle("/focus-timeline", timed(s.handleFocusTimeline))
		mux.Handle("/wms/api/drain", timed(web.HandleDrainAPI(s.obsStore)))
	}

	mux.Handle("/wms/cost-flow", timed(web.HandleCostFlowPage))
	mux.Handle("/wms/api/cost-flow", timed(web.HandleCostFlowAPI(s.wmsDB)))
	mux.Handle("/wms/tags", timed(web.HandleTagsPage))
	mux.Handle("/wms/api/tags", timed(web.HandleTagsAPI(s.wmsDB)))
	mux.Handle("/wms", timed(web.HandleWMS(s.wmsDB)))
	mux.Handle("/", timed(web.HandleDashboard))
	mux.Handle("/metrics", timed(observability.Handler(s.promRegistry).ServeHTTP))
}

// SecurityHeaders wraps a handler to inject standard security response headers.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' https://unpkg.com https://d3js.org https://cdn.jsdelivr.net; "+
				"style-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

// Close releases the JSONL log file, WMS database connections, and stops the
// session sweeper goroutine.
func (s *Server) Close() error {
	if s.telemetryCancel != nil {
		s.telemetryCancel()
		time.Sleep(100 * time.Millisecond)
	}
	close(s.sweepStop)
	if s.obsStore != nil {
		s.obsStore.Close() //nolint:errcheck
	}
	if s.wmsStore != nil && s.wmsStore != s.obsStore {
		if c, ok := s.wmsStore.(io.Closer); ok {
			c.Close() //nolint:errcheck
		}
	}
	// MySQL shares the *sql.DB handle between obsStore and wmsDB; Close via obsStore above.
	return s.logFile.Close()
}

// handleEvent accepts POST /event, builds a JSONL record, and appends it to logFile.
// Per ERRATA E-05: one typed decode at the top; all new branches use struct field access.
func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Typed decode — E-05: all new branches use struct fields.
	var event hook.HookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// Map decode for the existing JSONL/SSE pipeline (untouched).
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// Orphan dispatch warning: SendMessage PreToolUse with no WMS entity refs.
	if event.HookEventName == "PreToolUse" && event.ToolName == "SendMessage" {
		refs := s.sessions.GetEntityRefsForSession(event.SessionID)
		hasRef := false
		for _, r := range refs {
			if len(r.OutcomeIDs) > 0 || len(r.WorkunitIDs) > 0 {
				hasRef = true
				break
			}
		}
		if !hasRef {
			data["_warn_msg"] = "no WMS task — orphan dispatch"
		}
	}

	// Subagent name resolution: when the lead spawns an Agent with a name,
	// record the mapping; when subagent events arrive, resolve to the name.
	s.resolveSubagentName(event, data)

	record := s.buildRecord(data)

	line, err := json.Marshal(record)
	if err != nil {
		http.Error(w, "marshal error", http.StatusInternalServerError)
		return
	}
	line = append(line, '\n')

	s.mu.Lock()
	_, werr := s.logFile.Write(line)
	s.mu.Unlock()
	if werr != nil {
		s.metrics.EventWriteErrorsTotal.With(prometheus.Labels{
			"reason": "jsonl_write",
		}).Inc()
		http.Error(w, "write error", http.StatusInternalServerError)
		return
	}

	// Publish formatted HTML to SSE subscribers.
	html := web.FormatEventHTML(record)
	s.bus.publish([]byte(html))

	// Observability branches — keyed on the typed event struct + raw map for enriched fields.
	s.dispatchObservability(event, data)

	// Focus-absent nudge: on PreToolUse, check whether (session, agent) has an
	// open focus interval. If not, return additionalContext asking the agent to
	// call wms_setFocus. Nudge up to nudgeMaxCount times then stop.
	// Skip activity MCP tools (always called first) and ToolSearch (needed to
	// load deferred tools like wms_setFocus) — nudging on these is unreasonable.
	resp := map[string]interface{}{"status": "ok"}
	if event.HookEventName == "PreToolUse" && s.obsStore != nil &&
		!strings.HasPrefix(event.ToolName, "mcp__activity__") &&
		event.ToolName != "ToolSearch" {
		agent := agentNameFor(event.AgentType)
		if msg, shouldNudge := s.focusNudge.check(event.SessionID, agent, func() bool {
			return s.hasOpenFocusInterval(event.SessionID, agent)
		}); shouldNudge {
			resp["additionalContext"] = msg
		}
	}

	// Activity/team-dispatch nudge for UserPromptSubmit: return the same
	// instruction text the hub Go client injects locally so remote clients
	// (e.g. the Python thin client) receive it from hookd and can pass it
	// through. The hub Go client generates its own copy from the constants
	// directly and ignores this field — no double-injection on the hub.
	// Always return both halves: hookd cannot observe a remote session's
	// solo/team marker (it is client-local state, never sent over the wire),
	// so remote UserPromptSubmit always receives team context. A solo remote
	// will see the dispatch mandate; this is the least-harm default since the
	// common remote case is team and the text is guidance, not enforcement.
	if event.HookEventName == "UserPromptSubmit" {
		resp["additionalContext"] = hook.ACTIVITY_INSTRUCTION + hook.TEAM_DISPATCH_INSTRUCTION
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// dispatchObservability runs the four observability branches from SPEC §6.4
// and §7.1. Called after JSONL write so the write path is never blocked by
// store calls. data is the full enriched event map (may contain _usage etc.).
func (s *Server) dispatchObservability(event hook.HookEvent, data map[string]interface{}) {
	ctx := context.Background()
	agentType := event.AgentType

	// Upsert the session entry on every event that carries identity.
	if event.SessionID != "" {
		switch event.HookEventName {
		case "PreToolUse", "UserPromptSubmit":
			isNew := s.sessions.Upsert(event.SessionID, agentType)
			if isNew {
				s.metrics.SessionsTotal.With(prometheus.Labels{
					"host": s.cfg.Host,
				}).Inc()
			}
			s.metrics.HookEventsTotal.With(prometheus.Labels{
				"event":      event.HookEventName,
				"host":       s.cfg.Host,
				"agent_name": agentNameFor(agentType),
			}).Inc()
			if event.HookEventName == "PreToolUse" && event.ToolName != "" {
				s.metrics.ToolCallsTotal.With(prometheus.Labels{
					"tool":       event.ToolName,
					"host":       s.cfg.Host,
					"agent_name": agentNameFor(agentType),
					"status":     "",
				}).Inc()
			}
		case "Stop":
			s.metrics.HookEventsTotal.With(prometheus.Labels{
				"event":      "Stop",
				"host":       s.cfg.Host,
				"agent_name": agentNameFor(agentType),
			}).Inc()
		case "PostToolUse":
			s.metrics.HookEventsTotal.With(prometheus.Labels{
				"event":      "PostToolUse",
				"host":       s.cfg.Host,
				"agent_name": agentNameFor(agentType),
			}).Inc()
		}
	}

	switch event.HookEventName {
	case "PreToolUse":
		toolInput := normaliseToolInput(event.ToolInput)
		switch event.ToolName {
		// WMS label population uses PreToolUse (not PostToolUse) because Claude Code
		// does not fire PostToolUse for successful MCP calls. The caller-provides-ID
		// design means the id is in the tool INPUT, so PreToolUse has all data needed.
		// Claude Code emits MCP tool names as mcp__<server>__<tool>; match the wire form.
		//
		// v3 entities (Outcome/WorkUnit) only. The sessions-table last-write-wins
		// pointer (SetSession*) is intentionally NOT written for v2 — the in-memory
		// tracker feeds the bridge gauge and wms_intervals (kind='focus') feeds the
		// allocator, so the sessions pointer is redundant for v3.
		case mcpwms.MCPToolCreateOutcome:
			if id := hook.StrField(toolInput, "id", 64); id != "" {
				s.focusNudge.setFocus(event.SessionID, agentNameFor(agentType))
				s.sessions.SetOutcome(event.SessionID, agentType, id)
				if s.obsStore != nil {
					key := store.SessionKey{SessionID: event.SessionID, AgentName: agentNameFor(agentType)}
					go func() {
						_ = s.obsStore.OpenFocusInterval(ctx, key, wms.EntityOutcome, id)
					}()
				}
				s.stashMCPIdentity(mcpwms.ToolCreateOutcome, id, event.SessionID, agentType)
			}
		case mcpwms.MCPToolCreateWorkUnit:
			if id := hook.StrField(toolInput, "id", 64); id != "" {
				s.focusNudge.setFocus(event.SessionID, agentNameFor(agentType))
				s.sessions.SetWorkUnit(event.SessionID, agentType, id)
				if s.obsStore != nil {
					key := store.SessionKey{SessionID: event.SessionID, AgentName: agentNameFor(agentType)}
					go func() {
						_ = s.obsStore.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, id)
					}()
				}
				s.stashMCPIdentity(mcpwms.ToolCreateWorkUnit, id, event.SessionID, agentType)
			}
		case mcpwms.MCPToolUpdateOutcomeStatus:
			if id := hook.StrField(toolInput, "id", 64); id != "" {
				s.stashMCPIdentity(mcpwms.ToolUpdateOutcomeStatus, id, event.SessionID, agentType)
			}
		case mcpwms.MCPToolUpdateWorkUnitStatus:
			// Register the workunit ref when an agent transitions it to active so
			// the session accumulates a cost attribution target even if the agent
			// didn't create the workunit itself.
			if newStatus := hook.StrField(toolInput, "status", 64); newStatus == wms.StatusActive {
				if id := hook.StrField(toolInput, "id", 64); id != "" {
					s.sessions.SetWorkUnit(event.SessionID, agentType, id)
					if s.obsStore != nil {
						key := store.SessionKey{SessionID: event.SessionID, AgentName: agentNameFor(agentType)}
						go func() {
							_ = s.obsStore.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, id)
						}()
					}
				}
			}
			if id := hook.StrField(toolInput, "id", 64); id != "" {
				s.stashMCPIdentity(mcpwms.ToolUpdateWorkUnitStatus, id, event.SessionID, agentType)
			}
		// wms_setFocus is the explicit per-agent focus signal: the agent
		// declares the entity it is now working on, which is what attributes
		// its token cost to that entity. Open a focus interval (the store
		// guards against re-opening the same entity, and closes the prior one).
		case mcpwms.MCPToolSetFocus:
			entityType := hook.StrField(toolInput, "entityType", 64)
			id := hook.StrField(toolInput, "entityID", 64)
			if id != "" {
				s.focusNudge.setFocus(event.SessionID, agentNameFor(agentType))
				switch entityType {
				case wms.EntityOutcome:
					s.sessions.SetOutcome(event.SessionID, agentType, id)
				case wms.EntityWorkUnit:
					s.sessions.SetWorkUnit(event.SessionID, agentType, id)
				}
				if s.obsStore != nil && (entityType == wms.EntityOutcome || entityType == wms.EntityWorkUnit) {
					key := store.SessionKey{SessionID: event.SessionID, AgentName: agentNameFor(agentType)}
					go func() {
						_ = s.obsStore.OpenFocusInterval(ctx, key, entityType, id)
					}()
				}
				s.stashMCPIdentity(mcpwms.ToolSetFocus, id, event.SessionID, agentType)
			}
		case mcpwms.MCPToolTagEntity:
			if id := hook.StrField(toolInput, "entityID", 64); id != "" {
				s.stashMCPIdentity(mcpwms.ToolTagEntity, id, event.SessionID, agentType)
			}
		case mcpwms.MCPToolUpdateStatus:
			if id := hook.StrField(toolInput, "entityID", 64); id != "" {
				s.stashMCPIdentity(mcpwms.ToolUpdateStatus, id, event.SessionID, agentType)
			}
		case mcpwms.MCPToolClaimWorkUnit:
			if id := hook.StrField(toolInput, "id", 64); id != "" {
				s.stashMCPIdentity(mcpwms.ToolClaimWorkUnit, id, event.SessionID, agentType)
			}
		// wms_assignWorkUnit is NOT a trigger per SPEC §4.4 / ERRATA E-04.
		case "mcp__activity__reportActivity":
			s.metrics.ActivityCallsTotal.With(prometheus.Labels{
				"method":     "reportActivity",
				"host":       s.cfg.Host,
				"agent_name": agentNameFor(agentType),
			}).Inc()
		case "mcp__activity__setOverallIntent":
			s.metrics.ActivityCallsTotal.With(prometheus.Labels{
				"method":     "setOverallIntent",
				"host":       s.cfg.Host,
				"agent_name": agentNameFor(agentType),
			}).Inc()
		case "mcp__activity__completeActivity":
			s.metrics.ActivityCallsTotal.With(prometheus.Labels{
				"method":     "completeActivity",
				"host":       s.cfg.Host,
				"agent_name": agentNameFor(agentType),
			}).Inc()
		}

	case "PostToolUse":
		// WMS label population moved to PreToolUse — PostToolUse does not fire for
		// successful MCP tool calls in Claude Code (only PreToolUse + PostToolUseFailure).

	case "WMSStatusChange":
		// Path 2: cross-process WMSStatusChange POST from wms-mcp subprocess.
		// Use data (the raw map) — hook.HookEvent has no wms_* fields so the
		// typed struct decode silently drops them.
		entityType := hook.StrField(data, "wms_entity_type", 64)
		oldStatus := hook.StrField(data, "wms_old_status", 64)
		newStatus := hook.StrField(data, "wms_new_status", 64)
		if entityType != "" {
			observability.IncrementEntityCounts(entityType, oldStatus, newStatus)
			s.metrics.WMSStatusChangesTotal.With(prometheus.Labels{
				"entity_type": entityType,
				"old_status":  oldStatus,
				"new_status":  newStatus,
			}).Inc()
		}
		// When a v3 entity reaches the terminal state, close the agent's focus
		// interval for THAT entity so post-completion cost stops attributing to
		// finished work. Entity-scoped (not a blanket close of whatever the
		// agent has open): completing a child WorkUnit must not orphan a lead's
		// parent-Outcome focus, which would dump all subsequent coordination
		// cost into the unallocated bucket. CloseFocusIntervalForEntity is a
		// 0-row no-op unless the open interval is exactly this entity — covering
		// both "agent focused elsewhere" and "nothing open" (e.g. a
		// rollup-cascade completion whose agent had no open interval). Mirrors
		// the reaper's entity-scoped CloseIntervalsOnTerminalEntities.
		//
		// wms_agent_name is the bare AgentType from the MCP call's p.Meta; the
		// open path keys intervals with the agentNameFor() form ("@<name>", ""
		// for lead). Normalise here so the close matches the open's key exactly.
		if s.obsStore != nil && newStatus == wms.StatusDone &&
			(entityType == wms.EntityOutcome || entityType == wms.EntityWorkUnit) {
			sid := hook.StrField(data, "wms_session_id", 64)
			agent := agentNameFor(hook.StrField(data, "wms_agent_name", 64))
			eid := hook.StrField(data, "wms_entity_id", 128)
			if sid != "" && eid != "" {
				key := store.SessionKey{SessionID: sid, AgentName: agent}
				go func() {
					_ = s.obsStore.CloseFocusIntervalForEntity(ctx, key, entityType, eid)
				}()
			}
		}
		// W2 soft enforcement: warn (don't block) when a workunit reaches done
		// without a tag for every required key. Unconditional — distinct from the
		// hard store-level reject gated by RequireTagsOnDone. The transition has
		// already succeeded; this is observability only.
		if s.obsStore != nil && newStatus == wms.StatusDone && entityType == wms.EntityWorkUnit {
			id := hook.StrField(data, "wms_entity_id", 128)
			if id != "" {
				agent := agentNameFor(hook.StrField(data, "wms_agent_name", 64))
				warnSID := hook.StrField(data, "wms_session_id", 64)
				// Detached like the focus-interval close above: keep the two
				// store reads off the WMSStatusChange POST response path.
				go func() {
					s.warnMissingRequiredTags(ctx, entityType, id, agent, warnSID)
				}()
			}
		}

	case "Stop":
		affectedKeys := s.sessions.CloseSession(event.SessionID)
		s.subagentNames.clearSession(event.SessionID)
		s.focusNudge.clearSession(event.SessionID)
		if s.obsStore != nil && len(affectedKeys) > 0 {
			// Parse the event timestamp as the fallback for ResolveSessionEnd.
			var stopFallback time.Time
			if ts, ok := data["ts"].(string); ok && ts != "" {
				if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
					stopFallback = parsed.UTC()
				}
			}
			go func() {
				closeTime, err := s.obsStore.ResolveSessionEnd(ctx, event.SessionID, stopFallback)
				if err != nil {
					slog.Warn("resolve session end", "session", event.SessionID, "error", err)
					closeTime = stopFallback
					if closeTime.IsZero() {
						closeTime = time.Now().UTC()
					}
				}
				for _, k := range affectedKeys {
					_ = s.obsStore.UpsertSession(ctx, store.Session{
						SessionID: k.SessionID,
						AgentName: k.AgentName,
						Host:      s.cfg.Host,
						Username:  s.cfg.User,
						Status:    store.SessionStatusClosed,
						LastSeen:  closeTime,
					})
					n, drainErr := s.obsStore.CloseSessionIntervals(ctx, k.SessionID, k.AgentName, closeTime)
					if drainErr != nil {
						slog.Warn("drain session intervals",
							"session", k.SessionID, "agent", k.AgentName, "error", drainErr)
					} else if n > 0 {
						slog.Info("drained open intervals on Stop",
							"session", k.SessionID, "agent", k.AgentName, "closed", n)
					}
				}
			}()
		}
		// Per-entity cost is written by the allocator (rollup → cost_rollup) from
		// token_ledger ⋈ wms_intervals (kind='focus') — no Stop-time per-entity write here.
	}
}

// warnMissingRequiredTags implements W2 soft close-out enforcement: it loads the
// required tag keys and the workunit's bound tags, and if any required key has no
// tag it logs a warning and emits a WMSCloseOutWarning JSONL record so the gap is
// visible in feed and the dashboards. The status transition is not affected — the
// hard reject is the store's job, gated by RequireTagsOnDone. Best-effort: any
// store error is logged and swallowed so the handler is never broken.
func (s *Server) warnMissingRequiredTags(ctx context.Context, entityType, id, agentName, sessionID string) {
	required, err := s.obsStore.ListRequiredTagKeys(ctx)
	if err != nil {
		slog.Warn("closeout warning: list required tag keys", "entity_id", id, "error", err)
		return
	}
	if len(required) == 0 {
		return
	}
	tags, err := s.obsStore.GetEntityTags(ctx, entityType, id)
	if err != nil {
		slog.Warn("closeout warning: get entity tags", "entity_id", id, "error", err)
		return
	}
	missing := missingRequiredKeys(required, tags)
	if len(missing) == 0 {
		return
	}
	slog.Warn("workunit done without required tags", "entity_id", id, "missing", missing)
	s.emitCloseOutWarning(id, agentName, missing, sessionID)
}

// missingRequiredKeys returns the required keys for which present holds no tag,
// preserving the order of required. Pure helper — unit-testable in isolation.
func missingRequiredKeys(required []string, present []wms.EntityTag) []string {
	have := make(map[string]bool, len(present))
	for _, t := range present {
		have[t.TagKey] = true
	}
	var missing []string
	for _, k := range required {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	return missing
}

// emitCloseOutWarning appends a WMSCloseOutWarning record to the JSONL log and
// publishes it to SSE subscribers, using the same enrich/write/publish path as
// handleEvent so feed and the dashboard render it like any other event. The
// _warn_msg field surfaces a bold [WARN] line; entity_id and missing carry the
// structured detail for dashboards.
func (s *Server) emitCloseOutWarning(id, agentName string, missing []string, sessionID string) {
	if sessionID == "" {
		sessionID = "wms"
	}
	payload := map[string]interface{}{
		"hook_event_name": "WMSCloseOutWarning",
		"session_id":      sessionID,
		"_host":           s.cfg.Host,
		"_agent_name":     agentName,
		"ts":              time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"_warn_msg":       fmt.Sprintf("workunit %s done without required tags: %s", id, strings.Join(missing, ", ")),
	}
	record := s.buildRecord(payload)
	record["entity_id"] = id
	record["missing"] = missing

	line, err := json.Marshal(record)
	if err != nil {
		slog.Warn("closeout warning: marshal record", "entity_id", id, "error", err)
		return
	}
	line = append(line, '\n')

	s.mu.Lock()
	_, werr := s.logFile.Write(line)
	s.mu.Unlock()
	if werr != nil {
		slog.Warn("closeout warning: write record", "entity_id", id, "error", werr)
		return
	}

	s.bus.publish([]byte(web.FormatEventHTML(record)))
}

// hasOpenFocusInterval queries the DB for an open focus interval for
// (session, agent). Used as the cache-miss fallback in the nudge cache.
func (s *Server) hasOpenFocusInterval(sessionID, agentName string) bool {
	if s.obsStore == nil {
		return false
	}
	has, err := s.obsStore.HasOpenFocusInterval(context.Background(),
		store.SessionKey{SessionID: sessionID, AgentName: agentName})
	if err != nil {
		return false
	}
	return has
}

// agentNameFor converts hook AgentType to the canonical agent_name label.
func agentNameFor(agentType string) string {
	if agentType == "" {
		return ""
	}
	return "@" + agentType
}

// stashMCPIdentity records hook-derived identity for injection into the
// subsequent MCP call keyed by toolSuffix:entityID (10 s TTL).
func (s *Server) stashMCPIdentity(toolSuffix, entityID, sessionID, agentType string) {
	key := toolSuffix + ":" + entityID
	s.pendingMCPMu.Lock()
	if s.pendingMCPIdent == nil {
		s.pendingMCPIdent = make(map[string]mcpIdentity)
	}
	s.pendingMCPIdent[key] = mcpIdentity{
		SessionID: sessionID,
		AgentType: agentType,
		ExpiresAt: time.Now().Add(10 * time.Second),
	}
	s.pendingMCPMu.Unlock()
}

// injectMCPIdentity looks up stashed hook identity for the tools/call and, if
// found and unexpired, injects _meta.session_id and _meta.agent_type into the
// params. Returns the original raw params unchanged on any parse failure.
func (s *Server) injectMCPIdentity(raw json.RawMessage) json.RawMessage {
	// Parse enough to get name and entity ID.
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			ID       string `json:"id"`
			EntityID string `json:"entityID"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return raw
	}

	entityID := p.Arguments.ID
	if entityID == "" {
		entityID = p.Arguments.EntityID
	}
	if p.Name == "" || entityID == "" {
		return raw
	}

	// Strip the "mcp__wms__" prefix to get the tool suffix used as the key.
	const prefix = "mcp__wms__"
	toolSuffix := strings.TrimPrefix(p.Name, prefix)
	key := toolSuffix + ":" + entityID

	now := time.Now()
	s.pendingMCPMu.Lock()
	var ident mcpIdentity
	var ok bool
	if s.pendingMCPIdent != nil {
		ident, ok = s.pendingMCPIdent[key]
		if ok {
			delete(s.pendingMCPIdent, key)
		}
	}
	// Lazy TTL cleanup — bounded to avoid holding the lock too long.
	cleaned := 0
	for k, v := range s.pendingMCPIdent {
		if cleaned >= 10 {
			break
		}
		if now.After(v.ExpiresAt) {
			delete(s.pendingMCPIdent, k)
			cleaned++
		}
	}
	s.pendingMCPMu.Unlock()

	if !ok || now.After(ident.ExpiresAt) {
		return raw
	}

	// Unmarshal to map, inject _meta fields, re-marshal.
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	meta, _ := m["_meta"].(map[string]interface{})
	if meta == nil {
		meta = make(map[string]interface{})
	}
	if meta["session_id"] == nil || meta["session_id"] == "" {
		meta["session_id"] = ident.SessionID
	}
	if meta["agent_type"] == nil || meta["agent_type"] == "" {
		meta["agent_type"] = ident.AgentType
	}
	m["_meta"] = meta
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return json.RawMessage(out)
}

// subagentNameMap tracks the human-given name for subagents spawned via the
// Agent tool. Claude Code sets agent_type to the subagent's type descriptor
// (e.g. "general-purpose") rather than the name the lead gave it; this map
// resolves the type back to the name so feed shows "@scraper-research" instead
// of "@general-purpose".
//
// Keyed by (session_id, agent_type) → display name ("@scraper-research").
// For concurrent same-type subagents the last-spawned name wins — imperfect
// but far better than the raw type for every event.
type subagentNameMap struct {
	mu    sync.Mutex
	names map[string]map[string]string // session_id → agent_type → "@name"
}

func (m *subagentNameMap) record(sessionID, agentType, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.names == nil {
		m.names = make(map[string]map[string]string)
	}
	byType := m.names[sessionID]
	if byType == nil {
		byType = make(map[string]string)
		m.names[sessionID] = byType
	}
	byType[agentType] = "@" + name
}

func (m *subagentNameMap) resolve(sessionID, agentType string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.names == nil {
		return ""
	}
	return m.names[sessionID][agentType]
}

func (m *subagentNameMap) clearSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.names, sessionID)
}

// normaliseToolInput coerces event.ToolInput (interface{}) to a
// map[string]interface{} for StrField reads. Handles both direct map and
// JSON-string-encoded forms.
func normaliseToolInput(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	if s, ok := v.(string); ok {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m
		}
	}
	return nil
}

// mustMarshal marshals v to JSON; returns nil on error.
func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// handleHealth responds to GET /health with a simple status payload.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"status":  "ok",
		"version": version.Version,
		"commit":  version.Commit,
	})
}

// handleSSE streams events to an SSE client, optionally replaying history first.
// Query param: ?history=N (default 0, max 500).
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Send history burst from JSONL before subscribing so no events are missed.
	historyN := 0
	if v := r.URL.Query().Get("history"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 500 {
				n = 500
			}
			historyN = n
		}
	}

	if historyN > 0 {
		lines := s.readLastLines(historyN)
		for _, raw := range lines {
			var rec map[string]interface{}
			if err := json.Unmarshal([]byte(raw), &rec); err != nil {
				continue
			}
			html := web.FormatEventHTML(rec)
			fmt.Fprintf(w, "data: %s\n\n", html)
		}
		flusher.Flush()
	}

	id, ch := s.bus.subscribe()
	if ch == nil {
		http.Error(w, "too many SSE subscribers", http.StatusServiceUnavailable)
		return
	}
	s.metrics.SSESubscribers.Inc()
	defer func() {
		s.bus.unsubscribe(id)
		s.metrics.SSESubscribers.Dec()
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

// readLastLines reads at most n lines from the tail of the JSONL log.
// Opens the file read-only so it does not interfere with the append-mode logFile.
func (s *Server) readLastLines(n int) []string {
	f, err := os.Open(s.cfg.LogFile)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// resolveSubagentName captures agent names from Agent tool calls and resolves
// them for subsequent subagent events. Claude Code sets agent_type to the type
// descriptor ("general-purpose") for non-team subagents; this method overrides
// _agent_name with the human-given name from the Agent tool_input.
func (s *Server) resolveSubagentName(event hook.HookEvent, data map[string]interface{}) {
	if event.HookEventName == "PreToolUse" && event.ToolName == "Agent" {
		ti := normaliseToolInput(event.ToolInput)
		name := hook.StrField(ti, "name", 64)
		if name != "" {
			agentType := hook.StrField(ti, "subagent_type", 64)
			if agentType == "" {
				agentType = "general-purpose"
			}
			s.subagentNames.record(event.SessionID, agentType, name)
		}
	}

	if event.AgentType != "" {
		if resolved := s.subagentNames.resolve(event.SessionID, event.AgentType); resolved != "" {
			data["_agent_name"] = resolved
		}
	}
}

// buildRecord enriches and constructs the JSONL record from the raw event payload.
func (s *Server) buildRecord(data map[string]interface{}) map[string]interface{} {
	// Enrich display fields from raw hook payload. Idempotent: fields already
	// set by the Go hook client are left unchanged.
	hook.EnrichRecord(data)

	str := func(key string) string {
		v, _ := data[key].(string)
		return v
	}

	// Passthrough: if the record already has enriched fields (tag, display),
	// it's a pre-enriched JSONL line from the relay. Write it as-is — but still
	// pass any command-bearing field through the redactor. The hub redacts
	// before relaying, so this is normally a no-op; it is the replica's own
	// safety net against an un-redacted line arriving by any path.
	if _, hasTag := data["tag"]; hasTag {
		if _, hasDisplay := data["display"]; hasDisplay {
			if str("ts") == "" {
				data["ts"] = time.Now().UTC().Format("2006-01-02T15:04:05Z")
			}
			redactCommandFields(data)
			return data
		}
	}

	ts := str("ts")
	if ts == "" {
		ts = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}

	session := str("session_id")
	if len(session) > 12 {
		session = session[:12]
	}

	record := map[string]interface{}{
		"ts":         ts,
		"event":      str("hook_event_name"),
		"session":    session,
		"host":       str("_host"),
		"model":      str("_model"),
		"tool":       str("tool_name"),
		"agent_name": str("_agent_name"),
	}

	if focus := str("_focus"); focus != "" {
		record["focus"] = focus
	}
	if thought := str("_thought"); thought != "" {
		record["tag"] = "THNK"
		record["display"] = thought
	}
	if toolTag := str("_tool_tag"); toolTag != "" {
		record["tag"] = toolTag
		record["display"] = str("_tool_display")
	}
	if bashCmd := str("_bash_cmd"); bashCmd != "" {
		record["bash_cmd"] = bashCmd
	}
	if file := str("_file"); file != "" {
		record["file"] = file
	}
	if warnMsg := str("_warn_msg"); warnMsg != "" {
		record["warn_msg"] = warnMsg
	}
	if done := str("_done"); done != "" {
		record["tag"] = "DONE"
		record["display"] = done
	}
	if t := str("_team"); t != "" {
		record["team"] = t
	}

	// Choke-point redaction: scrub credentials from any command-bearing field
	// before this record is marshalled to JSONL. JSONL is the on-disk contract
	// feeding feed, the dashboard, and the public relay mirror, so masking here
	// protects all three sinks at once — including events from the Python remote
	// client, whose raw tool_input.command was enriched into _bash_cmd above.
	redactCommandFields(record)

	return record
}

// redactCommandFields masks credentials in the command-bearing fields of a
// JSONL record map in place. bash_cmd carries the raw shell command; display
// is scrubbed defensively in case a future producer (or the relay passthrough)
// routes a command through it.
//
// TRUST BOUNDARY: this field allow-list (bash_cmd, display) is the contract. A
// future change that routes a shell command through any other record field
// (e.g. focus, warn_msg) would bypass redaction — add the new field here.
func redactCommandFields(rec map[string]interface{}) {
	for _, key := range []string{"bash_cmd", "display"} {
		if v, ok := rec[key].(string); ok && v != "" {
			rec[key] = redact.Redact(v)
		}
	}
}

// rpcRequest is the minimal JSON-RPC 2.0 envelope.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func writeRPCResponse(w http.ResponseWriter, id interface{}, result interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeRPCError(w http.ResponseWriter, id interface{}, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]interface{}{"code": code, "message": message},
	})
}

// handleMCPActivity accepts POST /mcp/activity with a JSON-RPC 2.0 body and
// dispatches to the activity MCP package.
func (s *Server) handleMCPActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}

	// JSON-RPC notifications have no id; spec requires no response.
	if req.ID == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch req.Method {
	case "initialize":
		writeRPCResponse(w, req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]interface{}{"name": "activity-mcp", "version": version.Version},
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
		})
	case "tools/list":
		writeRPCResponse(w, req.ID, map[string]interface{}{"tools": mcpactivity.ToolDefs})
	case "tools/call":
		text, callErr := mcpactivity.HandleToolCall(req.Params)
		if callErr != nil {
			writeRPCError(w, req.ID, callErr.Code, callErr.Message)
		} else {
			writeRPCResponse(w, req.ID, mcpactivity.TextResult(text))
		}
	default:
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// handleMCPWMS accepts POST /mcp/wms with a JSON-RPC 2.0 body and dispatches
// to the wms MCP package.
func (s *Server) handleMCPWMS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.wmsStore == nil {
		writeRPCError(w, nil, -32000, "WMS store not available")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}

	// JSON-RPC notifications have no id; spec requires no response.
	if req.ID == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch req.Method {
	case "initialize":
		writeRPCResponse(w, req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]interface{}{"name": "wms-mcp", "version": version.Version},
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
		})
	case "tools/list":
		writeRPCResponse(w, req.ID, map[string]interface{}{"tools": mcpwms.ToolDefs})
	case "tools/call":
		params := s.injectMCPIdentity(req.Params)
		result, callErr := mcpwms.HandleToolCall(s.wmsStore, s.wmsEng, params)
		if callErr != nil {
			writeRPCError(w, req.ID, callErr.Code, callErr.Message)
		} else {
			writeRPCResponse(w, req.ID, result)
		}
	default:
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// handleEventsAPI serves GET /api/events?limit=N&since=TIMESTAMP.
// Returns recent enriched JSONL records as a JSON array, newest-first.
func (s *Server) handleEventsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}

	var sinceTime time.Time
	hasSince := false
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			sinceTime = t
			hasSince = true
		}
	}

	// Read more lines than limit to have headroom for since-filtering.
	readCount := limit
	if hasSince {
		readCount = limit * 3
		if readCount > 1500 {
			readCount = 1500
		}
	}

	rawLines := s.readLastLines(readCount)

	var records []map[string]interface{}
	for _, line := range rawLines {
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if hasSince {
			if ts, ok := rec["ts"].(string); ok {
				if t, err := time.Parse(time.RFC3339, ts); err == nil && !t.After(sinceTime) {
					continue
				}
			}
		}
		records = append(records, rec)
	}

	// Reverse to newest-first.
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}

	// Trim to limit after filtering.
	if len(records) > limit {
		records = records[:limit]
	}

	w.Header().Set("Content-Type", "application/json")
	if records == nil {
		records = []map[string]interface{}{}
	}
	json.NewEncoder(w).Encode(records) //nolint:errcheck
}

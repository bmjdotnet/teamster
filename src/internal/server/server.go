// Package server implements the hookd HTTP event receiver.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

	"database/sql"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	gaugemysql "github.com/bmjdotnet/teamster/internal/agenthealth/gauge/mysql"
	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/hook"
	"github.com/bmjdotnet/teamster/internal/intercept"
	mcpactivity "github.com/bmjdotnet/teamster/internal/mcp/activity"
	mcphealth "github.com/bmjdotnet/teamster/internal/mcp/health"
	mcproster "github.com/bmjdotnet/teamster/internal/mcp/roster"
	mcpwms "github.com/bmjdotnet/teamster/internal/mcp/wms"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/redact"
	"github.com/bmjdotnet/teamster/internal/roster"
	"github.com/bmjdotnet/teamster/internal/store"
	_ "github.com/bmjdotnet/teamster/internal/store/mysql" // registers mysql, mariadb
	"github.com/bmjdotnet/teamster/internal/version"
	"github.com/bmjdotnet/teamster/internal/web"
	"github.com/bmjdotnet/teamster/internal/wms"
	"github.com/prometheus/client_golang/prometheus"
)

const maxBodySize = 1 << 20 // 1 MB

const maxSSESubscribers = 100

const writeTimeout = 60 * time.Second

// ssePayload carries one event in both wire formats: html is the htmx-ready
// snippet (existing behavior, internal/web.FormatEventHTML output); raw is
// the original marshaled JSONL record line (no trailing newline), consumed
// by ?format=json subscribers (e.g. ctop) that can't parse HTML.
type ssePayload struct {
	html []byte
	raw  []byte
}

// eventBus fans out new event payloads to active SSE subscribers.
type eventBus struct {
	mu          sync.RWMutex
	subscribers map[uint64]chan ssePayload
	nextID      uint64
}

// subscribe registers a new subscriber and returns its ID and receive channel.
// Returns (0, nil) when the subscriber limit has been reached.
func (b *eventBus) subscribe() (uint64, chan ssePayload) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subscribers) >= maxSSESubscribers {
		return 0, nil
	}
	id := b.nextID
	b.nextID++
	ch := make(chan ssePayload, 64)
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
func (b *eventBus) publish(payload ssePayload) {
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

// instanceEntry is one registered Agent-tool subagent instance, keyed in
// Server.instanceRegistry by sessionID + "|" + agent_id — the unique
// per-instance identifier every hook event carries. Distinguishes a
// genuine new spawn (key absent) from a turn-resume of an already-registered
// instance (key present).
type instanceEntry struct {
	name         string // unique roster name (e.g., "@Explore-2")
	rosterID     string
	healAttempts int       // bounds selfHealParentRef retries — see its doc comment
	parentKnown  bool      // ParentRef is set; no heal needed
	descKnown    bool      // description came from the sidecar (ground truth); no heal needed
	lastHealAt   time.Time // spaces async heal launches from tool events
}

// Server receives hook telemetry events via HTTP and writes them to a JSONL log.
type Server struct {
	cfg              config.Config
	logFile          *os.File
	mu               sync.Mutex
	bus              eventBus
	wmsStore         wms.Store
	wmsEng           *wms.EngineImpl
	obsStore         store.Store // new unified store (nil when store package not ready)
	gaugeStore       gauge.GaugeStore
	gaugeDB          *sql.DB
	sessions         *observability.SessionTracker
	metrics          *observability.Metrics
	promRegistry     *prometheus.Registry
	sweepStop        chan struct{}
	telemetry        *telemetryQueue
	telemetryAgents  *agentCache
	telemetryCtx     context.Context
	telemetryCancel  context.CancelFunc
	subagentNames    subagentNameMap
	regMu            sync.Mutex
	instanceRegistry map[string]instanceEntry
	// earlyRegistered holds instKeys whose roster row was written by the
	// early (first tool event) path, so SubagentStart still owes a FIFO pop.
	earlyRegistered map[string]struct{}
	pendingMCPMu    sync.Mutex
	pendingMCPIdent map[string][]mcpIdentity // key: "toolSuffix:entityID", FIFO per key
	focusNudge      focusNudgeCache
	pressureNudge   pressureNudgeCache
	wmsWarnings     wmsWarningQueue
	rosterLastSeen  lastSeenCache
	turnStates      turnStateTracker
	registry        *intercept.Registry
}

// storeOpenMaxAttempts bounds how many times NewServer retries store.Open
// before giving up and starting with the /wms dashboard disabled.
const storeOpenMaxAttempts = 10

// claimFocusIntervalFailuresTotal counts OpenFocusInterval failures on the
// claim-success path (WMSStatusChange pending/""->active for a WorkUnit) —
// the first interval-open call site with dedicated Prometheus visibility
// instead of only a swallowed slog.Warn. Registered directly against
// observability.Registry in NewServer (not added to observability.Metrics)
// since NewServer runs exactly once per process (cmd/hookd/main.go), so a
// package-level MustRegister here carries the same one-shot-registration
// safety as the Metrics vecs without touching that struct.
var claimFocusIntervalFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "teamster_claim_focus_interval_failures_total",
	Help: "Focus interval open failures on WorkUnit claim success, by entity_type.",
}, []string{"entity_type"})

// tagColorsFromRegistry extracts the [3]int color for display.SetTagColors
// from a loaded interceptor registry's tag definitions.
func tagColorsFromRegistry(reg *intercept.Registry) map[string][3]int {
	if reg == nil {
		return nil
	}
	colors := make(map[string][3]int, len(reg.Tags))
	for label, def := range reg.Tags {
		colors[label] = def.Color
	}
	return colors
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
	metrics.BuildInfo.WithLabelValues(version.Version, version.Commit, version.BuildTime).Set(1)

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
		claimFocusIntervalFailuresTotal,
	)

	sweepStop := make(chan struct{})
	sessions.StartSweeper(sweepStop)

	// Interceptor registry: embedded defaults, overlaid with
	// <basedir>/etc/interceptors.yaml if present and valid. A malformed or
	// missing overlay file falls back to the embedded defaults — a cosmetic
	// config file must never take down ingest (design doc C5).
	basedir := filepath.Dir(cfg.DataDir)
	interceptReg, regErr := intercept.LoadWithOverlay(filepath.Join(basedir, "etc", "interceptors.yaml"))
	if regErr != nil {
		slog.Warn("interceptors.yaml invalid, using shipped defaults", "error", regErr)
	}
	display.SetTagColors(tagColorsFromRegistry(interceptReg))

	s := &Server{
		cfg:              cfg,
		logFile:          f,
		sessions:         sessions,
		metrics:          metrics,
		promRegistry:     reg,
		sweepStop:        sweepStop,
		pendingMCPIdent:  make(map[string][]mcpIdentity),
		instanceRegistry: make(map[string]instanceEntry),
		registry:         interceptReg,
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	if cfg.StoreDSN.Raw != "" {
		// Retry store.Open to ride out a boot race: on host startup hookd can
		// come up before MySQL is accepting connections. Bounded attempts with
		// exponential backoff (capped at 5s) so a genuinely-down store still
		// lets the rest of the server start in bounded time.
		var ms store.Store
		var storeErr error
		for attempt := 0; attempt < storeOpenMaxAttempts; attempt++ {
			ms, storeErr = store.Open(context.Background(), cfg.StoreDSN.Raw)
			if storeErr == nil {
				break
			}
			wait := time.Duration(1<<uint(attempt)) * 100 * time.Millisecond
			if wait > 5*time.Second {
				wait = 5 * time.Second
			}
			slog.Warn("store not ready, retrying", "attempt", attempt+1, "of", storeOpenMaxAttempts, "error", storeErr, "backoff", wait)
			time.Sleep(wait)
		}
		if storeErr == nil {
			s.obsStore = ms
			s.wmsStore = ms
			if initialCounts, hydErr := ms.CountEntitiesByStatus(context.Background()); hydErr == nil {
				observability.HydrateCounts(initialCounts)
			}
			s.wmsEng = wms.NewEngine(ms, nil)
			s.wmsEng.AddObserver(observability.NewInProcessObserver(observability.IncrementEntityCounts))
			reg.MustRegister(
				observability.NewUsageCollector(s.obsStore),
				observability.NewTagCountsCollector(s.obsStore),
				observability.NewAttributionCollector(s.obsStore),
				observability.NewDependenciesCollector(s.obsStore),
				observability.NewDecompositionCollector(s.obsStore),
				observability.NewCostCollector(s.obsStore),
				observability.NewIntervalPhaseCostCollector(s.obsStore),
				observability.NewBacklogCollector(s.obsStore),
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

			if gdb, gErr := openGaugeDB(cfg.StoreDSN); gErr == nil {
				s.gaugeDB = gdb
				s.gaugeStore = gaugemysql.New(gdb)
			} else {
				slog.Warn("gauge store unavailable — /mcp/health disabled", "error", gErr)
			}
		} else {
			slog.Error("WMS store unavailable after retries — /wms dashboard disabled", "error", storeErr)
		}
	}

	s.startReaper()

	return s, nil
}

// RegisterRoutes attaches the server's handlers to mux. In read-only mode
// the MCP, telemetry, session, and drain write endpoints return 403; /event
// and all read/dashboard/SSE routes remain available.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	timed := func(h http.HandlerFunc) http.Handler {
		return http.TimeoutHandler(http.HandlerFunc(h), writeTimeout, "request timeout")
	}

	mux.Handle("/event", timed(s.handleEvent))
	mux.Handle("/health", timed(s.handleHealth))
	mux.HandleFunc("/events/stream", s.handleSSE)
	mux.Handle("/api/events", timed(s.handleEventsAPI))
	mux.Handle("/rates", timed(s.handleRates))

	if s.cfg.ReadOnly {
		reject := func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "read-only mode", http.StatusForbidden)
		}
		mux.Handle("/mcp/activity", timed(reject))
		mux.Handle("/mcp/wms", timed(reject))
		mux.Handle("/mcp/roster", timed(reject))
		mux.Handle("/mcp/health", timed(reject))
		mux.Handle("/telemetry", timed(reject))
		mux.Handle("/session", timed(reject))
		mux.Handle("/focus-timeline", timed(reject))
		mux.Handle("/wms/api/drain", timed(reject))
		mux.Handle("/nudge", timed(reject))
		mux.Handle("/context", timed(reject))
	} else {
		mux.Handle("/mcp/activity", timed(s.handleMCPActivity))
		mux.Handle("/mcp/wms", timed(s.handleMCPWMS))
		mux.Handle("/mcp/roster", timed(s.handleMCPRoster))
		mux.Handle("/mcp/health", timed(s.handleMCPHealth))
		mux.Handle("/telemetry", timed(s.handleTelemetry))
		mux.Handle("/session", timed(s.handleSession))
		mux.Handle("/focus-timeline", timed(s.handleFocusTimeline))
		mux.Handle("/wms/api/drain", timed(web.HandleDrainAPI(s.obsStore)))
		mux.Handle("/nudge", timed(s.handleNudge))
		mux.Handle("/context", timed(s.handleContextReport))
	}

	mux.Handle("/wms/cost-flow", timed(web.HandleCostFlowPage))
	mux.Handle("/wms/api/cost-flow", timed(web.HandleCostFlowAPI(s.obsStore)))
	mux.Handle("/wms/tags", timed(web.HandleTagsPage))
	mux.Handle("/wms/api/tags", timed(web.HandleTagsAPI(s.obsStore)))

	// Muster health dashboard data plane: pure reads, must work on read-only
	// replicas, so these are registered unconditionally (unlike /mcp/health,
	// which stays scoped/agent-facing and is rejected in read-only mode).
	mux.Handle("GET /health/api/agents", timed(s.handleHealthAgentsAPI))
	mux.Handle("GET /health/api/agents/{roster_id}", timed(s.handleHealthSnapshotAPI))
	mux.Handle("GET /health/api/alerts", timed(s.handleHealthAlertsAPI))
	mux.Handle("GET /health/api/team/{team_name}", timed(s.handleHealthTeamAPI))
	mux.Handle("GET /health/dashboard", timed(web.HandleHealthPage))
	mux.HandleFunc("/health/stream", s.handleSSE) // /events/stream alias; same handler, same bus
	mux.Handle("/wms", timed(web.HandleWMS(s.obsStore)))
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
	if s.gaugeDB != nil {
		s.gaugeDB.Close() //nolint:errcheck
	}
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

	raw, err := json.Marshal(record)
	if err != nil {
		http.Error(w, "marshal error", http.StatusInternalServerError)
		return
	}
	line := append(raw, '\n')

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

	// Publish both wire formats to SSE subscribers: html for the existing
	// htmx dashboard, raw JSONL for ?format=json consumers (e.g. ctop).
	html := web.FormatEventHTML(record)
	s.bus.publish(ssePayload{html: []byte(html), raw: raw})

	// Observability branches — keyed on the typed event struct + raw map for enriched fields.
	s.dispatchObservability(event, data)

	// Focus-absent nudge: on PreToolUse, check whether (session, agent) has an
	// open focus interval. If not, return additionalContext asking the agent to
	// call wms_setFocus. Nudge up to nudgeMaxCount times then stop.
	// Skip activity MCP tools (always called first), ToolSearch (needed to load
	// deferred tools like wms_setFocus), and WMS MCP tools — nudging during
	// wms_setFocus itself is unreasonable and produces same-event false nudges.
	resp := map[string]interface{}{"status": "ok"}
	if event.HookEventName == "PreToolUse" && s.obsStore != nil &&
		!strings.HasPrefix(event.ToolName, "mcp__activity__") &&
		!strings.HasPrefix(event.ToolName, "mcp__wms__") &&
		event.ToolName != "ToolSearch" {
		agent := agentNameFor(event.AgentType)
		if msg, shouldNudge := s.focusNudge.check(event.SessionID, agent, func() bool {
			return s.hasAnyFocusInterval(event.SessionID, agent)
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

	// Pressure nudge: an independent signal from health-collector's threshold
	// engine (POST /nudge), delivered the same way as the focus nudge above —
	// injected as additionalContext on the target agent's next PreToolUse or
	// UserPromptSubmit, one-shot. Appended rather than overwritten since either
	// branch above may have already populated additionalContext for this event.
	if event.SessionID != "" && (event.HookEventName == "PreToolUse" || event.HookEventName == "UserPromptSubmit") {
		agent := agentNameFor(event.AgentType)
		if msg := s.pressureNudge.consume(event.SessionID, agent); msg != "" {
			if existing, ok := resp["additionalContext"].(string); ok && existing != "" {
				resp["additionalContext"] = existing + "\n\n" + msg
			} else {
				resp["additionalContext"] = msg
			}
		}
	}

	// WMS dispatch-protocol warnings: bridge same-event warnings
	// (data["_warn_msg"], e.g. orphan dispatch on this PreToolUse) and
	// consume queued deferred warnings (e.g. close-out tag enforcement that
	// fired asynchronously during a WMSStatusChange) into additionalContext
	// so the agent sees the feedback, not just the feed/ctop.
	if event.SessionID != "" && (event.HookEventName == "PreToolUse" || event.HookEventName == "UserPromptSubmit") {
		var wmsCtx string
		if warnMsg, _ := data["_warn_msg"].(string); warnMsg != "" {
			wmsCtx = warnMsg
		}
		if queued := s.wmsWarnings.consume(event.SessionID, agentNameFor(event.AgentType)); queued != "" {
			if wmsCtx != "" {
				wmsCtx += "\n\n" + queued
			} else {
				wmsCtx = queued
			}
		}
		if wmsCtx != "" {
			if existing, ok := resp["additionalContext"].(string); ok && existing != "" {
				resp["additionalContext"] = existing + "\n\n" + wmsCtx
			} else {
				resp["additionalContext"] = wmsCtx
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// modelFromData extracts the hook client's "_model" enrichment field: the
// operator's configured model, read from the LOCAL ~/.claude/settings.json
// by internal/hook.ProcessEvent's getModel() (hub-local clients) — set
// unconditionally on every event, before the per-event-type switch, so it's
// present (or absent) consistently across a session's whole lifecycle
// rather than only on some events. That determinism is what makes it safe
// to attach to every UpsertSession call in this file without a
// read-before-write: a later empty value never clobbers an earlier
// non-empty one, because the client re-derives the identical value from the
// same static config file on every event. It is only ever a settings.json
// default, not per-turn truth (an in-session /model switch won't be
// reflected) — health-collector's token_ledger-derived model remains the
// authoritative source once it's available; this just gives the sessions
// table (and, via health-collector's fallback, agent_health_gauge) a value
// immediately instead of staying empty until the token-scraper's first pass.
func modelFromData(data map[string]interface{}) string {
	v, _ := data["_model"].(string)
	return v
}

// hostFromData extracts the hook client's "_host" enrichment field so
// observability writes attribute to the event's originating host (remote
// client) rather than always the hub's own configured host. The value is
// normalized to the short hostname (domain suffix stripped) so that
// "studio" and "studio.bmj.net" resolve to the same identity.
func hostFromData(data map[string]interface{}, fallback string) string {
	if v, ok := data["_host"].(string); ok && v != "" {
		return shortHostname(v)
	}
	return shortHostname(fallback)
}

func shortHostname(h string) string {
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[:i]
	}
	return h
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
		// SubagentStart shares the same auto-registration path: it carries
		// agent_type (like a teammate's PreToolUse) and is the earliest
		// signal a new Agent-tool subagent exists, so it should register a
		// roster entry / session row just as promptly as a teammate's first
		// tool call does. BUT SubagentStart also fires on every Agent-Teams
		// teammate turn-resume (mailbox wakeup) with the SAME agent_type as
		// its very first event — agent_type/agent_name is not a safe
		// "have I seen this entity" discriminator on its own: it's a
		// stable per-teammate name (correctly repeats every turn-resume)
		// but ALSO a reusable subagent_type category label (correctly
		// repeats across distinct parallel Agent-tool dispatches of, say,
		// "general-purpose"). The in-memory SessionTracker's own Upsert
		// isn't a reliable enough guard either — it can evict a long-idle
		// teammate's entry via its own sweep/timeout, making that
		// teammate's next turn-resume falsely look brand new. So
		// SubagentStart alone checks the PERSISTENT roster store first: if
		// a roster entry already exists for (session, agent_name), this is
		// a known entity resuming, not a new spawn — skip registration
		// entirely and just refresh liveness.
		case "PreToolUse", "UserPromptSubmit", "SubagentStart":
			if event.HookEventName == "SubagentStart" {
				agent := s.registerSubagentStart(ctx, event, data)
				s.metrics.HookEventsTotal.With(prometheus.Labels{
					"event":      event.HookEventName,
					"host":       s.cfg.Host,
					"agent_name": agent,
				}).Inc()
				break
			}

			agent := resolvedAgentName(data, agentType)
			isNew := s.sessions.Upsert(event.SessionID, strings.TrimPrefix(agent, "@"))
			if spawnerType, _ := data["_spawner_type"].(string); spawnerType != "" {
				// Sub-subagent: SubagentStart is the registration authority.
				// Skip roster/session creation here to prevent competing writes.
				break
			}
			if isNew {
				s.metrics.SessionsTotal.With(prometheus.Labels{
					"host": s.cfg.Host,
				}).Inc()
				// Early upsert: make the session visible in MySQL immediately
				// (not just after Stop). Paired with an agent_roster row so
				// roster queries can see live agents. §1.1 + §2.1 of P0-roster.
				if s.obsStore != nil {
					now := time.Now().UTC()
					spawnerType, _ := data["_spawner_type"].(string)
					go func() {
						if event.AgentID != "" {
							if existingID, err := s.obsStore.ResolveRosterID(ctx, event.SessionID, agent); err == nil {
								if existing, err := s.obsStore.GetRosterEntry(ctx, existingID); err == nil &&
									existing.AgentID != "" && existing.AgentID != event.AgentID {
									slog.Debug("roster row belongs to another instance; deferring to SubagentStart",
										"session", event.SessionID, "agent", agent, "agent_id", event.AgentID, "owner_agent_id", existing.AgentID)
									return
								}
							}
						}
						sess := store.Session{
							SessionID: event.SessionID,
							AgentName: agent,
							Host:      hostFromData(data, s.cfg.Host),
							Username:  s.cfg.User,
							Status:    store.SessionStatusActive,
							Runtime:   "claude_code",
						}
						// Model only for the lead — teammate events carry
						// the lead's _model (hook client reads the parent
						// transcript), so stamping it clobbers the sessions
						// row. health-collector sources teammate model from
						// token_ledger.model (authoritative).
						if agentType == "" {
							sess.Model = modelFromData(data)
						}
						if err := s.obsStore.UpsertSession(ctx, sess); err != nil {
							slog.Warn("upsert session", "session", event.SessionID, "agent", agent, "error", err)
						}
						// Relationship heuristic for S2: empty agentType = lead,
						// non-empty = teammate (the predominant Agent Teams case).
						// A non-empty spawnerType means a teammate spawned this via
						// the Agent tool — a sub-subagent, not a direct teammate.
						// Gate 1 refines via meta.json taskKind.
						rel := "lead"
						parentAgent := ""
						if agentType != "" {
							rel = "teammate"
							if spawnerType != "" {
								parentAgent = agentNameFor(spawnerType)
								rel = "subagent"
							}
						}
						rosterID := roster.GenerateRosterID()
						boundAt := now
						sid := event.SessionID
						entry := store.RosterEntry{
							RosterID:     rosterID,
							SessionID:    &sid,
							AgentName:    agent,
							Host:         hostFromData(data, s.cfg.Host),
							Runtime:      "claude_code",
							Relationship: rel,
							CreatedAt:    now,
							BoundAt:      &boundAt,
							AgentID:      event.AgentID,
						}
						// Attempt to resolve the parent's roster_id as parent_ref.
						// macOS separate-session teammates carry _parent_session_id
						// (injected by teamster.py from the process's CLI args) —
						// resolve the parent in THAT session. Hub/Linux teammates
						// share the lead's session_id, so fall back to resolving
						// within this event's own session.
						// Also inherit team_name: prefer _team_name from the event
						// (macOS), else the lead's roster entry.
						parentSessionID := event.SessionID
						if psid, _ := data["_parent_session_id"].(string); psid != "" {
							parentSessionID = psid
						}
						if agentType != "" || parentSessionID != event.SessionID {
							if parentID, err := s.obsStore.ResolveRosterID(ctx, parentSessionID, parentAgent); err == nil {
								entry.ParentRef = &parentID
							}
							if leadRID, err := s.obsStore.ResolveRosterID(ctx, parentSessionID, ""); err == nil {
								if leadRoster, err := s.obsStore.GetRosterEntry(ctx, leadRID); err == nil && leadRoster.TeamName != "" {
									entry.TeamName = leadRoster.TeamName
								}
							}
							if entry.TeamName == "" {
								if tn, _ := data["_team_name"].(string); tn != "" {
									entry.TeamName = tn
								}
							}
						}
						if err := s.obsStore.UpsertRosterEntry(ctx, entry); err != nil {
							slog.Warn("upsert roster entry", "session", event.SessionID, "agent", agent, "roster_id", rosterID, "error", err)
						} else if event.AgentID != "" {
							s.regMu.Lock()
							if s.earlyRegistered == nil {
								s.earlyRegistered = make(map[string]struct{})
							}
							s.earlyRegistered[event.SessionID+"|"+event.AgentID] = struct{}{}
							s.regMu.Unlock()
						}
					}()
				}
			} else if s.obsStore != nil {
				// Throttled last_seen refresh: once per ~30s per (session, agent).
				if s.rosterLastSeen.shouldRefresh(event.SessionID, agent) {
					go func() {
						sess := store.Session{
							SessionID: event.SessionID,
							AgentName: agent,
							Host:      hostFromData(data, s.cfg.Host),
							Username:  s.cfg.User,
							Status:    store.SessionStatusActive,
						}
						if agentType == "" {
							sess.Model = modelFromData(data)
						}
						if err := s.obsStore.UpsertSession(ctx, sess); err != nil {
							slog.Warn("upsert session", "session", event.SessionID, "agent", agent, "error", err)
						}
					}()
				}
			}
			s.metrics.HookEventsTotal.With(prometheus.Labels{
				"event":      event.HookEventName,
				"host":       s.cfg.Host,
				"agent_name": agent,
			}).Inc()
			if event.HookEventName == "UserPromptSubmit" {
				s.turnStates.StartTurn(event.SessionID, agent)
			}
			if event.HookEventName == "PreToolUse" && event.ToolName != "" {
				s.metrics.ToolCallsTotal.With(prometheus.Labels{
					"tool":       event.ToolName,
					"host":       s.cfg.Host,
					"agent_name": agent,
					"status":     "",
				}).Inc()
			}
		case "Stop", "SubagentStop":
			s.metrics.HookEventsTotal.With(prometheus.Labels{
				"event":      event.HookEventName,
				"host":       s.cfg.Host,
				"agent_name": agentNameFor(agentType),
			}).Inc()
		case "PostToolUse":
			s.metrics.HookEventsTotal.With(prometheus.Labels{
				"event":      "PostToolUse",
				"host":       s.cfg.Host,
				"agent_name": agentNameFor(agentType),
			}).Inc()
		case "TeammateIdle":
			// The only push signal for a teammate's idle transition — without
			// it, turnStateTracker only flips to idle on Stop (session/agent
			// end), so a teammate between turns reads as "processing" the
			// whole time it's alive. Identity here is teammate_name, not
			// agent_type (this isn't a tool call).
			agent := agentNameFor(event.TeammateName)
			if agent != "" {
				s.turnStates.EndTurnForAgent(event.SessionID, agent)
			}
			s.metrics.HookEventsTotal.With(prometheus.Labels{
				"event":      "TeammateIdle",
				"host":       s.cfg.Host,
				"agent_name": agent,
			}).Inc()
		case "TaskCompleted":
			// Log-only: already written to JSONL by the generic handleEvent
			// path above; no roster/session state to update.
			s.metrics.HookEventsTotal.With(prometheus.Labels{
				"event":      "TaskCompleted",
				"host":       s.cfg.Host,
				"agent_name": agentNameFor(event.TeammateName),
			}).Inc()
		}
	}

	// Gauge activity: data has already passed through hook.EnrichRecord (called
	// from buildRecord before dispatchObservability runs), so _thought/_tool_tag/
	// _tool_display/_done are populated the same way they are for the JSONL
	// record's tag/display fields. Mirror that same precedence here so ctop/
	// health.html's ACTIVITY column reflects whatever the feed shows, covering
	// both ordinary tool calls and mcp__activity__reportActivity/
	// completeActivity (which enrich to _thought/_done, not _tool_tag).
	if s.gaugeStore != nil && event.SessionID != "" {
		if tag, display := activityFromData(data); display != "" {
			key := gauge.GaugeKey{Host: hostFromData(data, s.cfg.Host), SessionID: event.SessionID, AgentName: resolvedAgentName(data, agentType)}
			go func() {
				if err := s.gaugeStore.UpdateActivity(ctx, key, display, tag, time.Now()); err != nil {
					slog.Warn("gauge UpdateActivity", "session", key.SessionID, "agent", key.AgentName, "error", err)
				}
			}()
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
						if err := s.obsStore.OpenFocusInterval(ctx, key, wms.EntityOutcome, id); err != nil {
							slog.Warn("open focus interval", "session", key.SessionID, "agent", key.AgentName, "entity_type", wms.EntityOutcome, "entity_id", id, "error", err)
						}
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
						if err := s.obsStore.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, id); err != nil {
							slog.Warn("open focus interval", "session", key.SessionID, "agent", key.AgentName, "entity_type", wms.EntityWorkUnit, "entity_id", id, "error", err)
						}
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
							if err := s.obsStore.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, id); err != nil {
								slog.Warn("open focus interval", "session", key.SessionID, "agent", key.AgentName, "entity_type", wms.EntityWorkUnit, "entity_id", id, "error", err)
							}
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
						if err := s.obsStore.OpenFocusInterval(ctx, key, entityType, id); err != nil {
							slog.Warn("open focus interval", "session", key.SessionID, "agent", key.AgentName, "entity_type", entityType, "entity_id", id, "error", err)
						}
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
				"agent_name": resolvedAgentName(data, agentType),
			}).Inc()
			if msg := hook.StrField(toolInput, "message", 256); msg != "" {
				s.turnStates.SetActivity(event.SessionID, resolvedAgentName(data, agentType), msg)
			}
		case "mcp__activity__setOverallIntent":
			s.metrics.ActivityCallsTotal.With(prometheus.Labels{
				"method":     "setOverallIntent",
				"host":       s.cfg.Host,
				"agent_name": resolvedAgentName(data, agentType),
			}).Inc()
			if msg := hook.StrField(toolInput, "message", 256); msg != "" {
				s.turnStates.SetActivity(event.SessionID, resolvedAgentName(data, agentType), msg)
			}
		case "mcp__activity__completeActivity":
			s.metrics.ActivityCallsTotal.With(prometheus.Labels{
				"method":     "completeActivity",
				"host":       s.cfg.Host,
				"agent_name": resolvedAgentName(data, agentType),
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
		if s.obsStore != nil && wms.IsTerminal(entityType, newStatus) {
			sid := hook.StrField(data, "wms_session_id", 64)
			agent := agentNameFor(hook.StrField(data, "wms_agent_name", 64))
			eid := hook.StrField(data, "wms_entity_id", 128)
			if sid != "" && eid != "" {
				key := store.SessionKey{SessionID: sid, AgentName: agent}
				go func() {
					if err := s.obsStore.CloseFocusIntervalForEntity(ctx, key, entityType, eid); err != nil {
						slog.Warn("close focus interval for entity", "session", key.SessionID, "agent", key.AgentName, "entity_type", entityType, "entity_id", eid, "error", err)
					}
				}()
			}
		}
		// Claim-success focus interval: open a focus interval for the claiming
		// agent the moment their claim actually lands, so focus attribution is
		// mechanical instead of depending on a voluntary wms_setFocus call.
		// Keyed off THIS event (not wms_claimWorkUnit's PreToolUse) because
		// WMSStatusChange only fires after the engine's OnStatusChange call,
		// which only runs after store.ClaimWorkUnit has already succeeded — the
		// loser of a claim race never reaches this line. PreToolUse fires before
		// the atomic claim executes and can't distinguish winner from loser, so
		// opening there would give the loser a bogus interval. oldStatus=="" is
		// included alongside "pending" to also cover a workunit adopted straight
		// into active with no recorded prior status. Also fires for a plain
		// wms_updateWorkUnitStatus pending->active transition (WMSStatusChange
		// carries no discriminator for which MCP tool triggered it) — harmless,
		// since OpenFocusInterval no-ops when the exact entity is already open
		// (e.g. from that tool's own PreToolUse-time open above).
		if s.obsStore != nil && entityType == wms.EntityWorkUnit && newStatus == wms.StatusActive &&
			(oldStatus == wms.StatusPending || oldStatus == "") {
			sid := hook.StrField(data, "wms_session_id", 64)
			rawAgent := hook.StrField(data, "wms_agent_name", 64)
			eid := hook.StrField(data, "wms_entity_id", 128)
			if sid != "" && eid != "" {
				// The MCP call's own _meta is not a usable identity source: the
				// stdio wms-mcp client sends none, so wms_agent_name arrives
				// empty for lead and teammate alike. Before this recovery every
				// claim in this hub's history — 170 of 170 — recorded an empty
				// agent, so this open never once fired. Fall back to the identity
				// the claim's own PreToolUse already stashed, where Claude Code
				// does stamp agent_type. Empty-with-ok means the hook saw the
				// lead, which is a real answer and keys the interval correctly.
				agentKnown := rawAgent != ""
				if !agentKnown {
					// WMSStatusChange carries no discriminator for WHICH tool
					// drove the transition (see this case's opening comment), so
					// peek under every tool that can raise a workunit
					// pending->active. There are FOUR, all of which stash their
					// hook-derived identity; they differ in whether anything else
					// opens the interval for them:
					//   ToolClaimWorkUnit    — no PreToolUse open. Recovery is
					//                          the only path. (server.go:919)
					//   ToolUpdateStatus     — no PreToolUse open either; its
					//                          case only stashes (server.go:913-916),
					//                          and wms_updateStatus with
					//                          entityType "workunit" runs the same
					//                          UpdateWorkUnitStatus + OnStatusChange
					//                          as a claim. Recovery is load-bearing
					//                          here too.
					//   ToolUpdateWorkUnitStatus — belt-and-braces ONLY: its own
					//                          PreToolUse already opened the
					//                          interval at server.go:873-878 under
					//                          the hook's agentType, so by the time
					//                          this runs OpenFocusInterval no-ops.
					//                          Kept so the branch does not depend on
					//                          that ordering holding.
					//   ToolCreateWorkUnit   — belt-and-braces too, same reason: a
					//                          caller-supplied status (wms.go's
					//                          strArgDefault("status", pending))
					//                          means createWorkUnit(status:"active")
					//                          raises this same event, but its own
					//                          PreToolUse opened the interval first.
					//                          Listed so the set stays complete, not
					//                          because an interval depends on it.
					//
					// First answer wins, and each peek sees only its own tool's
					// stash. So two different agents reaching this entity through
					// DIFFERENT tools inside the TTL are never compared and the
					// contention is invisible — peekMCPIdentity's disagreement
					// test is per key, not per entity. Narrow enough to accept;
					// claim is first because it is the likelier real claimant.
					for _, tool := range []string{mcpwms.ToolClaimWorkUnit, mcpwms.ToolUpdateStatus, mcpwms.ToolUpdateWorkUnitStatus, mcpwms.ToolCreateWorkUnit} {
						if recovered, ok := s.peekMCPIdentity(tool, eid, sid); ok {
							rawAgent, agentKnown = recovered, true
							break
						}
					}
				}
				if !agentKnown {
					// Previously this fell through in silence — the warning below
					// lived only in the error branch, which an unidentified claim
					// never reaches. Silence here is why the defect survived 170
					// claims. Route to the lead ("" agent) since by definition we
					// could not identify who acted.
					slog.Warn("claim focus interval skipped: no agent identity",
						"session", sid, "work_unit_id", eid)
					claimFocusIntervalFailuresTotal.With(prometheus.Labels{"entity_type": wms.EntityWorkUnit}).Inc()
					s.wmsWarnings.queue(sid, "", fmt.Sprintf(
						"[WMS] could not identify the claiming agent for WU %s, so no focus interval was opened — call wms_setFocus to attribute your cost", eid))
				} else {
					agent := agentNameFor(rawAgent)
					key := store.SessionKey{SessionID: sid, AgentName: agent}
					go func() {
						if err := s.obsStore.OpenFocusInterval(ctx, key, wms.EntityWorkUnit, eid); err != nil {
							slog.Warn("open focus interval on claim", "session", sid, "agent", agent, "work_unit_id", eid, "error", err)
							claimFocusIntervalFailuresTotal.With(prometheus.Labels{"entity_type": wms.EntityWorkUnit}).Inc()
							s.wmsWarnings.queue(sid, agent, fmt.Sprintf(
								"[WMS] focus interval failed to open for WU %s — call wms_setFocus manually", eid))
						}
					}()
				}
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

	case "Stop", "SubagentStop":
		// Phantom SubagentStop (no agent_type): Claude Code fires these for
		// suggested next prompts and idle recaps — not real subagent
		// completions. They must not close the session or affect turn state.
		if event.HookEventName == "SubagentStop" && agentType == "" {
			break
		}
		agent := agentNameFor(agentType) // fallback
		if agentType != "" {
			// Resolve the unique auto-numbered name (e.g. "@Explore-2") the
			// matching SubagentStart assigned, rather than the raw agentType
			// label every same-type spawn shares.
			instKey := event.SessionID + "|" + event.AgentID
			s.regMu.Lock()
			if inst, ok := s.instanceRegistry[instKey]; ok {
				agent = inst.name
				delete(s.instanceRegistry, instKey)
			}
			s.regMu.Unlock()

			// Teammate/subagent stop: close only this agent. Both share the
			// lead's session_id, so a session-wide close here would
			// incorrectly mark still-active peers as stopped.
			s.sessions.CloseAgent(event.SessionID, agent)
			s.subagentNames.clearAgent(event.SessionID, agentType)
			s.focusNudge.clearAgentTurn(event.SessionID, agent)
			s.pressureNudge.clearAgent(event.SessionID, agent)
			s.wmsWarnings.clearAgent(event.SessionID, agent)
			s.rosterLastSeen.clearAgent(event.SessionID, agent)
			s.turnStates.EndTurnForAgent(event.SessionID, agent)
		} else {
			// Lead stop: end the lead's turn and clear every agent's per-turn
			// state. This does NOT mean the session ended — Claude Code emits
			// Stop at the end of every assistant turn — so nothing durable
			// (session status, intervals) is written from here. See the
			// comment below the clears.
			s.sessions.CloseSession(event.SessionID)
			s.subagentNames.clearSession(event.SessionID)
			s.focusNudge.clearSession(event.SessionID)
			s.pressureNudge.clearSession(event.SessionID)
			s.wmsWarnings.clearSession(event.SessionID)
			s.rosterLastSeen.clearSession(event.SessionID)
			s.turnStates.EndTurn(event.SessionID)
			s.regMu.Lock()
			for k := range s.instanceRegistry {
				if strings.HasPrefix(k, event.SessionID+"|") {
					delete(s.instanceRegistry, k)
				}
			}
			s.regMu.Unlock()
		}
		// NOTHING DURABLE IS WRITTEN HERE, deliberately. Stop is a TURN
		// boundary, not a session boundary: Claude Code emits it at the end of
		// every assistant turn, and there is no SessionEnd hook to distinguish
		// the two. This block used to mark every affected session closed and
		// drain its open intervals; because a lead's Stop carries no
		// agent_type it took the session-wide branch and killed every
		// teammate's interval too, teammates sharing the lead's session_id.
		// Measured before removal: that drain closed 279 intervals in 7 days
		// and only ~7% of a focused session's spend landed inside an interval.
		//
		// Intervals now close on one of three real signals: the agent focusing
		// something else (OpenFocusInterval's handoff), its entity reaching a
		// terminal status (CloseFocusIntervalForEntity, above), or the session
		// going quiet past TEAMSTER_GC_STALE_HOURS (reaper phase 3, which also
		// marks the session closed). Leaving an interval open costs nothing in
		// attribution: the allocator joins token_ledger rows to intervals by
		// timestamp, and a session that has stopped writes no ledger rows.
		//
		// Per-entity cost is written by the allocator (rollup → cost_rollup) from
		// token_ledger ⋈ wms_intervals (kind='focus') — no Stop-time per-entity write here.
	}
}

// registerSubagentStart is the registration entry point for SubagentStart
// events — both true sub-subagents (spawned by a non-lead agent) and direct
// teammates/subagents of the lead (see registerNewSubagentInstance's rel
// derivation; "sole registration authority" was this comment's claim in an
// earlier revision, but dispatchObservability's PreToolUse/UserPromptSubmit
// early-upsert path can ALSO first-register a teammate whose SubagentStart
// hasn't been processed yet — see the parent-ref-fifo-fix WU's DEFECTS.md
// entry for the untested latent race that leaves open). It resolves the
// instance key (session + the spawn's own agent_id, a unique per-instance
// identifier unlike agent_type, which is a reusable category label shared by
// every same-type spawn) against instanceRegistry: a known key is a
// turn-resume (mailbox wakeup or hookd-restart already-registered instance)
// and only needs a liveness refresh (plus a bounded parent-ref self-heal
// attempt, see selfHealParentRef); an unknown key is a potentially new
// spawn, resolved via registerNewSubagentInstance. Returns the resolved
// unique agent name.
func (s *Server) registerSubagentStart(ctx context.Context, event hook.HookEvent, data map[string]interface{}) string {
	instKey := event.SessionID + "|" + event.AgentID

	s.regMu.Lock()
	if inst, ok := s.instanceRegistry[instKey]; ok {
		s.regMu.Unlock()
		// Upsert's second parameter is a raw agentType — it prefixes with
		// "@" internally — so trim inst.name's existing prefix rather than
		// double-prefixing.
		s.sessions.Upsert(event.SessionID, strings.TrimPrefix(inst.name, "@"))
		if s.obsStore != nil && s.rosterLastSeen.shouldRefresh(event.SessionID, inst.name) {
			go func() {
				if err := s.obsStore.UpsertSession(ctx, store.Session{
					SessionID: event.SessionID,
					AgentName: inst.name,
					Host:      hostFromData(data, s.cfg.Host),
					Username:  s.cfg.User,
					Status:    store.SessionStatusActive,
					// Model intentionally omitted — the store's COALESCE guard on
					// that column protects against an empty value clobbering one
					// already captured from a prior event.
				}); err != nil {
					slog.Warn("upsert session", "session", event.SessionID, "agent", inst.name, "error", err)
				}
			}()
		}
		// A resumed instance may still carry a nil ParentRef from a spawn-time
		// sidecar/FIFO miss (see registerNewSubagentInstance) — bounded retry,
		// see selfHealParentRef's doc comment for why this is safe to call on
		// every resume.
		s.selfHealParentRef(ctx, event, instKey)
		// SubagentStart marks the subagent processing the same way a lead's
		// UserPromptSubmit does — without this, a subagent that never
		// receives its own UserPromptSubmit would stay at the
		// turnStateTracker's zero value (never "processing") for its entire
		// lifetime, showing incorrectly idle in the health dashboard.
		// SubagentStop is the matching EndTurnForAgent.
		s.turnStates.StartTurn(event.SessionID, inst.name)
		return inst.name
	}
	s.regMu.Unlock()

	// Unknown key — new spawn or hookd-restart resume.
	name := s.registerNewSubagentInstance(ctx, event, data, instKey)
	s.turnStates.StartTurn(event.SessionID, name)
	return name
}

// registerNewSubagentInstance handles a SubagentStart whose instance key is
// not yet in instanceRegistry — either a genuine new spawn or a resume after
// a hookd restart (which wiped the in-memory registry but left the roster
// row behind in the store). The uniqueify-check-through-UpsertRosterEntry
// sequence runs under regMu (mirroring the deleted preRegisterSubagent's
// locking discipline): two concurrent same-type spawns must not both see
// the base name as free and race to claim it. Only the trailing UpsertSession
// runs outside the lock — by that point the roster row is already durably
// unique, so it can't collide with a peer's registration.
func (s *Server) registerNewSubagentInstance(ctx context.Context, event hook.HookEvent, data map[string]interface{}, instKey string) string {
	sessionID := event.SessionID
	agentType := event.AgentType

	// Ground-truth enrichment from the launch-time sidecar, when available.
	// Best-effort: an unreadable file or absent fields just mean this stays
	// nil and registration falls back to the FIFO/TTL name-matching
	// heuristic below. See readSidecarForEvent's doc comment — a failure
	// here is now logged (it wasn't, and that silence is exactly what let
	// two real spawns land mis-parented to the lead; see the
	// parent-ref-fifo-fix WU).
	sidecarMeta := readSidecarForEvent(event)

	if event.AgentID != "" && s.obsStore != nil {
		name, err := s.obsStore.ResolveByAgentID(ctx, sessionID, event.AgentID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("resolve by agent_id", "session", sessionID, "agent_id", event.AgentID, "error", err)
		}
		if err == nil {
			if rosterID, err := s.obsStore.ResolveRosterID(ctx, sessionID, name); err == nil {
				s.regMu.Lock()
				if s.instanceRegistry == nil {
					s.instanceRegistry = make(map[string]instanceEntry)
				}
				_, early := s.earlyRegistered[instKey]
				delete(s.earlyRegistered, instKey)
				s.instanceRegistry[instKey] = instanceEntry{name: name, rosterID: rosterID}
				s.regMu.Unlock()
				if early {
					s.completeEarlyRegistration(ctx, event, instKey, rosterID)
				} else if entry, err := s.obsStore.GetRosterEntry(ctx, rosterID); err == nil && entry.ParentRef != nil {
					s.markParentKnown(instKey)
				}
				data["_agent_name"] = name
				s.sessions.Upsert(sessionID, strings.TrimPrefix(name, "@"))
				return name
			}
		}
	}

	baseName, spawnerType, fifoDesc := s.subagentNames.popWithDescription(sessionID, agentType)
	// spawnerKnown distinguishes "the FIFO gave us a confirmed answer, and
	// that answer happens to be the lead" (baseName came back non-empty,
	// spawnerType=="" because the lead's own events carry no agent_type)
	// from "we have no signal at all" (baseName came back empty and no
	// roster-adopt match below). Both cases reach the parentAgent=="" default
	// further down, but only the first one is a real "spawned by lead"
	// signal — see the ParentRef==nil handling after rel/parentAgent for
	// where this matters.
	spawnerKnown := true
	if baseName == "" {
		// FIFO empty or expired: no Agent-tool PreToolUse queued a name for
		// this spawn within the TTL. Check the roster before assuming this
		// is genuinely new — a hookd restart clears instanceRegistry but not
		// the persistent roster, so a live subagent's next turn-resume would
		// otherwise be mis-registered as a fresh spawn under a "-2" name.
		resolvedName := resolvedAgentName(data, agentType)
		if s.obsStore != nil {
			if rosterID, err := s.obsStore.ResolveRosterID(ctx, sessionID, resolvedName); err == nil {
				// Before adopting: check no other instance already maps to
				// this name (concurrent same-type agents after hookd
				// restart could otherwise both try to adopt it). The adopt
				// decision itself runs under regMu so the agent_id claim and
				// the registry insert are one step for racing resumes.
				alreadyClaimed := false
				s.regMu.Lock()
				for _, existing := range s.instanceRegistry {
					if existing.name == resolvedName {
						alreadyClaimed = true
						break
					}
				}
				if !alreadyClaimed && s.adoptRosterRow(ctx, event, rosterID) {
					if s.instanceRegistry == nil {
						s.instanceRegistry = make(map[string]instanceEntry)
					}
					s.instanceRegistry[instKey] = instanceEntry{name: resolvedName, rosterID: rosterID}
					s.regMu.Unlock()
					data["_agent_name"] = resolvedName
					s.sessions.Upsert(sessionID, strings.TrimPrefix(resolvedName, "@"))
					return resolvedName
				}
				s.regMu.Unlock()
				// Fall through to create a new entry — this name is already
				// adopted by a sibling's resume, or its row belongs to
				// another agent_id.
			}
		}
		baseName = agentNameFor(agentType)
		spawnerType = ""
		spawnerKnown = false
	}

	rel := "teammate"
	parentAgent := "" // resolves to the lead when empty AND spawnerKnown
	if spawnerType != "" {
		rel = "subagent"
		parentAgent = agentNameFor(spawnerType)
	}
	if sidecarMeta != nil && sidecarMeta.SpawnDepth > 0 {
		rel = "subagent"
	}

	now := time.Now().UTC()
	sid := sessionID
	boundAt := now

	s.regMu.Lock()
	if s.instanceRegistry == nil {
		s.instanceRegistry = make(map[string]instanceEntry)
	}
	unique := baseName
	if s.obsStore != nil {
		if _, err := s.obsStore.ResolveRosterID(ctx, sessionID, unique); err == nil {
			for i := 2; i <= 64; i++ {
				candidate := fmt.Sprintf("%s-%d", baseName, i)
				if _, err := s.obsStore.ResolveRosterID(ctx, sessionID, candidate); err != nil {
					unique = candidate
					break
				}
			}
		}
		if unique == baseName {
			if _, err := s.obsStore.ResolveRosterID(ctx, sessionID, unique); err == nil {
				// All 64 candidates exhausted; skip registration rather
				// than overwriting an existing entry.
				s.regMu.Unlock()
				data["_agent_name"] = unique
				s.sessions.Upsert(sessionID, strings.TrimPrefix(unique, "@"))
				return unique
			}
		}
	}
	rosterID := roster.GenerateRosterID()
	entry := store.RosterEntry{
		RosterID:     rosterID,
		SessionID:    &sid,
		AgentName:    unique,
		AgentID:      event.AgentID,
		Host:         hostFromData(data, s.cfg.Host),
		Runtime:      "claude_code",
		Relationship: rel,
		CreatedAt:    now,
		BoundAt:      &boundAt,
	}
	if sidecarMeta != nil && sidecarMeta.Model != "" {
		entry.Model = sidecarMeta.Model
	}
	// The sidecar is exact per agent_id; the FIFO can cross-label parallel
	// same-type spawns, so it is only a fallback (and stays open to heal).
	descFromSidecar := false
	if sidecarMeta != nil {
		if d := store.SanitizeRosterDescription(sidecarMeta.Description); d != "" {
			entry.Description = d
			descFromSidecar = true
		}
	}
	if entry.Description == "" {
		entry.Description = store.SanitizeRosterDescription(fifoDesc)
	}
	if s.obsStore != nil {
		// Sidecar parentAgentId, when present, is exact — prefer it over the
		// FIFO/TTL name-matching heuristic's parentAgent guess, which can
		// misattribute under concurrent same-type spawns.
		if sidecarMeta != nil && sidecarMeta.ParentAgentID != "" {
			if parentID, err := s.resolveParentRosterID(ctx, sessionID, sidecarMeta.ParentAgentID); err == nil {
				entry.ParentRef = &parentID
			} else {
				slog.Warn("resolve parent by agent_id", "session", sessionID, "agent_id", event.AgentID,
					"parent_agent_id", sidecarMeta.ParentAgentID, "error", err)
			}
		}
		// Only trust the FIFO-derived parentAgent when spawnerKnown — an
		// unconditional fallback here is exactly the bug the
		// parent-ref-fifo-fix WU traced: parentAgent=="" is ambiguous
		// between "confirmed spawned by the lead" and "we have no signal at
		// all", and ResolveRosterID(sessionID, "") always succeeds (it's the
		// lead's own roster row), so the ambiguous case silently resolved to
		// a confident wrong answer. When spawnerKnown is false and the
		// sidecar above didn't resolve it either, ParentRef stays nil — an
		// honest unknown that selfHealParentRef can correct once the sidecar
		// becomes readable (ctop and the roster/health APIs already render a
		// nil ParentRef as an unparented top-level row, not an error).
		if entry.ParentRef == nil && spawnerKnown {
			if parentID, err := s.obsStore.ResolveRosterID(ctx, sessionID, parentAgent); err == nil {
				entry.ParentRef = &parentID
			}
		}
		if leadRID, err := s.obsStore.ResolveRosterID(ctx, sessionID, ""); err == nil {
			if leadRoster, err := s.obsStore.GetRosterEntry(ctx, leadRID); err == nil {
				entry.TeamName = leadRoster.TeamName
			}
		}
		if err := s.obsStore.UpsertRosterEntry(ctx, entry); err != nil {
			slog.Warn("upsert roster entry", "session", sessionID, "agent", unique, "roster_id", rosterID, "error", err)
		}
	}
	s.instanceRegistry[instKey] = instanceEntry{name: unique, rosterID: rosterID, parentKnown: entry.ParentRef != nil, descKnown: descFromSidecar}
	s.regMu.Unlock()

	s.subagentNames.setResolved(sessionID, agentType, unique, spawnerType)
	data["_agent_name"] = unique
	s.sessions.Upsert(sessionID, strings.TrimPrefix(unique, "@"))

	if s.obsStore != nil {
		// Model intentionally omitted — the store's COALESCE guard on that
		// column protects against an empty value clobbering one already
		// captured from a prior event.
		if err := s.obsStore.UpsertSession(ctx, store.Session{
			SessionID: sessionID,
			AgentName: unique,
			Host:      hostFromData(data, s.cfg.Host),
			Username:  s.cfg.User,
			Status:    store.SessionStatusActive,
			Runtime:   "claude_code",
		}); err != nil {
			slog.Warn("upsert session", "session", sessionID, "agent", unique, "error", err)
		}
	}

	return unique
}

// adoptRosterRow decides whether the instance in event may take over an
// existing roster row found by name, and performs the claim when needed. Must
// be called with regMu held. A row stamped with a different agent_id belongs
// to another (typically dead) instance and is never inherited — its gauges,
// cost and ledger would leak across (teamster#27). A legacy row with no
// agent_id is claimed atomically (ClaimRosterAgentID) so only one of several
// racing resumes can take it. Any store error fails closed (caller numbers).
func (s *Server) adoptRosterRow(ctx context.Context, event hook.HookEvent, rosterID string) bool {
	entry, err := s.obsStore.GetRosterEntry(ctx, rosterID)
	if err != nil {
		slog.Warn("adopt roster row: read entry", "session", event.SessionID, "roster_id", rosterID, "error", err)
		return false
	}
	if entry.AgentID == event.AgentID {
		return true
	}
	if entry.AgentID != "" || event.AgentID == "" {
		return false
	}
	claimed, err := s.obsStore.ClaimRosterAgentID(ctx, rosterID, event.AgentID)
	if err != nil {
		slog.Warn("adopt roster row: claim agent_id", "session", event.SessionID, "roster_id", rosterID, "error", err)
		return false
	}
	return claimed
}

// markParentKnown records that the instance's row has a ParentRef so
// resolveSubagentName stops launching heal attempts for it.
func (s *Server) markParentKnown(instKey string) {
	s.regMu.Lock()
	if inst, ok := s.instanceRegistry[instKey]; ok {
		inst.parentKnown = true
		s.instanceRegistry[instKey] = inst
	}
	s.regMu.Unlock()
}

func (s *Server) markDescKnown(instKey string) {
	s.regMu.Lock()
	if inst, ok := s.instanceRegistry[instKey]; ok {
		inst.descKnown = true
		s.instanceRegistry[instKey] = inst
	}
	s.regMu.Unlock()
}

// completeEarlyRegistration finishes the FIFO bookkeeping SubagentStart owes
// an instance whose roster row was written by the early path: it consumes the
// lead's queued spawn record (so it can't shift the next same-type spawn) and
// uses the spawner to fill a nil ParentRef.
func (s *Server) completeEarlyRegistration(ctx context.Context, event hook.HookEvent, instKey, rosterID string) {
	_, spawnerType, fifoDesc := s.subagentNames.popWithDescription(event.SessionID, event.AgentType)
	entry, err := s.obsStore.GetRosterEntry(ctx, rosterID)
	if err != nil {
		return
	}
	desc, fromSidecar := store.SanitizeRosterDescription(fifoDesc), false
	if sc := readSidecarForEvent(event); sc != nil {
		if d := store.SanitizeRosterDescription(sc.Description); d != "" {
			desc, fromSidecar = d, true
		}
	}
	if desc != "" && (entry.Description == "" || (fromSidecar && entry.Description != desc)) {
		if err := s.obsStore.SetRosterDescription(ctx, rosterID, desc); err != nil {
			slog.Warn("early registration: set description", "session", event.SessionID, "roster_id", rosterID, "error", err)
		} else {
			entry.Description = desc
			if fromSidecar {
				s.markDescKnown(instKey)
			}
		}
	} else if fromSidecar {
		s.markDescKnown(instKey)
	}
	if entry.ParentRef != nil {
		s.markParentKnown(instKey)
		return
	}
	if spawnerType == "" {
		return
	}
	parentID, err := s.obsStore.ResolveRosterID(ctx, event.SessionID, agentNameFor(spawnerType))
	if err != nil {
		return
	}
	entry.ParentRef = &parentID
	entry.Relationship = "subagent"
	if err := s.obsStore.UpsertRosterEntry(ctx, entry); err != nil {
		slog.Warn("early registration: set parent_ref", "session", event.SessionID, "roster_id", rosterID, "error", err)
		return
	}
	s.markParentKnown(instKey)
}

// subagentMeta is the subset of a Claude Code agent-<id>.meta.json sidecar
// (written alongside a subagent's transcript at launch time) that
// registerNewSubagentInstance uses to enrich roster registration with ground
// truth the FIFO/TTL name-matching heuristic can't provide. Fields absent
// from a given sidecar (e.g. a plain Agent-tool spawn has no model/
// parentAgentId/spawnDepth, only agentType/description/toolUseId) just
// zero-value and are treated as "not available" by the caller.
type subagentMeta struct {
	ParentAgentID string `json:"parentAgentId"`
	Model         string `json:"model"`
	SpawnDepth    int    `json:"spawnDepth"`
	Description   string `json:"description"`
}

// readSubagentMeta reads and parses a subagent's meta.json sidecar.
func readSubagentMeta(path string) (*subagentMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m subagentMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// resolveParentRosterID maps a sidecar's parentAgentId (CC's per-instance
// agent_id for the spawning agent) to that parent's roster_id — the type
// ParentRef actually holds. This is two hops, not one: store.Store's
// ResolveByAgentID resolves agent_id -> agent_name (its documented contract,
// and the correct thing for its other caller, telemetry.go's ledger-row
// attribution, which wants a display name) — it does NOT return a roster_id.
// A prior revision of registerNewSubagentInstance stored ResolveByAgentID's
// return value directly into entry.ParentRef, which happened to go
// unnoticed only because sidecar reads were ALSO failing for the rows this
// path exists to get right (see parent-ref-fifo-fix WU); the moment
// ResolveByAgentID legitimately succeeds, that bug writes an agent_name
// string into a column every consumer (ctop's byRoster lookup, the roster
// and health APIs) expects to be a roster_id, rendering the row as an
// unresolvable orphan instead of correctly parented. ResolveRosterID here is
// the second hop, from that agent_name to its actual roster_id.
func (s *Server) resolveParentRosterID(ctx context.Context, sessionID, parentAgentID string) (string, error) {
	parentName, err := s.obsStore.ResolveByAgentID(ctx, sessionID, parentAgentID)
	if err != nil {
		return "", err
	}
	return s.obsStore.ResolveRosterID(ctx, sessionID, parentName)
}

// readSidecarForEvent reads a subagent's own .meta.json sidecar
// (<parentTranscriptDir>/subagents/agent-<agent_id>.meta.json, written by
// Claude Code itself at spawn time, same layout hook.go's
// getModelFromMetaSidecar and health-collector's teammate_context.go rely
// on) for the ground-truth parent/model/depth fields the FIFO/TTL
// name-matching heuristic can't provide. Returns nil when the file can't be
// read or parsed — logged at WARN rather than swallowed: the
// parent-ref-fifo-fix WU traced two real spawns (@teamster:implementer,
// @general-purpose) whose sidecars DID have the correct parentAgentId on
// disk minutes later, meaning the read failed transiently at the exact
// moment registration ran — most plausibly Claude Code hadn't finished
// writing the file yet — and the resulting nil sidecarMeta was
// indistinguishable from "no sidecar data exists" with nothing in the logs
// to tell the two apart. Called from both the initial registration
// (registerNewSubagentInstance) and the bounded resume retry
// (selfHealParentRef), so a transient miss gets a few more chances at the
// same signal instead of being permanent.
func readSidecarForEvent(event hook.HookEvent) *subagentMeta {
	if event.AgentID == "" || event.TranscriptPath == "" {
		return nil
	}
	childDir := strings.TrimSuffix(event.TranscriptPath, ".jsonl")
	metaPath := filepath.Join(childDir, "subagents", "agent-"+event.AgentID+".meta.json")
	meta, err := readSubagentMeta(metaPath)
	if err != nil {
		slog.Warn("read subagent sidecar", "session", event.SessionID, "agent_id", event.AgentID, "path", metaPath, "error", err)
		return nil
	}
	return meta
}

// selfHealMaxAttempts bounds how many SubagentStart resumes will retry a nil
// ParentRef before giving up permanently on that instance. A transient
// sidecar-read miss at spawn time (see readSidecarForEvent) typically
// becomes readable within an instance's first few turns; a small bounded
// number of tries is enough to catch that without turning a long-lived
// agent's every turn-resume (@agent-defs had 17 in one session) into a disk
// read plus a DB round trip for the rest of its life.
const selfHealMaxAttempts = 3

// selfHealMinSpacing is the minimum gap between heal attempts launched from
// an instance's tool events, so a Pre/Post pair can't burn the whole budget
// within milliseconds. A var so tests can shrink it.
var selfHealMinSpacing = 5 * time.Second

// selfHealParentRef re-attempts parent resolution for an already-registered
// instance (instKey already in instanceRegistry, i.e. this is a turn-resume,
// not the initial registration) whose roster row still has a nil ParentRef —
// the "we didn't know" marker the ParentRef==nil handling in
// registerNewSubagentInstance leaves behind instead of a confident wrong
// answer. It NEVER touches a row that already has a non-nil ParentRef: only
// an acknowledged unknown can be improved, a stored value — right or wrong —
// is left alone so a later bad read can't corrupt a good one. Bounded to
// selfHealMaxAttempts per instance (tracked in instanceRegistry) and cheap
// to fail: a still-unreadable sidecar just leaves ParentRef nil and returns,
// no retry loop, no blocking.
func (s *Server) selfHealParentRef(ctx context.Context, event hook.HookEvent, instKey string) {
	if s.obsStore == nil {
		return
	}

	s.regMu.Lock()
	inst, ok := s.instanceRegistry[instKey]
	if !ok || inst.healAttempts >= selfHealMaxAttempts {
		s.regMu.Unlock()
		return
	}
	inst.healAttempts++
	s.instanceRegistry[instKey] = inst
	s.regMu.Unlock()

	entry, err := s.obsStore.GetRosterEntry(ctx, inst.rosterID)
	if err != nil {
		return
	}
	needParent := entry.ParentRef == nil
	needDesc := !inst.descKnown
	if !needParent {
		s.markParentKnown(instKey)
	}
	if !needParent && !needDesc {
		return
	}

	sidecarMeta := readSidecarForEvent(event)
	if sidecarMeta == nil {
		return
	}
	// The sidecar description is ground truth for this agent_id, so unlike
	// parent_ref it may correct a stored (FIFO-sourced) value.
	if d := store.SanitizeRosterDescription(sidecarMeta.Description); needDesc && d != "" {
		if d == entry.Description {
			s.markDescKnown(instKey)
		} else if err := s.obsStore.SetRosterDescription(ctx, inst.rosterID, d); err != nil {
			slog.Warn("self-heal: set description", "session", event.SessionID, "roster_id", inst.rosterID, "error", err)
		} else {
			entry.Description = d
			s.markDescKnown(instKey)
		}
	}
	if !needParent || sidecarMeta.ParentAgentID == "" {
		return
	}
	parentID, err := s.resolveParentRosterID(ctx, event.SessionID, sidecarMeta.ParentAgentID)
	if err != nil {
		slog.Warn("self-heal: resolve parent by agent_id", "session", event.SessionID, "agent_id", event.AgentID,
			"parent_agent_id", sidecarMeta.ParentAgentID, "error", err)
		return
	}
	entry.ParentRef = &parentID
	if sidecarMeta.SpawnDepth > 0 {
		// The sidecar becoming readable also resolves the rel misclassification
		// that a nil ParentRef at spawn time leaves behind (see
		// registerNewSubagentInstance's rel derivation) — correcting one
		// without the other would leave the row internally inconsistent
		// (a correctly-parented row still labeled "teammate").
		entry.Relationship = "subagent"
	}
	if err := s.obsStore.UpsertRosterEntry(ctx, entry); err != nil {
		slog.Warn("self-heal: correct parent_ref", "session", event.SessionID, "roster_id", inst.rosterID, "error", err)
		return
	}
	s.markParentKnown(instKey)
}

// activityFromData extracts the (tag, display) pair hook.EnrichRecord wrote
// into data, in the same precedence buildRecord uses to fill the JSONL
// record's tag/display fields. Only one of _thought/_tool_tag/_done is ever
// set per event (EnrichRecord's switch is keyed on hook_event_name), so the
// order here doesn't pick between competing signals — it just covers every
// enrichment shape once, so this exact code doesn't need to be duplicated in
// every case that produces one of them.
func activityFromData(data map[string]interface{}) (tag, display string) {
	str := func(key string) string {
		v, _ := data[key].(string)
		return v
	}
	if thought := str("_thought"); thought != "" {
		return "THNK", thought
	}
	if toolTag := str("_tool_tag"); toolTag != "" {
		return toolTag, str("_tool_display")
	}
	if done := str("_done"); done != "" {
		return "DONE", done
	}
	return "", ""
}

// warnMissingRequiredTags implements W2 soft close-out enforcement: it loads the
// required tag keys and the workunit's EFFECTIVE tags — its own bindings plus any
// inherited from its parent outcome (wms.ResolveEntityTags; a required key set
// only on the outcome still counts as present here) — and if any required key has
// no tag it logs a warning and emits a WMSCloseOutWarning JSONL record so the gap
// is visible in feed and the dashboards. This now matches wms_getEntityTags and
// the store's RequireTagsOnDone hard reject on INHERITANCE, but the three still
// diverge on a second axis: this check (missingRequiredKeys) counts a key as
// present regardless of its bound value, while both stores' hard-reject loops
// additionally require TagValue != "" — an empty-value binding is present here
// but would still gate 'done'. The status transition is not affected — the hard
// reject is the store's job, gated by RequireTagsOnDone. Best-effort: any store
// error is logged and swallowed so the handler is never broken. A close posted
// under `teamster wms review-sweep`'s fixed identity (sessionID ==
// wms.ReviewSweepAgentID) still gets its WMSCloseOutWarning JSONL record, but
// never queues the agent-facing nudge — wh2-sweep-warning-queue.
//
// The gate is an identity-literal check, not a structural "is this session
// live" test, and that is a known, named limitation rather than an oversight:
// the review sweep is TODAY the only non-live poster of a WMSStatusChange at
// all (`wms gc` and `wms close` write journal rows directly and never notify
// hookd — a separate, pre-existing gap, not fixed here). The day some other
// batch tool starts notifying hookd under its own fixed identity, this check
// stops being complete and needs a second literal or a structural rewrite;
// tracked as a backlog item, not addressed by this WU on purpose.
func (s *Server) warnMissingRequiredTags(ctx context.Context, entityType, id, agentName, sessionID string) {
	required, err := s.obsStore.ListRequiredTagKeys(ctx)
	if err != nil {
		slog.Warn("closeout warning: list required tag keys", "entity_id", id, "error", err)
		return
	}
	if len(required) == 0 {
		return
	}
	resolved, err := wms.ResolveEntityTags(ctx, s.obsStore, entityType, id)
	if err != nil {
		slog.Warn("closeout warning: get entity tags", "entity_id", id, "error", err)
		return
	}
	tags := make([]wms.EntityTag, len(resolved))
	for i, rt := range resolved {
		tags[i] = rt.EntityTag
	}
	missing := missingRequiredKeys(required, tags)
	if len(missing) == 0 {
		return
	}
	slog.Warn("workunit done without required tags", "entity_id", id, "missing", missing)
	// wh2-sweep-warning-queue: sessionID == wms.ReviewSweepAgentID means this
	// close was posted by the review sweep under its fixed, non-live identity
	// (WP3-DESIGN.md §5) — no PreToolUse, Stop or UserPromptSubmit for that
	// (session, agent) pair ever runs, so queuing here would grow
	// wmsWarningQueue.pending by one unreachable key on every confirmed sweep
	// run, forever. Skip only the queue; the audit trail below is unaffected.
	//
	// Gated on sessionID (this function's own parameter, sourced from the
	// WMSStatusChange record's wms_session_id field), not the top-level
	// session_id/agent_type PreToolUse's consume() call reads elsewhere —
	// because sessionID/agentName are exactly what queue() below bakes into
	// the pending-map key via cacheKey(sessionID, normalizeAgent(agentName)),
	// so gating on them is gating on the literal key, not a lookalike. The
	// two field pairs carry the same value for a sweep-posted event only
	// because statusChangeRecord (hookobserver.go) copies change.SessionID
	// into both the top-level and wms_ fields when it is non-empty — a
	// property of that one call site, not a general guarantee to lean on.
	if sessionID != wms.ReviewSweepAgentID {
		s.wmsWarnings.queue(sessionID, agentName,
			fmt.Sprintf("[WMS] workunit %s closed without required tags: %s — add them with wms_tagEntity",
				id, strings.Join(missing, ", ")))
	}
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

	raw, err := json.Marshal(record)
	if err != nil {
		slog.Warn("closeout warning: marshal record", "entity_id", id, "error", err)
		return
	}
	line := append(raw, '\n')

	s.mu.Lock()
	_, werr := s.logFile.Write(line)
	s.mu.Unlock()
	if werr != nil {
		slog.Warn("closeout warning: write record", "entity_id", id, "error", werr)
		return
	}

	s.bus.publish(ssePayload{html: []byte(web.FormatEventHTML(record)), raw: raw})
}

// hasAnyFocusInterval queries the DB to answer "does this session/agent have
// an OPEN focus interval right now?" Used as the cache-miss fallback in the
// nudge cache. HasAnyFocusInterval used to mean "ever set" (open OR closed)
// under the old per-turn Stop-drain, where every interval closed at every
// turn end regardless of staleness — an ended interval still proved the
// agent called setFocus. Since wh2-idle-teammate-exemption, intervals only
// close on handoff, terminal entity, or reaper/sweep staleness, so this now
// requires ended_at IS NULL: a teammate whose interval the reaper closed for
// staleness must fail this check so invalidateAll's re-derivation (nudge.go)
// actually nudges it, instead of a stale "yes" from a closed row it set
// hours ago.
func (s *Server) hasAnyFocusInterval(sessionID, agentName string) bool {
	if s.obsStore == nil {
		return false
	}
	has, err := s.obsStore.HasAnyFocusInterval(context.Background(),
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

// resolvedAgentName prefers the human-given name resolved by
// resolveSubagentName (data["_agent_name"], e.g. "@thirdparty-investigator")
// over the generic agentNameFor(agentType) label (e.g. "@general-purpose"),
// so Agent-tool sub-subagents register and get attributed under the name the
// spawning agent gave them rather than colliding on their shared type.
func resolvedAgentName(data map[string]interface{}, agentType string) string {
	if name, _ := data["_agent_name"].(string); name != "" {
		return name
	}
	return agentNameFor(agentType)
}

// stashMCPIdentity records hook-derived identity for injection into the
// subsequent MCP call keyed by toolSuffix:entityID (10 s TTL). Appended to a
// FIFO per key rather than overwriting: two agents calling the same tool on
// the same entity within the TTL window each get their own stashed identity
// instead of the second stash clobbering the first, and injectMCPIdentity
// consumes them in the same order PreToolUse events arrived — the order the
// corresponding MCP calls arrive in.
func (s *Server) stashMCPIdentity(toolSuffix, entityID, sessionID, agentType string) {
	key := toolSuffix + ":" + entityID
	s.pendingMCPMu.Lock()
	if s.pendingMCPIdent == nil {
		s.pendingMCPIdent = make(map[string][]mcpIdentity)
	}
	s.pendingMCPIdent[key] = append(s.pendingMCPIdent[key], mcpIdentity{
		SessionID: sessionID,
		AgentType: agentType,
		ExpiresAt: time.Now().Add(10 * time.Second),
	})
	s.pendingMCPMu.Unlock()
}

// peekMCPIdentity reads the agent_type stashed by a PreToolUse for
// (toolSuffix, entityID) in sessionID WITHOUT consuming it. injectMCPIdentity
// pops entries FIFO for the HTTP JSON-RPC transport; the stdio wms-mcp
// subprocess never reaches that path, so its identity is still sitting in the
// stash when the resulting WMSStatusChange POST arrives. A consuming read here
// would steal an entry the HTTP path is entitled to.
//
// An empty agentType with ok==true is a MEANINGFUL answer, not a miss: hook
// payloads carry no agent_type for a lead, so "" means "the hook saw the lead".
// ok==false means no unexpired stash exists and the caller genuinely does not
// know who acted.
//
// SCOPE, so this is not read as more than it is: the test below sees exactly
// one key, toolSuffix+":"+entityID. It detects contention WITHIN one tool's
// stash, not contention on the entity generally. Two different agents reaching
// the same WorkUnit through DIFFERENT tools inside the TTL leave one entry
// under each key and are never compared — see the caller's loop, which returns
// the first tool that answers.
//
// DISAGREEMENT is reported as a miss; multiplicity alone is not. Two agents
// racing the same entity through the SAME tool within the TTL leave two
// entries under one key, and
// Agent-Teams teammates share the lead's session_id so sessionID cannot
// separate them. Only the claim winner produces a WMSStatusChange, and FIFO
// order is PreToolUse arrival order, not commit order — so choosing between
// DIFFERING candidates risks keying the interval to the loser, and declining
// turns a silent misattribution into a visible nudge.
//
// When the live candidates agree there is nothing to choose between, so the
// answer is returned. State the property precisely: agreement means "the
// candidates agree on the identity the interval would be keyed to", NOT "one
// agent retried". stashMCPIdentity stores the RAW agentType, which the Stop
// branch above notes is the label every same-type spawn shares — the unique
// auto-numbered name is resolved from instanceRegistry only there. So two
// DIFFERENT same-type teammates both stash e.g. "Explore" and are admitted
// here. That is safe because agentNameFor maps both to the identical key
// "@Explore": there is no wrong answer available. Declining on count instead
// bought nothing and cost a false decline plus a false warning whenever one
// agent's claim failed and was retried inside the TTL.
func (s *Server) peekMCPIdentity(toolSuffix, entityID, sessionID string) (string, bool) {
	key := toolSuffix + ":" + entityID
	now := time.Now()
	s.pendingMCPMu.Lock()
	defer s.pendingMCPMu.Unlock()
	var found string
	var n int
	for _, e := range s.pendingMCPIdent[key] {
		if now.After(e.ExpiresAt) || e.SessionID != sessionID {
			continue
		}
		if n > 0 && e.AgentType != found {
			return "", false
		}
		found = e.AgentType
		n++
	}
	if n == 0 {
		return "", false
	}
	return found, true
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
		entries := s.pendingMCPIdent[key]
		for i, e := range entries {
			if now.After(e.ExpiresAt) {
				continue
			}
			ident = e
			ok = true
			s.pendingMCPIdent[key] = append(entries[:i], entries[i+1:]...)
			if len(s.pendingMCPIdent[key]) == 0 {
				delete(s.pendingMCPIdent, key)
			}
			break
		}
	}
	// Lazy TTL cleanup — bounded to avoid holding the lock too long.
	cleaned := 0
	for k, v := range s.pendingMCPIdent {
		if cleaned >= 10 {
			break
		}
		live := v[:0]
		for _, e := range v {
			if !now.After(e.ExpiresAt) {
				live = append(live, e)
			}
		}
		if len(live) == 0 {
			delete(s.pendingMCPIdent, k)
		} else {
			s.pendingMCPIdent[k] = live
		}
		cleaned++
	}
	s.pendingMCPMu.Unlock()

	if !ok {
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

// subagentEntry is one queued Agent-tool spawn: the human-given name and the
// agent_type of whichever agent (lead or teammate) called the Agent tool.
type subagentEntry struct {
	name       string
	spawner    string
	desc       string
	recordedAt time.Time
}

// subagentEntryTTL bounds how long a queued Agent-tool spawn is trusted as
// "this is the SubagentStart we're waiting for". Past this age, pop() treats
// the entry as stale (still dequeues it, so it doesn't block later pops, but
// reports it as not-found) — the Agent-tool PreToolUse queued a name for a
// spawn that (for whatever reason) never sent a timely SubagentStart, so
// matching it to an unrelated later spawn of the same type would misattribute.
const subagentEntryTTL = 60 * time.Second

// subagentNameMap tracks the human-given name for subagents spawned via the
// Agent tool. Claude Code sets agent_type to the subagent's type descriptor
// (e.g. "general-purpose") rather than the name the lead gave it; this map
// resolves the type back to the name so feed shows "@scraper-research" instead
// of "@general-purpose".
//
// Keyed by "session_id|agent_type" → FIFO queue of entries, one per Agent-tool
// spawn of that type. record() appends on the Agent-tool PreToolUse; pop()
// dequeues the oldest entry — used exclusively by SubagentStart's own
// registration (registerNewSubagentInstance) to decide whether a given
// spawn was predicted by a preceding Agent-tool call, and mirrors the result
// into resolved regardless of TTL outcome. peek() is the read-only,
// FIFO-untouched lookup every other event uses (via resolveSubagentName) to
// display the resolved name for an already-registered instance — it never
// dequeues, so it can be called on every event without disturbing pop()'s
// bookkeeping. For concurrent same-type spawns, once all queued names are
// popped every further peek() shares the last-popped entry until
// setResolved overwrites it with a uniquified name.
type subagentNameMap struct {
	mu       sync.Mutex
	m        map[string][]subagentEntry
	resolved map[string]subagentEntry
}

func subagentNameKey(sessionID, agentType string) string {
	return sessionID + "|" + agentType
}

func (m *subagentNameMap) record(sessionID, agentType, name, spawnerType string) {
	m.recordWithDescription(sessionID, agentType, name, spawnerType, "")
}

// recordWithDescription is record plus the Agent-tool tool_input.description,
// the human-readable label that survives when CC blocks the name parameter.
func (m *subagentNameMap) recordWithDescription(sessionID, agentType, name, spawnerType, desc string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = make(map[string][]subagentEntry)
	}
	key := subagentNameKey(sessionID, agentType)
	m.m[key] = append(m.m[key], subagentEntry{name: "@" + name, spawner: spawnerType, desc: desc, recordedAt: time.Now()})
}

// pop dequeues the oldest queued entry for (sessionID, agentType), mirroring
// it into resolved. Returns ("", "") when the queue is empty OR the dequeued
// entry is older than subagentEntryTTL — in the TTL case the entry is still
// consumed (and still mirrored to resolved as a display fallback), just not
// reported as a confirmed match to the caller.
func (m *subagentNameMap) pop(sessionID, agentType string) (name, spawner string) {
	name, spawner, _ = m.popWithDescription(sessionID, agentType)
	return name, spawner
}

// popWithDescription is pop plus the entry's Agent-tool description; the
// description is withheld ("") under the same conditions as name and spawner.
func (m *subagentNameMap) popWithDescription(sessionID, agentType string) (name, spawner, desc string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := subagentNameKey(sessionID, agentType)
	queue := m.m[key]
	if len(queue) == 0 {
		return "", "", ""
	}
	entry := queue[0]
	if len(queue) == 1 {
		delete(m.m, key)
	} else {
		m.m[key] = queue[1:]
	}
	if m.resolved == nil {
		m.resolved = make(map[string]subagentEntry)
	}
	m.resolved[key] = entry
	if time.Since(entry.recordedAt) > subagentEntryTTL {
		return "", "", ""
	}
	return entry.name, entry.spawner, entry.desc
}

// peek returns the sticky last-resolved name for (sessionID, agentType)
// without touching the FIFO queue — for display resolution only, safe to
// call on every event.
func (m *subagentNameMap) peek(sessionID, agentType string) (name, spawner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := subagentNameKey(sessionID, agentType)
	entry := m.resolved[key]
	return entry.name, entry.spawner
}

// setResolved overwrites the sticky resolved entry for (sessionID, agentType)
// directly — used after uniquify-ing a popped base name (e.g. "@Explore" →
// "@Explore-2") so subsequent peek() calls return the FINAL registered name,
// not the pre-collision one pop() mirrored in.
func (m *subagentNameMap) setResolved(sessionID, agentType, name, spawnerType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resolved == nil {
		m.resolved = make(map[string]subagentEntry)
	}
	m.resolved[subagentNameKey(sessionID, agentType)] = subagentEntry{name: name, spawner: spawnerType}
}

func (m *subagentNameMap) clearSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := sessionID + "|"
	for k := range m.m {
		if strings.HasPrefix(k, prefix) {
			delete(m.m, k)
		}
	}
	for k := range m.resolved {
		if strings.HasPrefix(k, prefix) {
			delete(m.resolved, k)
		}
	}
}

// clearAgent removes the queued and resolved entries for a single agentType
// for sessionID, leaving other agent types tracked for the same session
// untouched. Use this for a teammate's Stop event, where the rest of the team
// is still mid-turn.
func (m *subagentNameMap) clearAgent(sessionID, agentType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := subagentNameKey(sessionID, agentType)
	delete(m.m, key)
	delete(m.resolved, key)
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

	// format selects the wire representation: "" or "html" (default, existing
	// htmx dashboard behavior, byte-identical to before) or "json" (raw JSONL,
	// for non-HTML consumers like ctop).
	jsonFormat := r.URL.Query().Get("format") == "json"

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
			if jsonFormat {
				// readLastLines strips newlines already; JSONL lines never
				// contain raw newlines, so one `data:` field per event is safe.
				fmt.Fprintf(w, "data: %s\n\n", raw)
				continue
			}
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
			if jsonFormat {
				fmt.Fprintf(w, "data: %s\n\n", payload.raw)
			} else {
				fmt.Fprintf(w, "data: %s\n\n", payload.html)
			}
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
// _agent_name with the human-given name from the Agent tool_input, and stashes
// the spawning agent's type in _spawner_type so dispatchObservability's roster
// registration can resolve the correct parent_ref (see resolvedAgentName).
func (s *Server) resolveSubagentName(event hook.HookEvent, data map[string]interface{}) {
	// FIFO push MUST run before the instanceRegistry early-return: a teammate
	// already in the registry (its own agent_id resolves) can still be
	// spawning a child via Agent — the child's spawn info goes into the FIFO
	// keyed by the child's subagent_type, not the parent's agent_id.
	if event.HookEventName == "PreToolUse" && event.ToolName == "Agent" {
		ti := normaliseToolInput(event.ToolInput)
		name := hook.StrField(ti, "name", 64)
		agentType := hook.StrField(ti, "subagent_type", 64)
		if agentType == "" {
			agentType = "general-purpose"
		}
		displayName := name
		if displayName == "" {
			displayName = agentType
		}
		s.subagentNames.recordWithDescription(event.SessionID, agentType, displayName, event.AgentType,
			store.SanitizeRosterDescription(hook.StrField(ti, "description", 4096)))
	}

	if event.AgentID != "" {
		instKey := event.SessionID + "|" + event.AgentID
		s.regMu.Lock()
		if inst, ok := s.instanceRegistry[instKey]; ok {
			data["_agent_name"] = inst.name
			heal := event.HookEventName != "SubagentStart" && (!inst.parentKnown || !inst.descKnown) &&
				inst.healAttempts < selfHealMaxAttempts && time.Since(inst.lastHealAt) >= selfHealMinSpacing
			if heal {
				inst.lastHealAt = time.Now()
				s.instanceRegistry[instKey] = inst
			}
			s.regMu.Unlock()
			if _, spawner := s.subagentNames.peek(event.SessionID, event.AgentType); spawner != "" {
				data["_spawner_type"] = spawner
			}
			if heal {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					s.selfHealParentRef(ctx, event, instKey)
				}()
			}
			return
		}
		s.regMu.Unlock()
	}

	if event.AgentType != "" {
		if resolved, spawner := s.subagentNames.peek(event.SessionID, event.AgentType); resolved != "" {
			data["_agent_name"] = resolved
			if spawner != "" {
				data["_spawner_type"] = spawner
			}
		}
	}
}

// buildRecord enriches and constructs the JSONL record from the raw event payload.
func (s *Server) buildRecord(data map[string]interface{}) map[string]interface{} {
	// Enrich display fields from raw hook payload. Idempotent: fields already
	// set by the Go hook client are left unchanged.
	hook.EnrichRecord(data, s.registry)

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

	// Two truncation boundaries, both load-bearing elsewhere: cmd/teamster's
	// `wms backfill` (wms_backfill.go) treats a resolved session id of
	// exactly 12 or 64 chars as suspect-truncated and refuses to write it,
	// specifically because these are the two lengths produced here. Changing
	// either cap without updating that gate silently reopens the defect it
	// exists to prevent.
	sessionFull := str("session_id")
	if len(sessionFull) > 64 {
		sessionFull = sessionFull[:64]
	}
	session := sessionFull
	if len(session) > 12 {
		session = session[:12]
	}

	record := map[string]interface{}{
		"ts":           ts,
		"event":        str("hook_event_name"),
		"session":      session,     // truncated: feed's display key, unchanged
		"session_full": sessionFull, // untruncated (capped to sessions.session_id's width): for ingestion/attribution
		"host":         str("_host"),
		"model":        str("_model"),
		"tool":         str("tool_name"),
		"agent_name":   str("_agent_name"),
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

// handleMCPRoster accepts POST /mcp/roster with a JSON-RPC 2.0 body and
// dispatches to the roster MCP package.
func (s *Server) handleMCPRoster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.obsStore == nil {
		writeRPCError(w, nil, -32000, "roster store not available")
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

	if req.ID == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch req.Method {
	case "initialize":
		writeRPCResponse(w, req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]interface{}{"name": "roster-mcp", "version": version.Version},
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
		})
	case "tools/list":
		writeRPCResponse(w, req.ID, map[string]interface{}{"tools": mcproster.ToolDefs})
	case "tools/call":
		result, callErr := mcproster.HandleToolCall(s.obsStore, req.Params)
		if callErr != nil {
			writeRPCErrorWithReason(w, req.ID, mcproster.FormatError(callErr))
		} else {
			writeRPCResponse(w, req.ID, result)
		}
	default:
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// writeRPCErrorWithReason writes a JSON-RPC error response with a structured
// error object that may include a reason field.
func writeRPCErrorWithReason(w http.ResponseWriter, id interface{}, errObj map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
		"jsonrpc": "2.0",
		"id":      id,
		"error":   errObj,
	})
}

// handleMCPHealth accepts POST /mcp/health with a JSON-RPC 2.0 body and
// dispatches to the health MCP package.
func (s *Server) handleMCPHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.obsStore == nil || s.gaugeStore == nil {
		writeRPCError(w, nil, -32000, "health store not available")
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

	if req.ID == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch req.Method {
	case "initialize":
		writeRPCResponse(w, req.ID, map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]interface{}{"name": "health-mcp", "version": version.Version},
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
		})
	case "tools/list":
		writeRPCResponse(w, req.ID, map[string]interface{}{"tools": mcphealth.ToolDefs})
	case "tools/call":
		result, callErr := mcphealth.HandleToolCall(s.obsStore, s.gaugeStore, s.turnStates.IsProcessing, req.Params)
		if callErr != nil {
			writeRPCErrorWithReason(w, req.ID, mcphealth.FormatError(callErr))
		} else {
			writeRPCResponse(w, req.ID, result)
		}
	default:
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// openGaugeDB opens a raw *sql.DB for the gauge store from the same StoreDSN
// the main store uses. The gauge store owns its own connection pool.
func openGaugeDB(dsn config.StoreDSN) (*sql.DB, error) {
	if dsn.Scheme != "mysql" && dsn.Scheme != "mariadb" {
		return nil, fmt.Errorf("gauge store requires mysql/mariadb, got %q", dsn.Scheme)
	}
	addr := dsn.Host
	if dsn.Port > 0 {
		addr = fmt.Sprintf("%s:%d", dsn.Host, dsn.Port)
	}
	driverDSN := fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&loc=UTC&time_zone=%%27%%2B00%%3A00%%27",
		dsn.User, dsn.Password, addr, dsn.Database)
	db, err := sql.Open("mysql", driverDSN)
	if err != nil {
		return nil, fmt.Errorf("open gauge db: %w", err)
	}
	db.SetMaxOpenConns(5)
	return db, nil
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

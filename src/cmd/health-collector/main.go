// Command health-collector polls token_ledger for per-agent token usage
// and writes agent_health_gauge rows. It is the sole writer of the full gauge
// row for Claude Code sessions (hookd separately targets just
// last_activity_ts/tool/display via GaugeStore.UpdateActivity — see
// chooseLastActivity below for how this collector avoids clobbering that on
// its next tick). Designed to run as a systemd timer or long-lived daemon
// with a 15-second poll interval.
//
// R2 exception E2: this collector reads token_ledger directly via *sql.DB
// rather than through the store interface — a recorded cross-concern read,
// not an untracked one.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	gaugemysql "github.com/bmjdotnet/teamster/internal/agenthealth/gauge/mysql"
	"github.com/bmjdotnet/teamster/internal/agenthealth/notify"
	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
	"github.com/bmjdotnet/teamster/internal/pricing"
	"github.com/bmjdotnet/teamster/internal/store"
	_ "github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/version"
)

const defaultInterval = 15 * time.Second

// statuslineStaleAfter bounds how long a POST /context-reported window
// survives without a fresh report before collectTick falls back to the
// model-name heuristic. Generous relative to statusLine's default 10s
// refreshInterval: an agent whose statusLine stops reporting (disabled
// mid-session, or the process exited) reverts to the heuristic instead of
// serving a frozen value forever.
const statuslineStaleAfter = 60 * time.Second

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("health-collector %s\n", version.String())
			return 0
		}
	}

	logger := logging.Init("health-collector")

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		return 1
	}

	if cfg.StoreDSN.Raw == "" {
		logger.Error("TEAMSTER_STORE_DSN is required")
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	st, err := store.Open(ctx, cfg.StoreDSN.Raw)
	if err != nil {
		logger.Error("store open failed", "error", err)
		return 1
	}
	defer st.Close()

	drvDSN, err := toDriverDSN(cfg.StoreDSN.Raw)
	if err != nil {
		logger.Error("parse DSN for gauge DB", "error", err)
		return 1
	}
	gaugeDB, err := sql.Open("mysql", drvDSN)
	if err != nil {
		logger.Error("open gauge DB", "error", err)
		return 1
	}
	defer gaugeDB.Close()
	if err := gaugeDB.PingContext(ctx); err != nil {
		logger.Error("gauge DB ping failed", "error", err)
		return 1
	}

	gs := gaugemysql.New(gaugeDB)
	engine := notify.NewEngine(notify.DefaultThresholdConfig())
	if cfg.HookServerURL != "" {
		nudgeURL := strings.TrimSuffix(cfg.HookServerURL, "/event") + "/nudge"
		engine.RegisterDelivery(notify.NewNudgeDelivery(nudgeURL))
	}
	compTracker := newCompositionTracker()
	teammateTracker := newTeammateContextTracker()

	prom := resolvePromClient(cfg)
	if prom == nil {
		logger.Info("prometheus tool-call enrichment disabled: no Prometheus URL resolvable")
	}
	var promWarned bool

	logger.Info("health-collector started",
		"host", cfg.Host, "interval", defaultInterval.String())
	collectLoop(ctx, st, gs, engine, compTracker, teammateTracker, prom, &promWarned, cfg.Host, defaultInterval)
	logger.Info("health-collector stopped")
	return 0
}

// resolvePromClient applies the same precedence as cmd/rollup: an explicit
// TEAMSTER_PROMETHEUS_URL override, else the configured Prometheus port on
// localhost, else nil (disabled — not every install runs Prometheus).
func resolvePromClient(cfg config.Config) *promClient {
	if cfg.PrometheusMode == "none" {
		return nil
	}
	promURL := os.Getenv("TEAMSTER_PROMETHEUS_URL")
	if promURL == "" && cfg.PrometheusPort != 0 {
		promURL = fmt.Sprintf("http://localhost:%d", cfg.PrometheusPort)
	}
	if promURL == "" {
		return nil
	}
	return newPromClient(promURL)
}

func collectLoop(ctx context.Context, st store.Store, gs gauge.GaugeStore, engine *notify.Engine, compTracker *compositionTracker, teammateTracker *teammateContextTracker, prom *promClient, promWarned *bool, host string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	highWater := make(map[string]time.Time)
	prevContext := make(map[string]int64)
	// costTotals accumulates SessionCostUSD purely in memory, never by
	// adding onto the persisted gauge row's existing value (see
	// collectTick) — a lead's row can carry a pre-039f718 statusLine-era
	// cost that already summed the ENTIRE session including every
	// teammate's spend. Accumulating token_ledger deltas on top of that
	// forever re-inflates the lead's total (and, transitively, the fleet
	// team-cost sum). Since highWater[k] starts unset for every key on
	// collector start, the first tick for any (session, agent) naturally
	// queries token_ledger's full history for that agent (since > zero
	// time) and seeds costTotals[k] with the true total — self-healing on
	// every collector restart too, not just this one-time transition.
	costTotals := make(map[string]float64)
	// tokensInTotals/tokensOutTotals mirror costTotals exactly: an in-memory
	// accumulator seeded purely from token_ledger deltas, never by adding
	// onto the persisted gauge row's TokensInTotal/TokensOutTotal. Reading
	// existing.TokensInTotal back and adding deltaIn on top of it double
	// counts on every collector restart, because highWater[k] is also unset
	// after a restart — the first post-restart tick's deltaIn is already the
	// agent's FULL token history (queried since the zero time), so adding
	// the already-persisted cumulative total on top doubles it, and every
	// subsequent restart doubles it again.
	tokensInTotals := make(map[string]int64)
	tokensOutTotals := make(map[string]int64)
	rosterIDs := make(map[string]string)
	teamNames := make(map[string]string)
	var lastSweep time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collectTick(ctx, st, gs, engine, compTracker, teammateTracker, prom, promWarned, host, highWater, prevContext, costTotals, tokensInTotals, tokensOutTotals, rosterIDs, teamNames)

			// Throttled to once per 1min, independent of the (much shorter)
			// poll interval — a closed session gets no further token_ledger
			// rows, so the collector never Upserts it again and its
			// updated_at simply stops advancing. That's what lets a 2h
			// cutoff naturally age it out here without any explicit
			// "session closed" signal.
			if time.Since(lastSweep) > time.Minute {
				cutoff := time.Now().Add(-2 * time.Hour)
				if n, err := gs.SweepOffline(ctx, cutoff); err != nil {
					slog.Warn("gauge sweep", "error", err)
				} else if n > 0 {
					slog.Info("swept offline gauge rows", "deleted", n)
				}
				lastSweep = time.Now()
			}
		}
	}
}

type ledgerRow struct {
	AgentName        string
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CacheWrite1h     int64
	CacheWrite5m     int64
	TotalInput       int64
	NText            int64
	NToolUse         int64
	NThinking        int64
	Timestamp        time.Time
}

func collectTick(ctx context.Context, st store.Store, gs gauge.GaugeStore, engine *notify.Engine, compTracker *compositionTracker, teammateTracker *teammateContextTracker, prom *promClient, promWarned *bool, host string, highWater map[string]time.Time, prevContext map[string]int64, costTotals map[string]float64, tokensInTotals map[string]int64, tokensOutTotals map[string]int64, rosterIDs map[string]string, teamNames map[string]string) {
	rx, ok := st.(store.RawExecutor)
	if !ok {
		slog.Warn("store does not support RawExecutor — cannot query token_ledger")
		return
	}

	sessions, err := discoverActiveSessions(ctx, rx)
	if err != nil {
		slog.Warn("discover active sessions", "error", err)
		return
	}

	// Per-session lead model/window, from statusline-sourced gauge rows
	// only. Used exclusively as teammateContextTracker.Update's last-resort
	// same-model fallback (a teammate whose own model-class window lookup
	// fails but is confirmed to be running the identical model string as the
	// lead borrows the lead's known window) — NOT a blanket inheritance for
	// every teammate, which is what this used to do (see removed
	// applyLeadWindowOverride, commit e587d3a): a teammate's context window
	// is now resolved from its OWN transcript first (see teammate_context.go).
	leadWindows := make(map[string]int64)
	leadModels := make(map[string]string)
	for _, sk := range sessions {
		if sk.AgentName != "" {
			continue
		}
		existing, found, err := gs.Get(ctx, gauge.GaugeKey{Host: host, SessionID: sk.SessionID, AgentName: ""})
		if err == nil && found && existing.ContextSource == gauge.ContextSourceStatusline && existing.ContextWindowTokens > 0 {
			leadWindows[sk.SessionID] = existing.ContextWindowTokens
			leadModels[sk.SessionID] = existing.Model
		}
	}

	// Fetched once per tick (per host), not once per session — the metric
	// carries no session_id label (internal/observability/metrics.go), so
	// there's nothing more specific to query per-session anyway.
	var toolCounts map[string]map[string]int64
	if prom != nil {
		tc, err := prom.ToolCounts(ctx, host)
		if err != nil {
			if !*promWarned {
				slog.Warn("prometheus tool-count query failed; leaving tool_calls_total/tool_call_counts_json unchanged until it recovers", "error", err)
				*promWarned = true
			}
		} else {
			toolCounts = tc
			*promWarned = false
		}
	}

	for _, sk := range sessions {
		k := sk.SessionID + "|" + sk.AgentName

		if _, ok := rosterIDs[k]; !ok {
			if rid, err := st.ResolveRosterID(ctx, sk.SessionID, sk.AgentName); err == nil {
				rosterIDs[k] = rid
			}
		}
		if _, ok := teamNames[k]; !ok {
			if rid, ok := rosterIDs[k]; ok && rid != "" {
				if entry, err := st.GetRosterEntry(ctx, rid); err == nil {
					teamNames[k] = entry.TeamName
				}
			}
		}

		since := highWater[k]

		rows, err := queryTokenLedger(ctx, rx, sk.SessionID, sk.AgentName, since)
		if err != nil {
			slog.Warn("query token_ledger", "session", sk.SessionID, "agent", sk.AgentName, "error", err)
			continue
		}
		if len(rows) == 0 {
			// No token_ledger data yet (token-scraper hasn't run, or genuinely
			// hasn't billed any tokens for this session). A gauge row may
			// already exist here — created by StatusLine's POST /context
			// (handleContextReport) before this collector's first pass ever
			// runs — but with an empty model, since only a subagentStatusLine
			// report ever carries one. Backfill just the model field from the
			// sessions table (now populated at hook registration — see
			// internal/server/server.go's dispatchObservability) so the
			// health API doesn't have to wait for token_ledger before
			// reporting a model. Read-modify-write on the existing row only:
			// there's no token_ledger data to recompute anything else from.
			backfillModelFromSession(ctx, st, gs, host, sk.SessionID, sk.AgentName)
			continue
		}

		latest := rows[len(rows)-1]

		var deltaIn, deltaOut int64
		for _, r := range rows {
			deltaIn += r.InputTokens
			deltaOut += r.OutputTokens
		}
		costTotals[k] += costForRows(rows)
		costUSD := costTotals[k]

		existing, existingFound, getErr := gs.Get(ctx, gauge.GaugeKey{Host: host, SessionID: sk.SessionID, AgentName: sk.AgentName})
		if getErr != nil {
			existingFound = false
		}
		tokensInTotals[k] += deltaIn
		tokensOutTotals[k] += deltaOut
		tokensIn := tokensInTotals[k]
		tokensOut := tokensOutTotals[k]
		var window, contextUsed int64
		var longCtx bool
		var fillPct float64
		var contextSource string
		var sessionTotalCost float64
		if sk.AgentName == "" {
			window, contextUsed, longCtx, fillPct, contextSource = chooseContextWindow(existing, existingFound, time.Now(), latest.TotalInput)
			// Computed once per session per tick (on the lead's row only,
			// never per teammate) directly from token_ledger — the durable
			// source of truth, unlike session_cost_usd/costTotals above
			// which is scoped to one agent_name and stops advancing (though
			// never reset) once that agent's own session leaves
			// discoverActiveSessions's active set. A teammate who finishes
			// and is later swept by SweepOffline's 2h cutoff still has
			// their spend counted here, because this sums every agent_name
			// in token_ledger for the session, not just currently-active
			// ones. ctop's team-total header should read this field on the
			// lead's row instead of summing per-agent SessionCostUSD across
			// gauge rows, which silently drops swept agents.
			if cost, err := querySessionTotalCost(ctx, rx, sk.SessionID); err != nil {
				slog.Warn("query session total cost", "session", sk.SessionID, "error", err)
				sessionTotalCost = existing.SessionTotalCostUSD
			} else {
				sessionTotalCost = cost
			}
		} else {
			leadWindow, leadWindowOK := leadWindows[sk.SessionID]
			result := teammateTracker.Update(sk.SessionID, sk.AgentName, leadModels[sk.SessionID], leadWindow, leadWindowOK)
			window, contextUsed, longCtx, fillPct, contextSource = result.Window, result.Used, result.LongCtx, result.FillPct, result.Source
		}
		free := window - contextUsed
		if free < 0 {
			free = 0
		}

		prevCtx := prevContext[k]
		resetSuspected := prevCtx > 0 && contextUsed < prevCtx/2
		prevContext[k] = contextUsed

		now := time.Now().UTC()
		lastActivityTs, lastActivityTool, lastActivityDisplay := chooseLastActivity(existing, existingFound, latest.Timestamp)

		agentKey := notify.AgentKey{
			Host:      host,
			SessionID: sk.SessionID,
			AgentName: sk.AgentName,
			RosterID:  rosterIDs[k],
			TeamName:  teamNames[k],
		}
		pressureLevel, pressureLevelSince := engine.Evaluate(ctx, agentKey, fillPct)

		row := gauge.GaugeRow{
			Host:                  host,
			SessionID:             sk.SessionID,
			AgentName:             sk.AgentName,
			Runtime:               "claude_code",
			Model:                 latest.Model,
			LongContextActive:     longCtx,
			ContextWindowTokens:   window,
			ContextTokensUsed:     contextUsed,
			ContextTokensFree:     free,
			ContextFillPct:        fillPct,
			ContextResetSuspected: resetSuspected,
			ContextSource:         contextSource,
			ContextReportedAt:     existing.ContextReportedAt,
			SessionCostUSD:        costUSD,
			SessionTotalCostUSD:   sessionTotalCost,
			StatuslineJSON:        existing.StatuslineJSON,
			TokensInTotal:         tokensIn,
			TokensOutTotal:        tokensOut,
			LastActivityTs:        &lastActivityTs,
			LastActivityTool:      lastActivityTool,
			LastActivityDisplay:   lastActivityDisplay,
			PressureLevel:         pressureLevel,
			PressureLevelSince:    &pressureLevelSince,
			CollectorStatus:       "fresh",
			UpdatedAt:             now,
		}
		if rid, ok := rosterIDs[k]; ok {
			row.RosterID = &rid
		}
		row.CompositionJSON = compTracker.Update(sk.SessionID, sk.AgentName)

		var freshToolCounts map[string]int64
		hasFreshToolCounts := false
		if toolCounts != nil {
			if fc, ok := toolCounts[sk.AgentName]; ok && len(fc) > 0 {
				freshToolCounts = fc
				hasFreshToolCounts = true
			}
		}
		row.ToolCallCountsJSON, row.ToolCallsTotal = chooseToolCounts(existing, existingFound, freshToolCounts, hasFreshToolCounts)

		if err := gs.Upsert(ctx, row); err != nil {
			slog.Warn("gauge upsert", "session", sk.SessionID, "agent", sk.AgentName, "error", err)
			continue
		}

		highWater[k] = latest.Timestamp
	}
}

// sessionModelReader is the minimal capability backfillModelFromSession
// needs from store.Store — narrower so it's fakeable in tests without
// implementing the full aggregate interface (Store composes a dozen+
// role-based sub-interfaces; collectTick's real store.Store argument
// already satisfies this structurally, no caller change required).
type sessionModelReader interface {
	GetSession(ctx context.Context, key store.SessionKey) (store.Session, error)
}

// gaugeModelStore is the minimal capability backfillModelFromSession needs
// from gauge.GaugeStore, for the same testability reason as
// sessionModelReader above.
type gaugeModelStore interface {
	Get(ctx context.Context, key gauge.GaugeKey) (gauge.GaugeRow, bool, error)
	Upsert(ctx context.Context, row gauge.GaugeRow) error
}

// backfillModelFromSession patches an existing gauge row's empty Model field
// from the sessions table, when there's no token_ledger data to derive one
// from (see collectTick's len(rows)==0 branch). No-op if there's no gauge
// row yet, the gauge row already has a model, or the sessions table doesn't
// have one either (sessions.model is now set at hook registration time —
// internal/server/server.go's dispatchObservability — but that's a
// settings.json default, not always available). Every other gauge field is
// left exactly as it was: there's nothing here to recompute without
// token_ledger data.
func backfillModelFromSession(ctx context.Context, st sessionModelReader, gs gaugeModelStore, host, sessionID, agentName string) {
	existing, found, err := gs.Get(ctx, gauge.GaugeKey{Host: host, SessionID: sessionID, AgentName: agentName})
	if err != nil || !found || existing.Model != "" {
		return
	}
	sess, err := st.GetSession(ctx, store.SessionKey{SessionID: sessionID, AgentName: agentName})
	if err != nil || sess.Model == "" {
		return
	}
	existing.Model = sess.Model
	if err := gs.Upsert(ctx, existing); err != nil {
		slog.Warn("backfill gauge model from session", "session", sessionID, "agent", agentName, "error", err)
	}
}

func discoverActiveSessions(ctx context.Context, rx store.RawExecutor) ([]store.SessionKey, error) {
	rows, err := rx.QueryRaw(ctx,
		`SELECT DISTINCT session_id, agent_name FROM sessions WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []store.SessionKey
	for rows.Next() {
		var sk store.SessionKey
		if err := rows.Scan(&sk.SessionID, &sk.AgentName); err != nil {
			return nil, err
		}
		result = append(result, sk)
	}
	return result, rows.Err()
}

func queryTokenLedger(ctx context.Context, rx store.RawExecutor, sessionID, agentName string, since time.Time) ([]ledgerRow, error) {
	rows, err := rx.QueryRaw(ctx, `
		SELECT agent_name, model, input_tokens, output_tokens,
		       cache_read_tokens, cache_write_tokens, cache_write_1h, cache_write_5m,
		       total_input, n_text, n_tool_use, n_thinking, timestamp
		FROM token_ledger
		WHERE session_id = ? AND agent_name = ? AND timestamp > ?
		ORDER BY timestamp`,
		sessionID, agentName, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ledgerRow
	for rows.Next() {
		var r ledgerRow
		if err := rows.Scan(
			&r.AgentName, &r.Model, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens, &r.CacheWrite1h, &r.CacheWrite5m,
			&r.TotalInput, &r.NText, &r.NToolUse, &r.NThinking, &r.Timestamp,
		); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// querySessionTotalCost sums token_ledger's own cost_usd column across every
// agent_name for a session — the durable full-session spend, unaffected by
// any individual agent's sessions.status or by SweepOffline deleting that
// agent's gauge row. Deliberately reads the persisted cost_usd column rather
// than recomputing via costForRows/internal/pricing: cost_usd was priced
// once by token-scraper at ingest time, so summing it needs no per-row
// pricing-table lookups (cheap even over a session's full history) and
// isn't affected by which pricing-table version this health-collector
// binary happens to be running — a stale-binary pricing gap (see
// internal/pricing/pricing.go's history of missing-entry defects) only
// affects costForRows's live recompute, never this stored-value sum, once
// token-scraper's own binary has the correct rates at ingest time.
func querySessionTotalCost(ctx context.Context, rx store.RawExecutor, sessionID string) (float64, error) {
	rows, err := rx.QueryRaw(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM token_ledger WHERE session_id = ?`,
		sessionID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var total float64
	if rows.Next() {
		if err := rows.Scan(&total); err != nil {
			return 0, err
		}
	}
	return total, rows.Err()
}

// costForRows sums each new ledger row's own per-row cost — component
// token columns (input/output/cache_read/cache_write, split by the 5m/1h
// cache-write TTL tier since they price differently) × internal/pricing
// rates, keyed by that row's OWN model (the real API model ID
// token-scraper recorded, never a sidecar alias). Deliberately never uses
// TotalInput: that column is occupancy-shaped (context-window fill), not
// cost-shaped, and would double- or under-count depending on cache state.
// token-scraper already dedups output_tokens growth across sidechain lines
// before writing token_ledger, so summing rows as-is here is correct — no
// further dedup needed. A pure function — no DB access — so it's
// unit-testable without mocking token_ledger.
func costForRows(rows []ledgerRow) float64 {
	var total float64
	for _, r := range rows {
		total += pricing.ComputeCost(r.Model, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWrite5m, r.CacheWrite1h)
	}
	return total
}

// defaultContextWindow is the LEAD's last-resort context window when no
// fresh StatusLine report exists for it (see chooseContextWindow) — a
// degraded estimate, marked as such via gauge.ContextSourceHeuristic, not a
// guess at the account's actual entitlement. The old model-name/host "[1m]"
// heuristic (contextWindowForModel/hostHasLongContext) is gone: StatusLine's
// own resolved, plan/entitlement-aware context_window_size is the only
// authoritative source for the lead now (see context-window-detection.md —
// a Pro-tier account without usage credits gets 200k even on a model whose
// name implies 1M, which the old heuristic got wrong). Teammates never reach
// this fallback — they have no StatusLine signal at all (subagentStatusLine
// doesn't fire for them) and are resolved entirely by
// teammateContextTracker (teammate_context.go) instead, landing on
// gauge.ContextSourceUnavailable rather than this flat default when their
// own transcript-derived signal isn't available.
var defaultContextWindow int64 = 200_000

// chooseContextWindow decides the LEAD's context window for one collectTick
// pass: trust a fresh statusLine-reported window (POST /context,
// gauge.ContextSourceStatusline) over the defaultContextWindow fallback.
// Never called for a teammate (see collectTick's sk.AgentName branch) — a
// pure function, no DB access, so the decision is unit-testable without
// mocking token_ledger/sessions queries.
//
// Prefers the existing gauge row's statusLine-sourced values when they exist
// and are younger than statuslineStaleAfter. Falls back to
// defaultContextWindow whenever no such row exists, it isn't
// statusLine-sourced, or it has gone stale — the lead's statusLine
// stopping (disabled mid-session, or the process exited) must not serve a
// frozen value forever.
func chooseContextWindow(existing gauge.GaugeRow, existingFound bool, now time.Time, heuristicUsed int64) (window, contextUsed int64, longCtx bool, fillPct float64, source string) {
	if existingFound && existing.ContextSource == gauge.ContextSourceStatusline && existing.ContextReportedAt != nil && now.Sub(*existing.ContextReportedAt) < statuslineStaleAfter {
		return existing.ContextWindowTokens, existing.ContextTokensUsed, existing.LongContextActive, existing.ContextFillPct, gauge.ContextSourceStatusline
	}

	window = defaultContextWindow
	contextUsed = heuristicUsed
	if window > 0 {
		fillPct = float64(contextUsed) / float64(window)
	}
	return window, contextUsed, false, fillPct, gauge.ContextSourceHeuristic
}

// chooseLastActivity decides last_activity_ts/tool/display for one
// collectTick agent. This collector has no fresh signal of its own for
// tool/display — that's hookd's job (GaugeStore.UpdateActivity, called
// per hook event) — so it always carries the existing row's values forward
// rather than zeroing them via its next full Upsert. ledgerTs (the latest
// token_ledger row's timestamp) is still a legitimate activity signal in its
// own right, so it wins only when it is newer than whatever hookd already
// recorded.
func chooseLastActivity(existing gauge.GaugeRow, existingFound bool, ledgerTs time.Time) (ts time.Time, tool, display string) {
	ts = ledgerTs
	if existingFound {
		tool = existing.LastActivityTool
		display = existing.LastActivityDisplay
		if existing.LastActivityTs != nil && existing.LastActivityTs.After(ts) {
			ts = *existing.LastActivityTs
		}
	}
	return ts, tool, display
}

// chooseToolCounts decides tool_call_counts_json/tool_calls_total for one
// collectTick agent, mirroring chooseContextWindow's "prefer fresh, else
// preserve" shape. A pure function — no DB/HTTP access — so it's
// unit-testable without mocking Prometheus or token_ledger.
//
// hasFresh must be false whenever Prometheus is unreachable for this tick,
// or this agent had no entries in an otherwise-successful query — both
// cases fall back to the existing row's values rather than zeroing them
// out, since teamster_tool_calls_total is a monotonic counter and a
// missing sample almost never means "reset to zero."
func chooseToolCounts(existing gauge.GaugeRow, existingFound bool, freshCounts map[string]int64, hasFresh bool) (countsJSON *string, total int64) {
	if hasFresh {
		data, err := json.Marshal(freshCounts)
		if err == nil {
			s := string(data)
			var sum int64
			for _, v := range freshCounts {
				sum += v
			}
			return &s, sum
		}
	}
	if existingFound {
		return existing.ToolCallCountsJSON, existing.ToolCallsTotal
	}
	return nil, 0
}

// toDriverDSN converts a mysql://user:pass@host:port/db URL to a
// go-sql-driver/mysql DSN string. Same logic as internal/store/mysql.convertDSN
// but callable from cmd/ without importing the unexported function.
func toDriverDSN(raw string) (string, error) {
	if !strings.HasPrefix(raw, "mysql://") {
		return "", fmt.Errorf("DSN must start with mysql://")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	cfg := mysqldriver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if u.User != nil {
		cfg.User = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cfg.Passwd = pw
		}
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Params = map[string]string{"time_zone": "'+00:00'"}
	for k, vs := range u.Query() {
		if len(vs) > 0 {
			cfg.Params[k] = vs[0]
		}
	}
	return cfg.FormatDSN(), nil
}

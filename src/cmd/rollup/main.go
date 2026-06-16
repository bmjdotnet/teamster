// Command rollup runs one attribution-aggregation pass: allocate
// usage_attribution (weights summing to 1 per message), rebuild cost_rollup,
// and reconcile per-session ledger cost against OTel. It is designed to be
// driven by a systemd timer every 5 minutes (run-once-and-exit), not as a
// long-lived daemon — each pass is idempotent.
//
// --sweep runs a full nightly deep-clean: entity hygiene (drain, gc,
// reclassify), then the full attribution pipeline (allocate + all recovery
// passes), then aggregation + reconciliation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bmjdotnet/teamster/internal/classify"
	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/rollup"
	"github.com/bmjdotnet/teamster/internal/store/mysql"
	"github.com/bmjdotnet/teamster/internal/wms"
)

func main() {
	os.Exit(run())
}

func run() int {
	reallocate := flag.Bool("reallocate", false,
		"clear unallocated attribution rows and re-derive them before the pass (recovery after agent-identity backfill)")
	reassembleIntervals := flag.Bool("reassemble-intervals", false,
		"opt-in one-time backfill: re-resolve interval_id for historical attribution rows so cost-by-phase covers existing work, then reassemble interval cost (the normal pass is forward-only)")
	recoverFocus := flag.Bool("recover-focus", false,
		"re-attribute method='unallocated' cost from the agent's intended-focus timeline read from the .claude transcripts (writes method='transcript_focus_recovery'); reversible with --unrecover")
	unrecover := flag.Bool("unrecover", false,
		"reverse a --recover-focus pass: delete every method='transcript_focus_recovery' row (and its evidence), returning those messages to the unallocated bucket")
	recoverWarmup := flag.Bool("recover-warmup", false,
		"re-attribute pre-first-setFocus warmup cost to the session's resolved outcome with phase=admin (writes method='admin_warmup'); reversible with --unrecover-warmup")
	uncoverWarmup := flag.Bool("unrecover-warmup", false,
		"reverse a --recover-warmup pass: delete every method='admin_warmup' row, its evidence, and synthetic admin intervals")
	recoverGaps := flag.Bool("recover-gaps", false,
		"re-attribute unallocated cost in partial-gap sessions by resolving the entity from the session's existing attributions (writes method='gap_recovery'); reversible with --unrecover-gaps")
	uncoverGaps := flag.Bool("unrecover-gaps", false,
		"reverse a --recover-gaps pass: delete every method='gap_recovery' row and its evidence")
	synthesizeFocus := flag.String("synthesize-focus", "",
		"re-attribute no-focus sessions using a JSON mapping file produced by the LLM orchestrator (writes method='synthesized_outcome'); reversible with --unsynthesize")
	unsynthesize := flag.Bool("unsynthesize", false,
		"reverse a --synthesize-focus pass: delete every method='synthesized_outcome' row and its evidence")
	sweep := flag.Bool("sweep", false,
		"nightly deep-clean: entity hygiene (drain, reclassify) + full attribution pipeline (allocate, recover-focus, recover-warmup, recover-gaps) + aggregation + reconciliation")
	sweepLLM := flag.Bool("sweep-llm", false,
		"with --sweep, also run LLM-assisted synthesis passes (orphan + gap fallback; uses claude --print)")
	dryRun := flag.Bool("dry-run", false,
		"with --sweep, --recover-focus, --recover-warmup, --recover-gaps, or --synthesize-focus, perform ZERO writes: log the plan and counts only")
	countOrphans := flag.Bool("count-orphans", false,
		"print the number of orphan sessions (unallocated, not yet synthesized) and exit; used by the sweep-llm timer guard")
	flag.Parse()

	if *sweepLLM && !*sweep {
		fmt.Fprintln(os.Stderr, "--sweep-llm requires --sweep")
		return 2
	}

	logger := logging.Init("rollup")

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		return 1
	}
	if cfg.StoreDSN.Primary == "" {
		logger.Error("TEAMSTER_STORE_DSN is required")
		return 1
	}

	st, err := mysql.New(cfg.StoreDSN.Primary)
	if err != nil {
		logger.Error("open store failed", "error", err)
		return 1
	}
	defer st.Close() //nolint:errcheck

	if *countOrphans {
		var count int
		err := st.DB().QueryRowContext(context.Background(),
			`SELECT COUNT(DISTINCT t.session_id)
			 FROM usage_attribution ua
			 JOIN token_ledger t ON t.message_id = ua.message_id
			 WHERE ua.method = 'unallocated'
			   AND t.session_id NOT IN (
			     SELECT DISTINCT t2.session_id
			     FROM usage_attribution ua2
			     JOIN token_ledger t2 ON t2.message_id = ua2.message_id
			     WHERE ua2.method = 'synthesized_outcome'
			   )`).Scan(&count)
		if err != nil {
			logger.Error("count-orphans query failed", "error", err)
			return 1
		}
		fmt.Println(count)
		return 0
	}

	// Reconciliation is enabled when a Prometheus URL is resolvable; otherwise
	// the pass runs allocation + rollup only. Default to the configured
	// Prometheus port on localhost; allow an explicit override.
	var otel rollup.OTelSource
	promURL := os.Getenv("TEAMSTER_PROMETHEUS_URL")
	if promURL == "" && cfg.PrometheusPort != 0 {
		promURL = fmt.Sprintf("http://localhost:%d", cfg.PrometheusPort)
	}
	if promURL != "" {
		otel = rollup.NewPromOTel(promURL)
		logger.Info("reconciliation enabled", "prometheus_url", promURL)
	} else {
		logger.Warn("reconciliation disabled: no Prometheus URL")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	r := rollup.New(st.DB(), otel, logger)

	if *sweep {
		return runSweep(ctx, st, r, cfg, logger, *sweepLLM, *dryRun)
	}

	// Opt-in historical backfill runs before the regular pass: it re-resolves
	// interval_id for already-attributed rows (which the forward-only Allocate
	// never revisits) so cost-by-phase covers existing work, then reassembles.
	if *reassembleIntervals {
		n, err := r.ReassembleIntervals(ctx)
		if err != nil {
			logger.Error("reassemble-intervals backfill failed", "error", err)
			return 1
		}
		logger.Info("reassemble-intervals backfill complete", "rows_populated", n)
	}
	// --unrecover / --unrecover-warmup run BEFORE the normal pass: they delete the
	// recovery rows so the following Allocate re-derives those messages as
	// unallocated, restoring the pre-recovery state.
	if *unrecover {
		n, err := r.Unrecover(ctx)
		if err != nil {
			logger.Error("unrecover failed", "error", err)
			return 1
		}
		logger.Info("unrecover complete", "reverted", n)
	}
	if *uncoverWarmup {
		n, err := r.UncoverWarmup(ctx)
		if err != nil {
			logger.Error("unrecover-warmup failed", "error", err)
			return 1
		}
		logger.Info("unrecover-warmup complete", "reverted", n)
	}
	if *uncoverGaps {
		n, err := r.UncoverGaps(ctx)
		if err != nil {
			logger.Error("unrecover-gaps failed", "error", err)
			return 1
		}
		logger.Info("unrecover-gaps complete", "reverted", n)
	}
	if *unsynthesize {
		n, err := r.Unsynthesize(ctx)
		if err != nil {
			logger.Error("unsynthesize failed", "error", err)
			return 1
		}
		logger.Info("unsynthesize complete", "reverted", n)
	}

	if err := r.Run(ctx, *reallocate); err != nil {
		logger.Error("rollup pass failed", "error", err)
		return 1
	}

	// --recover-focus runs AFTER the normal pass so it operates on freshly-derived
	// unallocated rows. --dry-run makes it perform zero writes (the live-DB
	// validation path). ProjectsDir defaults to $HOME/.claude/projects in the
	// transcript reader; SCRAPER_SESSION_GLOB-style overrides are not needed here
	// because the reader globs <projectsDir>/*/<sessionId>.jsonl itself.
	if *recoverFocus {
		stats, err := r.RecoverFocus(ctx, rollup.RecoverOptions{
			ProjectsDir: os.Getenv("TEAMSTER_CLAUDE_PROJECTS_DIR"),
			// Host-scope to THIS host+user: recovery can only read the .claude
			// transcripts physically present here. Sessions from another host/user
			// are deferred and logged (a host-local or future fetch-based pass
			// handles them), not mis-counted as irreducible warmup.
			Host:   cfg.Host,
			User:   cfg.User,
			DryRun: *dryRun,
		})
		if err != nil {
			logger.Error("recover-focus failed", "error", err)
			return 1
		}
		logger.Info("recover-focus complete",
			"sessions", stats.Sessions, "examined", stats.Examined,
			"recovered", stats.Recovered, "via_lead", stats.RecoveredLead,
			"unrecoverable", stats.Unrecoverable,
			"deferred_sessions", stats.Deferred, "deferred_messages", stats.DeferredMessages,
			"dry_run", *dryRun)
	}

	// --recover-warmup runs AFTER --recover-focus (which claims the post-first-
	// setFocus unallocated cost, leaving only the warmup residual as targets).
	if *recoverWarmup {
		stats, err := r.RecoverWarmup(ctx, rollup.RecoverOptions{
			ProjectsDir: os.Getenv("TEAMSTER_CLAUDE_PROJECTS_DIR"),
			Host:        cfg.Host,
			User:        cfg.User,
			DryRun:      *dryRun,
		})
		if err != nil {
			logger.Error("recover-warmup failed", "error", err)
			return 1
		}
		logger.Info("recover-warmup complete",
			"sessions", stats.Sessions, "examined", stats.Examined,
			"recovered", stats.Recovered, "no_outcome", stats.NoOutcome,
			"deferred_sessions", stats.Deferred, "deferred_messages", stats.DeferredMessages,
			"dry_run", *dryRun)
	}

	// --recover-gaps runs AFTER warmup: it resolves lead/teammate thread gaps
	// from the session's existing attributions (no transcript needed).
	if *recoverGaps {
		stats, err := r.RecoverGaps(ctx, *dryRun)
		if err != nil {
			logger.Error("recover-gaps failed", "error", err)
			return 1
		}
		logger.Info("recover-gaps complete",
			"threads", stats.Sessions, "examined", stats.Examined,
			"recovered", stats.Recovered, "skipped", stats.Skipped,
			"dry_run", *dryRun)
	}

	// --synthesize-focus consumes the LLM orchestrator's mapping file and writes
	// the deterministic attributions. Runs after all transcript-based recovery.
	if *synthesizeFocus != "" {
		stats, err := r.SynthesizeFocus(ctx, rollup.SynthesizeOptions{
			MappingFile: *synthesizeFocus,
			ProjectsDir: os.Getenv("TEAMSTER_CLAUDE_PROJECTS_DIR"),
			Host:        cfg.Host,
			User:        cfg.User,
			DryRun:      *dryRun,
		})
		if err != nil {
			logger.Error("synthesize-focus failed", "error", err)
			return 1
		}
		logger.Info("synthesize-focus complete",
			"mappings_loaded", stats.MappingsLoaded, "sessions", stats.Sessions,
			"examined", stats.Examined, "recovered", stats.Recovered,
			"skipped", stats.Skipped, "dry_run", *dryRun)
	}
	return 0
}

// runSweep executes the nightly deep-clean pipeline: entity hygiene, then the
// full attribution pipeline, then aggregation + reconciliation. Each step is
// idempotent — a re-run fixes 0 if nothing new to fix. The pipeline ordering
// matters: hygiene first (so dangling intervals don't pollute attribution),
// allocate before recovery (so recovery targets fresh unallocated rows).
func runSweep(ctx context.Context, st *mysql.Store, r *rollup.Runner, cfg config.Config, logger *slog.Logger, sweepLLM, dryRun bool) int {
	logger.Info("sweep: starting nightly attribution sweep", "dry_run", dryRun, "sweep_llm", sweepLLM)
	start := time.Now()

	recoverOpts := rollup.RecoverOptions{
		ProjectsDir: os.Getenv("TEAMSTER_CLAUDE_PROJECTS_DIR"),
		Host:        cfg.Host,
		User:        cfg.User,
		DryRun:      dryRun,
	}

	// --- Tier 1: Entity hygiene (deterministic, fast) ---

	// Step 1: Drain dangling intervals on terminal entities + closed sessions.
	if !dryRun {
		n, err := st.CloseIntervalsOnTerminalEntities(ctx)
		if err != nil {
			logger.Error("sweep: drain terminal entities failed", "error", err)
			return 1
		}
		if n > 0 {
			logger.Info("sweep: drained terminal-entity intervals", "closed", n)
		}

		n, err = st.CloseIntervalsForClosedSessions(ctx)
		if err != nil {
			logger.Error("sweep: drain closed sessions failed", "error", err)
			return 1
		}
		if n > 0 {
			logger.Info("sweep: drained closed-session intervals", "closed", n)
		}

		staleThreshold := time.Now().UTC().Add(-24 * time.Hour)
		n, err = st.CloseIntervalsForStaleSessions(ctx, staleThreshold)
		if err != nil {
			logger.Error("sweep: drain stale sessions failed", "error", err)
			return 1
		}
		if n > 0 {
			logger.Info("sweep: drained stale-session intervals", "closed", n, "threshold", staleThreshold)
		}
	} else {
		logger.Info("sweep (dry-run): would drain dangling intervals")
	}

	// Step 2: Reclassify phase/work-type on intervals the classifier missed.
	if !dryRun {
		cr := classify.New(st, wms.NewJSONLSignalReader(), cfg.LogFile, logger)
		if err := cr.Run(ctx, false, classify.DefaultReclassifyLimit); err != nil {
			logger.Warn("sweep: classify pass failed (non-fatal)", "error", err)
		} else {
			logger.Info("sweep: classify pass complete")
		}
	} else {
		logger.Info("sweep (dry-run): would run classify pass")
	}

	// --- Tier 2: Cost attribution (deterministic) ---

	// Step 3: Allocate (catch any 5-min-timer misses).
	if err := r.Run(ctx, false); err != nil {
		logger.Error("sweep: allocate pass failed", "error", err)
		return 1
	}
	logger.Info("sweep: allocate pass complete")

	// Step 4: Transcript-focus recovery.
	focusStats, err := r.RecoverFocus(ctx, recoverOpts)
	if err != nil {
		logger.Error("sweep: recover-focus failed", "error", err)
		return 1
	}
	logger.Info("sweep: recover-focus complete",
		"recovered", focusStats.Recovered, "unrecoverable", focusStats.Unrecoverable,
		"deferred", focusStats.Deferred)

	// Step 5: Warmup recovery.
	warmupStats, err := r.RecoverWarmup(ctx, recoverOpts)
	if err != nil {
		logger.Error("sweep: recover-warmup failed", "error", err)
		return 1
	}
	logger.Info("sweep: recover-warmup complete",
		"recovered", warmupStats.Recovered, "no_outcome", warmupStats.NoOutcome)

	// Step 6: Gap recovery.
	gapStats, err := r.RecoverGaps(ctx, dryRun)
	if err != nil {
		logger.Error("sweep: recover-gaps failed", "error", err)
		return 1
	}
	logger.Info("sweep: recover-gaps complete",
		"recovered", gapStats.Recovered, "skipped", gapStats.Skipped)

	// --- Tier 3: LLM-assisted attribution (gated) ---
	var llmStats rollup.SweepLLMStats
	if sweepLLM {
		var llmErr error
		llmStats, llmErr = r.SweepLLM(ctx, recoverOpts)
		if llmErr != nil {
			logger.Error("sweep: LLM synthesis failed (non-fatal)", "error", llmErr)
		} else {
			logger.Info("sweep: LLM synthesis complete",
				"orphans_synthesized", llmStats.OrphansSynthesized,
				"gaps_synthesized", llmStats.GapsSynthesized,
				"skipped", llmStats.Skipped,
				"errors", llmStats.Errors)
		}
	}

	elapsed := time.Since(start)
	logger.Info("sweep: nightly attribution sweep complete",
		"duration", elapsed.Round(time.Millisecond),
		"focus_recovered", focusStats.Recovered,
		"warmup_recovered", warmupStats.Recovered,
		"gap_recovered", gapStats.Recovered,
		"llm_synthesized", llmStats.OrphansSynthesized+llmStats.GapsSynthesized,
		"dry_run", dryRun)

	if !dryRun {
		writeSweepState(cfg.DataDir, elapsed, focusStats.Recovered, warmupStats.Recovered, gapStats.Recovered)
	}
	return 0
}

func writeSweepState(dataDir string, elapsed time.Duration, focusRecovered, warmupRecovered, gapRecovered int) {
	s := observability.SweepState{
		LastRunTimestamp: float64(time.Now().Unix()),
		DurationSeconds: elapsed.Seconds(),
		RecoveredTotal: map[string]float64{
			"transcript_focus_recovery": float64(focusRecovered),
			"admin_warmup":              float64(warmupRecovered),
			"gap_recovery":              float64(gapRecovered),
		},
	}
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(dataDir, "sweep-state.json"), data, 0o644)
}

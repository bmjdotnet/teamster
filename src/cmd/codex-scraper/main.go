// Command codex-scraper tails Codex CLI rollout JSONL files
// (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl and ~/.codex/archived_sessions/)
// and is the sole writer of Codex cost/ledger data: it POSTs per-token_count
// telemetry rows to hookd's /telemetry endpoint (same contract token-scraper
// uses for Claude Code) and, since hookd's hook-event pipeline never fires
// for Codex, upserts the Codex sessions row itself via hookd's POST /session
// endpoint — the same HTTP transport as /telemetry, so this binary needs no
// store DSN of its own.
//
// Oneshot, not a daemon: driven by a systemd timer (mirrors classify), not a
// poll loop (unlike token-scraper). Idempotent — safe to run concurrently
// with itself only in the sense that a second run before the first exits
// would re-read from the same persisted cursor; systemd's default oneshot
// semantics (one instance at a time) is assumed, same as classify/rollup.
//
// Scope boundaries (v1): solo-only (no Codex Agent Teams), --ephemeral exec
// runs are invisible (Codex skips persisting their rollout file entirely),
// and mcp_tool_call_end / response_item(function_call) events are parsed for
// correctness (Ok/Err discrimination, schema understanding) but not yet
// shipped as telemetry — no wire contract for per-tool-call Codex activity
// exists yet; the ledger (token_count) and sessions-row paths are the v1
// deliverable.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
	"github.com/bmjdotnet/teamster/internal/version"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("codex-scraper %s\n", version.String())
			return 0
		}
	}

	logger := logging.Init("codex-scraper")

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(os.Getenv("HOME"), ".codex")
	}
	roots := []string{
		filepath.Join(codexHome, "sessions"),
		filepath.Join(codexHome, "archived_sessions"),
	}
	if v := os.Getenv("CODEX_SCRAPER_SESSION_ROOTS"); v != "" {
		roots = strings.Split(v, ",")
	}

	dryRun := os.Getenv("SCRAPER_DRY_RUN") == "true" || os.Getenv("SCRAPER_DRY_RUN") == "1"

	// hookd base URL: HookServerURL is "http://host:9125/event"; /telemetry
	// and /session are sibling endpoints on the same hookd instance.
	hookdBase := strings.TrimSuffix(cfg.HookServerURL, "/event")

	telemetryURL := os.Getenv("TEAMSTER_TELEMETRY_URL")
	if telemetryURL == "" {
		telemetryURL = hookdBase + "/telemetry"
	}

	sessionURL := os.Getenv("TEAMSTER_SESSION_URL")
	if sessionURL == "" {
		sessionURL = hookdBase + "/session"
	}

	cursorPath := filepath.Join(cfg.DataDir, "codex-scraper-cursors.json")

	httpClient := &http.Client{Timeout: 5 * time.Second}

	logger.Info("starting",
		"roots", roots,
		"telemetry_url", telemetryURL,
		"session_url", sessionURL,
		"dry_run", dryRun,
	)

	s := &scraper{
		client:       httpClient,
		telemetryURL: telemetryURL,
		host:         cfg.Host,
		username:     cfg.User,
		roots:        roots,
		cursorPath:   cursorPath,
		dryRun:       dryRun,
		cursors:      make(map[string]*cursorEntry),
		st:           &httpSessionUpserter{client: httpClient, sessionURL: sessionURL},
	}

	if err := s.loadCursors(); err != nil {
		logger.Warn("loading cursors failed, starting fresh", "error", err)
	}

	if err := s.poll(ctx); err != nil {
		logger.Error("poll error", "error", err)
		return 1
	}
	return 0
}

// Command mcp-scraper tails hookd's events.jsonl, filters to completed
// (PostToolUse) mcp__* tool calls, and ledgers one row per call into
// claude_telemetry.mcp_tool_calls — a queryable call-volume instrument for
// WP12's reduction work (WP11-mcp-telemetry-capture.md §2).
//
// Oneshot, not a daemon: driven by a systemd timer (mirrors classify/
// codex-scraper), not a poll loop. events.jsonl is hookd's own JSONL sink —
// wms-mcp itself never writes telemetry, so this tailer's only data source
// is the file, never a direct read of any MCP server.
//
// The ledger table is created at install time (lib/installrunner.sh,
// immediately after claude_telemetry's own provisioning block) only under
// --store-mode=install. Under --store-mode=managed or --store-mode=external,
// neither of which runs install-time DDL, this binary's own CREATE TABLE IF
// NOT EXISTS below is the sole mechanism — it creates the table on first
// enabled run, which requires the DB credential to hold CREATE on
// claude_telemetry.
//
// Single-writer invariant: exactly one mcp-scraper process writes
// mcp_tool_calls, ever — the hub's. teamster-mcp-scraper.timer is
// permanently masked on every `teamster clone` target for this reason (a
// clone must not ledger its own local traffic as if it were the hub's,
// corrupting the volume measurement this table exists to support) — see
// cmd/teamster/clone_data.go's temporaryQuiesceUnits entry for this binary.
// The uq_source_pos unique key has no host column because this invariant
// makes one unnecessary: there is never more than one writer to collide.
//
// Out of scope (WP11's own "Known gap not fixed here"): no call's
// tool_input/entity-id arguments are captured here or anywhere upstream —
// mcp_tool_calls is a volume instrument, not an audit trail.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

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
			fmt.Printf("mcp-scraper %s\n", version.String())
			return 0
		}
	}

	logger := logging.Init("mcp-scraper")

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		return 1
	}

	// Checked first, unconditionally — defense-in-depth against a systemd
	// timer left enabled after the operator toggled Enabled back off
	// (mirrors cmd/teamster/wms_review_sweep.go's own run-time gate,
	// WP11-TAILER-DESIGN.md §7 site 7).
	if !cfg.MCPScraper.Enabled {
		logger.Info("mcp-scraper: disabled (mcp-scraper.enabled=false in teamster.yaml)")
		return 0
	}

	if cfg.StoreDSN.Raw == "" {
		logger.Error("TEAMSTER_STORE_DSN is required")
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	driverDSN, err := driverDSNForDatabase(cfg.StoreDSN.Raw, "claude_telemetry")
	if err != nil {
		logger.Error("building claude_telemetry DSN failed", "error", err)
		return 1
	}
	db, err := sql.Open("mysql", driverDSN)
	if err != nil {
		logger.Error("open claude_telemetry failed", "error", err)
		return 1
	}
	defer db.Close() //nolint:errcheck
	if err := db.PingContext(ctx); err != nil {
		logger.Error("claude_telemetry ping failed", "error", err)
		return 1
	}

	// Defensive no-op given lib/installrunner.sh's install-time DDL (see
	// package doc); only matters for a host upgraded from a pre-this-WU
	// binary, or an operator who manually dropped the table.
	if _, err := db.ExecContext(ctx, createTableSQL); err != nil {
		logger.Error("create mcp_tool_calls table failed", "error", err)
		return 1
	}

	ls := dbLedgerStore{db: db}
	s := &scraper{
		store:      ls,
		logPath:    cfg.LogFile,
		cursorPath: filepath.Join(cfg.DataDir, "mcp-scraper-cursor.json"),
	}

	cursor, ok, loadErr := s.loadCursor()
	if loadErr != nil {
		logger.Warn("mcp-scraper: loading cursor failed, treating as no persisted cursor", "error", loadErr)
	}
	if !ok {
		fresh, err := seedFreshCursor(ctx, s.logPath, ls)
		if err != nil {
			logger.Error("mcp-scraper: recovering generation from ledger table failed", "error", err)
			return 1
		}
		cursor = fresh
	}
	s.cursor = cursor

	if err := s.poll(ctx); err != nil {
		logger.Error("mcp-scraper: poll failed", "error", err)
		return 1
	}
	return 0
}

// driverDSNForDatabase converts a mysql://user:pass@host:port/db URL into a
// go-sql-driver/mysql DSN targeting database db instead of the URL's own
// path segment. mcp-scraper connects to claude_telemetry, a sibling of the
// app DSN's teamster database (WP11-TAILER-DESIGN.md §3's DSN construction).
// A small, deliberate third copy of the pattern in
// internal/backup/mysql.go's dsnForDatabase (URL-level database-name
// surgery) and cmd/health-collector/main.go's toDriverDSN (URL → driver
// DSN) — combined here since mcp-scraper needs both halves in one step, and
// this codebase's own established style for this operation is a small local
// copy per caller, not a shared export (internal/clonedata.VerifyRODSN's own
// doc comment says the same of its copy).
func driverDSNForDatabase(raw, db string) (string, error) {
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
	cfg.DBName = db
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

// Command classify runs one classification pass: derive work-type on workunits
// (reusing the inline RuleClassifier rules) and derive phase on each closed
// interval (the new B4 output, written to wms_intervals.phase). It is
// designed to be driven by a systemd timer every 5 minutes (run-once-and-exit),
// not as a long-lived daemon — each pass is idempotent.
//
// --reclassify clears classifier-derived phases (phase_source='classifier'
// only; declared phases are never touched) before the pass, so phase is
// re-derived from scratch with the current rules — the recovery path after a
// rules change or a signal backfill.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/bmjdotnet/teamster/internal/classify"
	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
	"github.com/bmjdotnet/teamster/internal/store"
	_ "github.com/bmjdotnet/teamster/internal/store/mysql" // registers mysql, mariadb
	"github.com/bmjdotnet/teamster/internal/wms"
)

func main() {
	os.Exit(run())
}

func run() int {
	reclassify := flag.Bool("reclassify", false,
		"clear classifier-derived interval phases and re-derive them with the current rules (declared phases are never touched)")
	dryRun := flag.Bool("dry-run", false,
		"with lifecycle tag recovery: log what would be applied but perform no writes")
	flag.Parse()

	logger := logging.Init("classify")

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
		logger.Error("open store failed", "error", err)
		return 1
	}
	defer st.Close() //nolint:errcheck

	reclassifyLimit := classify.DefaultReclassifyLimit
	if v := os.Getenv("TEAMSTER_RECLASSIFY_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			reclassifyLimit = n
		} else {
			logger.Warn("TEAMSTER_RECLASSIFY_LIMIT invalid, using default", "value", v, "default", reclassifyLimit)
		}
	}

	r := classify.New(st, wms.NewJSONLSignalReader(), cfg.LogFile, logger)
	if err := r.Run(ctx, *reclassify, reclassifyLimit, *dryRun); err != nil {
		logger.Error("classify pass failed", "error", err)
		return 1
	}
	return 0
}

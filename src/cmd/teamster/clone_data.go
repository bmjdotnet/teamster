package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bmjdotnet/teamster/internal/backup"
	"github.com/bmjdotnet/teamster/internal/clone"
	"github.com/bmjdotnet/teamster/internal/clonedata"
	"github.com/bmjdotnet/teamster/internal/logging"
)

// WP3 (Leg 3, DESIGN.md §4 / WP3-data-leg.md §2): the data pipeline that
// runs between WP2's install and the clone's first `teamster start`. Order:
//
//  1. assert the target's schema is at this binary's max known version
//     (installrunner.sh's own migration step is non-fatal, DESIGN.md gap #10)
//  2. resolve the source snapshot to transfer — pin `latest` by default
//     (I-B: no write against the source unless --fresh-backup is explicit)
//  3. transfer the candidate files (never config.tar.gz — I1) push-only to
//     a fresh directory on target
//  4. stop the daemons that would race an empty-then-restored schema
//  5. invoke `teamster restore <dir> --force` on the target
//  6. restart those daemons (sweep/backup stay masked permanently — R11,
//     handled by maskDisposableTimers in clone_install.go, unrelated to
//     this temporary stop/restart pair)
//  7. verify row counts source vs target, source-side via the read-only
//     clone_verify_ro credential (I7), never the app DSN

// temporaryQuiesceUnits are stopped before restore and restarted after —
// distinct from teamster-sweep.timer/teamster-backup.timer/
// teamster-wms-review-sweep.timer/teamster-mcp-scraper.timer, which
// maskDisposableTimers permanently masks and never restarts (R11, I-E).
// These three connect to MySQL directly and fire within seconds of --wire
// completing (OnBootSec has almost certainly already elapsed by the time
// this pipeline reaches them — WP3-data-leg.md §2 step 4), so the restore
// window must be protected from them. teamster-mcp-scraper.timer is
// deliberately NOT in this list even though it shares that same direct-MySQL
// exposure (WP11-TAILER-DESIGN.md §3's DSN construction): maskDisposableTimers
// already masks it permanently before this pipeline's restore window opens
// (clone.go calls maskDisposableTimers ahead of stopTemporaryDaemons), so a
// temporary stop/restart here is redundant at best — and actively wrong,
// since restartTemporaryDaemons' `systemctl start` on an already-masked unit
// returns rc=1 and fails the whole clone. Codex's own HTTP-only scraper needs
// no entry here for a different reason: it never opens a direct DB
// connection at all.
var temporaryQuiesceUnits = []string{
	"teamster-rollup.timer",
	"teamster-classify.timer",
	"teamster-health-collector.service",
}

// stopTemporaryDaemons stops the units in temporaryQuiesceUnits — a
// temporary quiesce for the restore window only, restarted in
// restartTemporaryDaemons once restore + verification complete.
func stopTemporaryDaemons(ctx context.Context, sshRun clone.SSHRunner, target string) error {
	args := append([]string{"sudo", "systemctl", "stop"}, temporaryQuiesceUnits...)
	if _, err := sshRun(ctx, target, args...); err != nil {
		return fmt.Errorf("clone: stopping %s on %s: %w", strings.Join(temporaryQuiesceUnits, ", "), target, err)
	}
	return nil
}

// restartTemporaryDaemons restarts the units stopTemporaryDaemons stopped —
// the clone needs them running to function as a usable WMS dev instance.
func restartTemporaryDaemons(ctx context.Context, sshRun clone.SSHRunner, target string) error {
	args := append([]string{"sudo", "systemctl", "start"}, temporaryQuiesceUnits...)
	if _, err := sshRun(ctx, target, args...); err != nil {
		return fmt.Errorf("clone: restarting %s on %s: %w", strings.Join(temporaryQuiesceUnits, ", "), target, err)
	}
	return nil
}

// ErrSchemaVersionMismatch means the target's schema_version table doesn't
// report the exact max version this binary's install shipped —
// installrunner.sh's own migration step is non-fatal under set -euo
// pipefail (DESIGN.md gap #10), so a clean install exit code alone doesn't
// guarantee the schema is actually there. This is WP3-data-leg.md §2's step
// 1.5, and it must hard-fail before Leg 3 (restore) is allowed to start.
var ErrSchemaVersionMismatch = errors.New("clone: target schema_version does not match this binary's max known version")

// assertTargetSchemaCurrent compares SELECT MAX(version) FROM schema_version
// on the target (over SSH, using the target's own already-provisioned app DSN
// — the target is disposable, so this carries no I7 concern) against the
// target binary's own `store schema-version`. The expectation must come from
// the shipped binary, never the driver's mysql.MaxSchemaVersion(): the driver
// may be a different commit than the shipped ref (Stage A only proves the
// target matches the shipped ref).
func assertTargetSchemaCurrent(ctx context.Context, sshRun clone.SSHRunner, target, binaryPath string) error {
	wantOut, err := sshRun(ctx, target, binaryPath, "store", "schema-version")
	if err != nil {
		return fmt.Errorf("%w: querying %s's compiled schema version on %s (a ref predating `store schema-version` cannot be cloned): %v\n%s", ErrSchemaVersionMismatch, binaryPath, target, err, tailLines(wantOut, 20))
	}
	wantMaxVersion, perr := strconv.Atoi(strings.TrimSpace(wantOut))
	if perr != nil {
		return fmt.Errorf("%w: unexpected schema-version output %q on %s: %v", ErrSchemaVersionMismatch, strings.TrimSpace(wantOut), target, perr)
	}
	out, err := sshRun(ctx, target, binaryPath, "sql", "-N", "-e", "SELECT MAX(version) FROM schema_version")
	if err != nil {
		return fmt.Errorf("%w: querying schema_version on %s: %v\n%s", ErrSchemaVersionMismatch, target, err, tailLines(out, 20))
	}
	got, perr := strconv.Atoi(strings.TrimSpace(out))
	if perr != nil {
		return fmt.Errorf("%w: unexpected schema_version output %q on %s: %v", ErrSchemaVersionMismatch, strings.TrimSpace(out), target, perr)
	}
	if got != wantMaxVersion {
		return fmt.Errorf("%w: target %s reports schema v%d, its binary's install shipped v%d — installrunner.sh's migration step may have failed silently (its own exit code is not a reliable signal, DESIGN.md gap #10)", ErrSchemaVersionMismatch, target, got, wantMaxVersion)
	}
	return nil
}

// resolveSourceSnapshot resolves the source's own backup_dir/latest symlink
// — a local, read-only path resolution (R9: clone runs from the source
// host itself). This is the default per I-B: pinning `latest` (which the
// source's own hourly timer already maintains) keeps the source's write
// path untouched, in contrast to an earlier design that defaulted to
// triggering a fresh backup before every clone.
func resolveSourceSnapshot(cfg *backup.Config) (string, error) {
	latest := filepath.Join(cfg.BackupDir, "latest")
	resolved, err := filepath.EvalSymlinks(latest)
	if err != nil {
		return "", fmt.Errorf("clone: resolving source snapshot %s: %w", latest, err)
	}
	return resolved, nil
}

// triggerFreshSourceBackup runs a real backup on the source and returns the
// resulting snapshot directory — the --fresh-backup opt-in (I-B). This is a
// write against the source's own backup mechanism (never schema/data), and
// it is deliberately not the default.
func triggerFreshSourceBackup(ctx context.Context, cfg *backup.Config, configPath string) (string, error) {
	logger := logging.Init("clone")
	if err := backup.Run(ctx, cfg, configPath, false, logger); err != nil {
		return "", fmt.Errorf("clone: triggering fresh source backup: %w", err)
	}
	return resolveSourceSnapshot(cfg)
}

// transferSnapshot pushes each of clonedata.TransferCandidates's files from
// the local source snapshot to a fresh directory on target — a real copy
// (I6: push-only, source→target). Explicitly not a read off the source's
// NFS mount — even though the target may share the same export, clone
// always does a genuine scp, and remoteDir is always outside the shared
// mount (the I5×I6 trap, DESIGN.md §6).
func transferSnapshot(ctx context.Context, deps clone.Deps, snapshotDir, target, remoteDir string) error {
	candidates, err := clonedata.TransferCandidates(snapshotDir)
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	if _, err := deps.SSHRun(ctx, target, "mkdir", "-p", remoteDir); err != nil {
		return fmt.Errorf("clone: creating remote transfer dir %s on %s: %w", remoteDir, target, err)
	}
	madeDirs := map[string]bool{}
	for _, rel := range candidates {
		remoteParent := path.Dir(rel)
		if remoteParent != "." && !madeDirs[remoteParent] {
			if _, err := deps.SSHRun(ctx, target, "mkdir", "-p", remoteDir+"/"+remoteParent); err != nil {
				return fmt.Errorf("clone: creating remote dir %s on %s: %w", remoteParent, target, err)
			}
			madeDirs[remoteParent] = true
		}
		local := filepath.Join(snapshotDir, filepath.FromSlash(rel))
		if _, err := os.Stat(local); err != nil {
			return fmt.Errorf("clone: transfer candidate not found locally: %s: %w", local, err)
		}
		if err := deps.Upload(ctx, local, target, remoteDir+"/"+rel); err != nil {
			return fmt.Errorf("clone: transferring %s to %s: %w", rel, target, err)
		}
	}
	return nil
}

// invokeRestore runs `teamster restore --force <remoteDir>` on the target.
// --force must precede the positional directory argument: Go's flag package
// stops recognizing flags at the first non-flag token, so `restore <dir>
// --force` would silently leave force=false, fall through to the
// interactive confirmation prompt, and hang forever inside this SSH
// pipeline with nothing to read the prompt (verified empirically against
// flag.FlagSet.Parse — this is not a stylistic choice). --force here is
// teamster restore's own CLI flag (suppresses that prompt); it is not the
// mysql client's --force flag, which runMysqlImport no longer passes
// (S1/I-F). Runs against the target's own teamster.yaml, written by
// install and never overwritten by this restore since config.tar.gz was
// never transferred (I1).
func invokeRestore(ctx context.Context, sshRun clone.SSHRunner, target, binaryPath, remoteDir string) error {
	if _, err := sshRun(ctx, target, binaryPath, "restore", "--force", remoteDir); err != nil {
		return fmt.Errorf("clone: restore on %s failed: %w", target, err)
	}
	return nil
}

// cloneVerifyROPasswordPath is where installrunner.sh's
// provision_clone_verify_ro persists the clone_verify_ro password on the
// SOURCE host — read locally (R9), never transferred.
func cloneVerifyROPasswordPath(sourceBasedir string) string {
	return filepath.Join(sourceBasedir, "var", "clone", "clone_verify_ro_password")
}

// ErrCloneVerifyRONotProvisioned means the clone_verify_ro password file
// doesn't exist on this source host — installrunner.sh's
// provision_clone_verify_ro either hasn't run yet (this host predates the
// I7 fix and needs an upgrade) or failed. Row-count verification cannot
// proceed without it: the app DSN must never be used for source-side
// queries (I7).
var ErrCloneVerifyRONotProvisioned = errors.New("clone: clone_verify_ro is not provisioned on this source host — upgrade this instance (lib/installrunner.sh now provisions it automatically) before running teamster clone")

func readCloneVerifyROPassword(sourceBasedir string) (string, error) {
	data, err := os.ReadFile(cloneVerifyROPasswordPath(sourceBasedir))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCloneVerifyRONotProvisioned, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// runLocalSQL runs `<binaryPath> sql [--read-only] -N -e <query>` as a
// local subprocess with TEAMSTER_STORE_DSN=dsn in its environment — R9:
// clone's own source-side verification queries are always local, never
// SSH. The DSN (which carries the clone_verify_ro password) is passed only
// via the environment, never argv, matching teamster sql's own
// off-argv-credential discipline.
func runLocalSQL(ctx context.Context, binaryPath, dsn, query string, readOnly bool) (string, error) {
	args := []string{"sql"}
	if readOnly {
		args = append(args, "--read-only")
	}
	args = append(args, "-N", "-e", query)
	cmd := exec.CommandContext(ctx, binaryPath, args...)
	cmd.Env = append(os.Environ(), "TEAMSTER_STORE_DSN="+dsn)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("local %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// verifyRowCounts is WP3-data-leg.md §4's post-restore gate: for each of
// teamster/claude_telemetry, enumerate every table via SHOW TABLES on both
// sides (never a hand-picked subset — I-F) and diff row counts. Source-side
// queries run locally through the clone_verify_ro read-only credential
// (I7); target-side queries run over SSH through the target's own
// already-provisioned app DSN (the target is disposable, no I7 concern
// there). Returns a non-empty mismatch list rather than an error when the
// counts genuinely differ — an error return means verification itself
// couldn't run, not that it found a problem.
func verifyRowCounts(ctx context.Context, sshRun clone.SSHRunner, localBinary, targetBinary, target, sourceAppDSN, cvroPassword string) ([]string, error) {
	var allMismatches []string
	for _, db := range []string{"teamster", "claude_telemetry"} {
		roDSN, err := clonedata.VerifyRODSN(sourceAppDSN, cvroPassword, db)
		if err != nil {
			return nil, fmt.Errorf("clone: building clone_verify_ro DSN for %s: %w", db, err)
		}

		srcTablesOut, err := runLocalSQL(ctx, localBinary, roDSN, "SHOW TABLES", true)
		if err != nil {
			return nil, fmt.Errorf("clone: source SHOW TABLES on %s: %w", db, err)
		}
		srcTables := clonedata.ParseTableList(srcTablesOut)

		tgtTablesOut, err := sshRun(ctx, target, targetBinary, "sql", "-N", "--database="+db, "-e", "SHOW TABLES")
		if err != nil {
			return nil, fmt.Errorf("clone: target SHOW TABLES on %s: %w", db, err)
		}
		tgtTables := clonedata.ParseTableList(tgtTablesOut)

		srcCounts := make(map[string]int64, len(srcTables))
		for _, tbl := range srcTables {
			out, err := runLocalSQL(ctx, localBinary, roDSN, countStmt(tbl), true)
			if err != nil {
				return nil, fmt.Errorf("clone: source COUNT(*) on %s.%s: %w", db, tbl, err)
			}
			n, err := clonedata.ParseCount(out)
			if err != nil {
				return nil, fmt.Errorf("clone: source COUNT(*) on %s.%s: %w", db, tbl, err)
			}
			srcCounts[tbl] = n
		}

		tgtCounts := make(map[string]int64, len(tgtTables))
		for _, tbl := range tgtTables {
			out, err := sshRun(ctx, target, targetBinary, "sql", "-N", "--database="+db, "-e", countStmt(tbl))
			if err != nil {
				return nil, fmt.Errorf("clone: target COUNT(*) on %s.%s: %w", db, tbl, err)
			}
			n, err := clonedata.ParseCount(out)
			if err != nil {
				return nil, fmt.Errorf("clone: target COUNT(*) on %s.%s: %w", db, tbl, err)
			}
			tgtCounts[tbl] = n
		}

		for _, d := range clonedata.RowCountDiff(srcCounts, tgtCounts) {
			allMismatches = append(allMismatches, db+"."+d)
		}
	}
	return allMismatches, nil
}

// countStmt builds a COUNT(*) statement over a backtick-quoted table name —
// table names here always come from this server's own SHOW TABLES output,
// never external input.
func countStmt(table string) string {
	return "SELECT COUNT(*) FROM `" + table + "`"
}

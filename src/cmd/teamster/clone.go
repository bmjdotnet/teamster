package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmjdotnet/teamster/internal/backup"
	"github.com/bmjdotnet/teamster/internal/clone"
	"github.com/bmjdotnet/teamster/internal/clonetopology"
	"github.com/bmjdotnet/teamster/internal/store/mysql"
)

const cloneUsage = `usage: teamster clone [OPTIONS] <user>@<host>

Stands up a disposable Teamster instance running the same commit and data
as this instance, on a target host reached over SSH. Runs from the source
host (this one) and pushes outward — never installs on, migrates, or
mutates the source.

Code source (mutually exclusive; --repo-dir is the default mode):
  --repo-dir=<path>      ship from an on-disk repo or worktree
  --github-repo=<url>    fetch from GitHub; defaults to the canonical
                         bmjdotnet/teamster, accepts an alternate fork

Source instance:
  --source=<url>         instance to clone (default: the local instance)
  --ref=<commit>         override the detected commit

Safety:
  --dry-run              resolve and print the full plan, change nothing
  --allow-dirty          permit shipping a dirty working tree (default: refuse)
  --yes                  non-interactive (skip confirmation prompts)

Data:
  --fresh-backup         trigger a fresh backup on the source before cloning,
                         instead of pinning the existing 'latest' snapshot
                         (a write against the source; off by default)

Ships verified source, translates the source instance's topology into an
installer flag set, invokes the installer, and independently verifies the
result (ref resolution, transport, provenance, topology translation,
install, schema verification, permanent sweep/backup masking). Then moves
the data: transfers the source's backup snapshot, restores it on the
target, and verifies row counts match before bringing the clone online.
`

// runClone dispatches `teamster clone`.
func runClone(args []string) int {
	fs := flag.NewFlagSet("teamster clone", flag.ContinueOnError)
	repoDir := fs.String("repo-dir", "", "ship from an on-disk repo or worktree")
	githubRepo := fs.String("github-repo", "", "fetch from GitHub (optionally a fork URL)")
	source := fs.String("source", "", "instance to clone (default: the local instance)")
	ref := fs.String("ref", "", "override the detected commit")
	dryRun := fs.Bool("dry-run", false, "resolve and print the full plan, change nothing")
	allowDirty := fs.Bool("allow-dirty", false, "permit shipping a dirty working tree")
	yes := fs.Bool("yes", false, "non-interactive")
	freshBackup := fs.Bool("fresh-backup", false, "trigger a fresh backup on the source before cloning, instead of pinning the existing 'latest' snapshot (I-B: a write against the source; off by default)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, cloneUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = yes // no interactive confirmation exists yet in WP1's scope; accepted for CLI-surface completeness (DESIGN.md §2) ahead of WP2/WP3 wiring it up

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "clone: expected exactly one argument: <user>@<host>")
		fs.Usage()
		return 2
	}
	target := fs.Arg(0)

	if *repoDir != "" && *githubRepo != "" {
		fmt.Fprintln(os.Stderr, "clone: --repo-dir and --github-repo are mutually exclusive")
		return 2
	}
	mode := clone.SourceModeRepoDir
	if *githubRepo != "" {
		mode = clone.SourceModeGithubRepo
	} else if *repoDir == "" {
		fmt.Fprintln(os.Stderr, "clone: --repo-dir=<path> is required (or use --github-repo=<url>)")
		return 2
	}

	ctx := context.Background()
	deps := clone.Deps{}.WithDefaults()

	refResult, err := clone.ResolveRef(ctx, deps, clone.ResolveRefOptions{
		RefOverride: *ref,
		Source:      *source,
		Mode:        mode,
		RepoDir:     *repoDir,
		GithubURL:   *githubRepo,
		LocalBinary: clone.LocalTeamsterBinary(),
	})
	if refResult.FetchDir != "" {
		defer os.RemoveAll(refResult.FetchDir) //nolint:errcheck
	}
	if err != nil {
		if *dryRun {
			// --dry-run still prints through the Reachability: line on a §3
			// refusal, using whatever ResolveRef determined before failing,
			// then stops (WP1 §10) — it does not just discard partial state.
			out, _ := renderClonePlan(clonePlan{
				Source:        sourceLabelFor(*source),
				FullHash:      refResult.FullHash,
				ShortHash:     disclosureShort(refResult),
				Channel:       disclosureChannel(refResult, *ref),
				Mode:          mode,
				RepoDir:       *repoDir,
				GithubRepo:    githubRepoOrDefault(*githubRepo, mode),
				ReachableFail: err.Error(),
			})
			fmt.Print(out)
			return 1
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	shipRepoDir := *repoDir
	if mode == clone.SourceModeGithubRepo {
		shipRepoDir = refResult.FetchDir
	}

	dirty, dirtyErr := clone.CheckDirty(ctx, deps.Run, shipRepoDir)
	if dirtyErr != nil && mode == clone.SourceModeRepoDir {
		fmt.Fprintf(os.Stderr, "clone: %v\n", dirtyErr)
		return 1
	}

	// Cosmetic only: --dry-run never contacts the clone target, so this
	// stays a human-readable ~-prefixed placeholder. The real run below
	// probes the target for its absolute $HOME and overwrites targetDir
	// with a genuine absolute path before any remote command is built —
	// see ProbeTarget's doc comment for why a literal ~ is never safe to
	// pass into an actual remote command.
	targetDir := "~/clone-src/" + refResult.FullHash

	plan := clonePlan{
		Source:     sourceLabelFor(*source),
		FullHash:   refResult.FullHash,
		ShortHash:  disclosureShort(refResult),
		Channel:    disclosureChannel(refResult, *ref),
		Mode:       mode,
		RepoDir:    *repoDir,
		GithubRepo: githubRepoOrDefault(*githubRepo, mode),
		Dirty:      dirty.Dirty,
		DirtyN:     len(dirty.Files),
		AllowDirty: *allowDirty,
		TargetDir:  targetDir,
		Target:     target,
	}

	// WP2's dry-run section (§5c) is a pure function over the LOCAL source
	// teamster.yaml — needs zero network/target access, same as WP1's own
	// section, so it can run and be asserted against before the target is
	// even reachable.
	srcConfig, srcConfigPath, srcConfigErr := readSourceConfig()
	var topologyFlags []string
	var topologyErr error
	if srcConfigErr != nil {
		topologyErr = srcConfigErr
	} else {
		topologyFlags, topologyErr = clonetopology.Translate(srcConfig, clonetopology.TargetSpec{})
	}

	if *dryRun {
		out, stoppedEarly := renderClonePlan(plan)
		fmt.Print(out)
		if stoppedEarly {
			return 1
		}
		fmt.Print(renderTopologyPlan(topologyFlags, topologyErr))
		fmt.Print(dryRunTrailer)
		if (dirty.Dirty && !*allowDirty) || topologyErr != nil {
			return 1
		}
		return 0
	}

	if topologyErr != nil {
		fmt.Fprintf(os.Stderr, "clone: %v\n", topologyErr)
		return 1
	}

	// Interrogate the target once, up front, before any other remote
	// contact: every remote path this pipeline builds from here on is
	// absolute (rooted at the target's own $HOME), never the literal ~
	// used above for the --dry-run display.
	targetInfo, err := clone.ProbeTarget(ctx, deps.SSHRun, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clone: probing target %s: %v\n", target, err)
		return 1
	}
	targetDir = path.Join(targetInfo.Home, "clone-src", refResult.FullHash)

	result, err := clone.Ship(ctx, deps, clone.ShipOptions{
		RepoDir:    shipRepoDir,
		FullHash:   refResult.FullHash,
		AllowDirty: *allowDirty,
		Target:     target,
		TargetDir:  targetDir,
	})
	if err != nil {
		switch {
		case errors.Is(err, clone.ErrDirtyRefused):
			// err.Error() is the exact refusal text (WP1 §4) — no extra wrapping.
			fmt.Fprintln(os.Stderr, err)
		case errors.Is(err, clone.ErrFingerprintMismatch):
			fmt.Fprintf(os.Stderr, "clone: %v\n\nThe target directory's content does not match what was shipped. This is\nthe provenance gate (WP1 §2 step 6) — nothing has been installed.\n", err)
		default:
			fmt.Fprintf(os.Stderr, "clone: %v\n", err)
		}
		return 1
	}

	fmt.Printf("teamster clone: shipped and verified\n\n")
	fmt.Printf("Resolved commit:   %s\n", result.FullHash)
	fmt.Printf("Version string:    %s\n", result.VersionString)
	fmt.Printf("Target directory:  %s:%s\n", target, result.TargetDir)
	fmt.Printf("Fingerprint:       %s (matched)\n", result.ActualFingerprint)

	// WP2: preflight (refuse to silently repoint an existing remote install),
	// topology translation was already computed above, then invoke the
	// installer, verify, mask, start. installrunner.sh's own exit code is not
	// a reliable success signal (it warns-and-continues on migration
	// failure, DESIGN.md gap #10) — Stage A and the schema check below are
	// the real gates, not "the SSH command returned 0."
	if err := preflightExistingRemote(ctx, deps.SSHRun, target, path.Join(targetInfo.Home, ".claude", "settings.json")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	targetBasedir := path.Join(targetInfo.Home, "teamster") // TargetSpec{} above — no --basedir flag in v1's CLI surface
	targetBinary := path.Join(targetBasedir, "bin", "teamster")

	fmt.Printf("\nInstalling on %s (this can take several minutes — compiling ~17 Go binaries, downloading prometheus/grafana/otelcol)...\n", target)
	if _, err := invokeInstaller(ctx, deps.SSHScript, target, result.TargetDir, result.ShortHash, result.VersionString, topologyFlags); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	stageA, err := clone.VerifyStageA(ctx, deps.SSHRun, target, targetBinary, result.FullHash)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// WP3 step 1.5: assert the target's schema is genuinely at this binary's
	// max known version before Leg 3 starts. installrunner.sh's own migration
	// step is non-fatal under set -euo pipefail (DESIGN.md gap #10), so the
	// installer's clean exit code alone does not guarantee this.
	if err := assertTargetSchemaCurrent(ctx, deps.SSHRun, target, targetBinary, mysql.MaxSchemaVersion()); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	if err := maskDisposableTimers(ctx, deps.SSHRun, target); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// WP3 Leg 3: move the data. I-B: pin the existing `latest` snapshot by
	// default — a fresh trigger is an explicit opt-in (--fresh-backup),
	// since the default must not write against the source.
	backupCfg, err := backup.LoadConfig(srcConfigPath, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clone: loading source backup config: %v\n", err)
		return 1
	}
	var snapshotDir string
	if *freshBackup {
		fmt.Printf("\nTriggering a fresh backup on the source...\n")
		snapshotDir, err = triggerFreshSourceBackup(ctx, backupCfg, srcConfigPath)
	} else {
		snapshotDir, err = resolveSourceSnapshot(backupCfg)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	remoteDataDir := path.Join(targetInfo.Home, "clone-data", result.FullHash)
	fmt.Printf("Transferring data from %s to %s:%s...\n", snapshotDir, target, remoteDataDir)
	if err := transferSnapshot(ctx, deps, snapshotDir, target, remoteDataDir); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	fmt.Printf("Stopping %s for the restore window...\n", strings.Join(temporaryQuiesceUnits, ", "))
	if err := stopTemporaryDaemons(ctx, deps.SSHRun, target); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	fmt.Printf("Restoring on %s...\n", target)
	if err := invokeRestore(ctx, deps.SSHRun, target, targetBinary, remoteDataDir); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	if err := restartTemporaryDaemons(ctx, deps.SSHRun, target); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// I7: source-side verification queries run locally (R9) through the
	// dedicated clone_verify_ro read-only credential, never the source's
	// full-privilege app DSN.
	sourceBasedir := filepath.Dir(filepath.Dir(srcConfigPath))
	cvroPassword, err := readCloneVerifyROPassword(sourceBasedir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	fmt.Printf("Verifying row counts (source vs target)...\n")
	mismatches, err := verifyRowCounts(ctx, deps.SSHRun, clone.LocalTeamsterBinary(), targetBinary, target, srcConfig.Store.DSN, cvroPassword)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clone: row-count verification: %v\n", err)
		return 1
	}
	if len(mismatches) > 0 {
		// Warn, don't fail — the source is live production, so append-only
		// tables (cost_facts, token_ledger) and gauge tables
		// (agent_health_gauge) will always show drift between the backup
		// snapshot and a live query. The acceptance harness (WP4) is the
		// real correctness gate.
		fmt.Fprintf(os.Stderr, "clone: row-count verification found %d mismatch(es) (expected on a live source — backup-vs-now drift):\n", len(mismatches))
		for _, m := range mismatches {
			fmt.Fprintf(os.Stderr, "  %s\n", m)
		}
	}

	if err := startTarget(ctx, deps.SSHRun, target, targetBinary); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	fmt.Printf(`
teamster clone: install complete

Target binary reports: %s
Schema:                 verified (SELECT MAX(version), not the installer's exit code)
Data:                   restored (row-count check advisory — live source drift expected)
sweep/backup timers:    masked permanently (R11)
teamster start:         ok

TEAMSTER_HOOK_SERVER_URL and MCP endpoints on this host still point at the
original source, not this clone; point tooling at it deliberately.
`, stageA.TargetOutput)
	return 0
}

func sourceLabelFor(source string) string {
	if source == "" {
		return "local (this host)"
	}
	return source
}

func disclosureShort(r clone.ResolveRefResult) string {
	if r.Disclosure.Commit == "" {
		return r.FullHash[:min(7, len(r.FullHash))]
	}
	return r.Disclosure.Commit
}

func disclosureChannel(r clone.ResolveRefResult, refOverride string) string {
	if refOverride != "" {
		return "--ref (explicit override)"
	}
	return r.Disclosure.Channel
}

func githubRepoOrDefault(v string, mode clone.SourceMode) string {
	if mode != clone.SourceModeGithubRepo {
		return ""
	}
	if v == "" {
		return clone.DefaultGithubRepoURL
	}
	return v
}

// clonePlan is WP1's --dry-run section (WP1 §10) — fixed, greppable
// left-hand labels; the right-hand values are the only variable part.
type clonePlan struct {
	Source        string
	FullHash      string
	ShortHash     string
	Channel       string
	Mode          clone.SourceMode
	RepoDir       string
	GithubRepo    string
	ReachableFail string // non-empty = §3/short-hash refusal text; plan stops after this line
	Dirty         bool
	DirtyN        int
	AllowDirty    bool
	TargetDir     string
	Target        string
}

const dryRunTrailer = "\nNo changes made. Re-run without --dry-run to execute.\n"

// renderClonePlan renders WP1's --dry-run section (WP1 §10). stoppedEarly
// reports whether the plan hit a refusal (unreachable/dirty) and the
// returned string already carries the trailer — a caller with more
// sections to compose (WP2's "Topology plan:") should only append its own
// section and print the shared trailer when stoppedEarly is false.
func renderClonePlan(p clonePlan) (out string, stoppedEarly bool) {
	var b strings.Builder
	fmt.Fprintln(&b, "teamster clone: plan (--dry-run, no changes made)")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Source instance:   %s\n", p.Source)
	fmt.Fprintf(&b, "Resolved commit:   %s\n", p.FullHash)
	fmt.Fprintf(&b, "                    (short %s, reported by %s via %s)\n", p.ShortHash, p.Source, p.Channel)
	if p.Mode == clone.SourceModeRepoDir {
		fmt.Fprintf(&b, "Source mode:       --repo-dir=%s\n", p.RepoDir)
	} else {
		fmt.Fprintf(&b, "Source mode:       --github-repo=%s\n", p.GithubRepo)
	}
	if p.ReachableFail != "" {
		fmt.Fprintf(&b, "Reachability:      FAILED: %s\n", p.ReachableFail)
		b.WriteString(dryRunTrailer)
		return b.String(), true
	}
	if p.Mode == clone.SourceModeRepoDir {
		fmt.Fprintln(&b, "Reachability:      OK — resolved locally")
	} else {
		fmt.Fprintln(&b, "Reachability:      OK — fetched from GitHub")
	}
	switch {
	case !p.Dirty:
		fmt.Fprintln(&b, "Working tree:      clean")
	case p.AllowDirty:
		fmt.Fprintf(&b, "Working tree:      DIRTY (%d files changed) — proceeding, --allow-dirty set\n", p.DirtyN)
	default:
		fmt.Fprintf(&b, "Working tree:      DIRTY (%d files changed) — would refuse without --allow-dirty\n", p.DirtyN)
		b.WriteString(dryRunTrailer)
		return b.String(), true
	}
	fmt.Fprintf(&b, "Ship plan:         git archive %s from %s\n", p.FullHash, shipSourceLabel(p))
	fmt.Fprintf(&b, "                    → tarball → scp to %s:%s\n", p.Target, p.TargetDir)
	fmt.Fprintf(&b, "Target directory:  %s  (fresh — will not overwrite if present)\n", p.TargetDir)
	return b.String(), false
}

// renderTopologyPlan renders WP2's --dry-run section (WP2 §5c) — the exact
// lib/installrunner.sh flag vector clonetopology.Translate produces, one
// flag per line. Pure function's output, so this needs zero network and
// zero target access, same as WP1's own dry-run section.
func renderTopologyPlan(flags []string, err error) string {
	var b strings.Builder
	fmt.Fprintln(&b, "Topology plan:")
	if err != nil {
		fmt.Fprintf(&b, "  FAILED: %v\n", err)
		return b.String()
	}
	for _, f := range flags {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	return b.String()
}

func shipSourceLabel(p clonePlan) string {
	if p.Mode == clone.SourceModeRepoDir {
		return p.RepoDir
	}
	return p.GithubRepo
}

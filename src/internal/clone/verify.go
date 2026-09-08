package clone

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrStageAMismatch means the target's installed binary does not report the
// commit clone shipped — a build-pipeline problem (WP2's TEAMSTER_COMMIT
// export didn't reach the compiled binary), not a tree-provenance failure.
// That failure class belongs to the content-manifest gate (§2 step 6),
// which runs earlier, before the installer is invoked at all. See
// HANDOFF-delivery.md §4 for why these are independent checks, neither
// subsuming the other.
var ErrStageAMismatch = errors.New("clone: Stage A verification failed — target binary does not report the shipped commit")

// PrefixMatch reports whether fullHash starts with shortHash. This is the
// one shared comparison used both to sanity-check that §1.2's local
// resolution landed on the same commit §1.1's disclosure named, and, later,
// as Stage A's post-install check against the target's self-reported
// commit (WP1 §5) — a short hash is never compared for full-string
// equality against a full one.
func PrefixMatch(fullHash, shortHash string) bool {
	return len(shortHash) > 0 && strings.HasPrefix(fullHash, shortHash)
}

// StageAResult is the outcome of the post-install, pre-restore blocking
// gate (WP1 §5).
type StageAResult struct {
	TargetOutput string // raw `<binary> --version` output from the target
	TargetCommit string // parsed short commit as reported by the target
	FullHash     string // what was shipped
	Matched      bool
}

// VerifyStageA runs `<binaryPath> --version` over SSH against the target
// and prefix-matches the reported commit against fullHash. Needs no daemon
// running at all — it reads the ldflags stamp straight out of the binary —
// so it has no interaction with the restore-window quiesce, and works
// whether or not hookd has ever started (WP1 §5). Callers must not proceed
// to Leg 3 (the data restore) on a mismatch here.
func VerifyStageA(ctx context.Context, sshRun SSHRunner, target, binaryPath, fullHash string) (StageAResult, error) {
	out, err := sshRun(ctx, target, binaryPath, "--version")
	if err != nil {
		return StageAResult{}, fmt.Errorf("clone: Stage A: querying %s on %s: %w", binaryPath, target, err)
	}
	commit, _, _, err := ParseVersionOutput(out)
	if err != nil {
		return StageAResult{}, fmt.Errorf("clone: Stage A: %w", err)
	}
	res := StageAResult{
		TargetOutput: strings.TrimSpace(out),
		TargetCommit: commit,
		FullHash:     fullHash,
		Matched:      PrefixMatch(fullHash, commit),
	}
	if !res.Matched {
		return res, fmt.Errorf("%w: shipped %s, target %s reports %q (full output: %q)", ErrStageAMismatch, fullHash, target, commit, res.TargetOutput)
	}
	return res, nil
}

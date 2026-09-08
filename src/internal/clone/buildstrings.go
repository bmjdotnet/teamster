package clone

import (
	"context"
	"fmt"
	"strings"
)

// BuildStrings is what WP1 hands WP2 alongside the shipped directory: the
// values WP2 must export as TEAMSTER_COMMIT/TEAMSTER_VERSION immediately
// before invoking lib/installrunner.sh (WP1 §1.3, §8). Without this,
// installrunner.sh's own `git rev-parse --short HEAD` fails against the
// .git-less extracted tree and silently stamps commit=none into every
// binary.
type BuildStrings struct {
	FullHash      string // for §5's Stage A verification
	ShortHash     string // → TEAMSTER_COMMIT
	VersionString string // → TEAMSTER_VERSION, -dirty-suffixed when allowDirty
}

// DeriveBuildStrings computes the three values WP1 hands to WP2, anchored
// explicitly at fullHash — never at the source repo's current on-disk
// checkout, since the resolved commit may be a historical commit or a
// different worktree branch than whatever happens to be checked out right
// now (WP1 §1.3).
func DeriveBuildStrings(ctx context.Context, run CommandRunner, source, fullHash string, allowDirty bool) (BuildStrings, error) {
	shortOut, err := run(ctx, "", "git", "-C", source, "rev-parse", "--short", fullHash)
	if err != nil {
		return BuildStrings{}, fmt.Errorf("clone: deriving short hash for %s: %w", fullHash, err)
	}
	shortHash := strings.TrimSpace(shortOut)

	version, err := deriveVersionString(ctx, run, source, fullHash)
	if err != nil {
		return BuildStrings{}, err
	}
	if allowDirty {
		// git describe --tags <full_hash> never combines --dirty with a
		// commit-ish (that's a hard error, not a no-op — verified live,
		// see HANDOFF-delivery.md §1), so the fallback chain below never
		// produces its own -dirty suffix. Append it manually, once, here —
		// safe and unambiguous specifically because of that guarantee: no
		// double-append risk, no need to detect an existing suffix.
		version += "-dirty"
	}

	return BuildStrings{FullHash: fullHash, ShortHash: shortHash, VersionString: version}, nil
}

// deriveVersionString replicates installrunner.sh:1298-1302's own fallback
// chain, anchored at fullHash instead of the working tree's current
// HEAD/on-disk VERSION file:
//
//	git describe --tags <full_hash> || git show <full_hash>:VERSION || echo dev
//
// Never combines --dirty with a commit-ish (WP1 §1.3's corrected note —
// that combination is a hard error, exit 128, not a silent no-op).
func deriveVersionString(ctx context.Context, run CommandRunner, source, fullHash string) (string, error) {
	if out, err := run(ctx, "", "git", "-C", source, "describe", "--tags", fullHash); err == nil {
		return strings.TrimSpace(out), nil
	}
	if out, err := run(ctx, "", "git", "-C", source, "show", fullHash+":VERSION"); err == nil {
		return strings.TrimSpace(out), nil
	}
	return "dev", nil
}

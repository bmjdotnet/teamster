package clone

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrDirtyRefused means CheckDirty found uncommitted changes and
// --allow-dirty was not set — the default-refuse case (WP1 §4).
var ErrDirtyRefused = errors.New("clone: refusing to ship a dirty working tree without --allow-dirty")

// DirtyStatus reports whether repoDir has uncommitted changes relative to
// its HEAD (WP1 §4).
type DirtyStatus struct {
	Dirty bool
	Files []string // paths from `git status --short`, one per changed/untracked file
}

// CheckDirty runs `git status --short` against repoDir. A non-empty result
// means uncommitted changes exist — the default policy is to refuse
// shipping them (WP1 §4): a tool advertised as reproducing "the exact
// build" that silently ships uncommitted deltas defeats its own purpose.
func CheckDirty(ctx context.Context, run CommandRunner, repoDir string) (DirtyStatus, error) {
	out, err := run(ctx, "", "git", "-C", repoDir, "status", "--short")
	if err != nil {
		return DirtyStatus{}, fmt.Errorf("clone: checking working-tree status in %s: %w", repoDir, err)
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return DirtyStatus{Dirty: false}, nil
	}
	lines := strings.Split(out, "\n")
	return DirtyStatus{Dirty: true, Files: lines}, nil
}

// DirtyRefusalMessage builds the exact dirty-tree refusal text (WP1 §4,
// default path with no --allow-dirty).
func DirtyRefusalMessage(repoDir, fullHash string, n int) string {
	return fmt.Sprintf(`teamster clone: --repo-dir=%s has uncommitted changes and will not be
shipped as-is.

%s has %d modified/untracked file(s) relative to the resolved commit
%s. Shipping them would silently break the guarantee this tool
exists to provide: that the clone runs the exact code the source instance
is running, not "the exact code plus whatever happens to be sitting in the
working tree right now."

Commit or stash your changes, or re-run with:
  --allow-dirty

to ship the working tree as-is. Note: --allow-dirty ships content that
`+"`teamster clone`"+`'s own summary output will flag, and the clone's own
version string (both `+"`teamster --version`"+` on the target and its later
`+"`/health`"+`) will carry a -dirty marker — but the commit it reports will be
identical either way, since a commit hash cannot itself encode
uncommitted content. See the design notes on --allow-dirty below.
`, repoDir, repoDir, n, fullHash)
}

// EnumerateDirtyFiles lists exactly the files a dirty ship must archive:
// tracked files as they currently sit on disk (including local
// modifications) plus untracked-but-not-ignored files. This is WP1 §4's
// recommended Option 2 — it respects .gitignore for free, unlike a plain
// tar of the whole working tree, so a dirty ship's payload stays as
// reproducible as a clean one modulo the operator's actual pending changes.
//
// A file `git status` shows as deleted-but-unstaged is still listed here
// (git ls-files --cached doesn't know it's gone from disk) but no longer
// exists to read; callers building the archive must skip missing paths
// rather than treat a stat failure as fatal (WP1 §4's noted edge case).
func EnumerateDirtyFiles(ctx context.Context, run CommandRunner, repoDir string) ([]string, error) {
	out, err := run(ctx, "", "git", "-C", repoDir, "ls-files", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("clone: enumerating dirty fileset in %s: %w", repoDir, err)
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

package clone

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrShortHashUnexpandable means --github-repo mode was given a short
// (<40-char) hash with no --repo-dir available to expand it. GitHub's
// direct-SHA fetch requires the full 40-char hash and there is no local
// GitHub-side primitive to expand one (WP1 §1.2).
var ErrShortHashUnexpandable = errors.New("clone: short commit hash cannot be expanded to full length without a local repository")

// ErrGithubUnreachable means the commit could not be fetched from the
// configured GitHub remote — the R3 refusal case, not a bug.
var ErrGithubUnreachable = errors.New("clone: commit not reachable on GitHub remote")

var fullHashRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// isFullHash reports whether s is a full-length (40 hex char) commit hash.
func isFullHash(s string) bool {
	return fullHashRe.MatchString(s)
}

// ResolveFullHashRepoDir resolves shortOrFull to a full commit hash inside
// repoDir (WP1 §1.2). `^{commit}` rejects anything that isn't a real commit
// object. Local, instant, no network — works for worktrees too, since a
// worktree shares its parent's .git/objects.
func ResolveFullHashRepoDir(ctx context.Context, run CommandRunner, repoDir, shortOrFull string) (string, error) {
	out, err := run(ctx, "", "git", "-C", repoDir, "rev-parse", "--verify", shortOrFull+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("clone: commit %s not found in %s: %w", shortOrFull, repoDir, err)
	}
	return strings.TrimSpace(out), nil
}

// ResolveFullHashGithubRepo resolves shortOrFull to a full commit hash by
// fetching it directly from url into fetchDir (a fresh, empty directory the
// caller owns). The fetch attempt IS the reachability check (WP1 §1.2, §3)
// — there is no cheaper or more accurate separate probe.
//
// shortOrFull must already be a full 40-char hash: ErrShortHashUnexpandable
// otherwise. On fetch failure, returns ErrGithubUnreachable wrapping the git
// error text — that failure is the R3 refusal case, not a bug, so callers
// should build the refusal message from it rather than treat it as
// unexpected.
func ResolveFullHashGithubRepo(ctx context.Context, run CommandRunner, fetchDir, url, shortOrFull string) (fullHash string, err error) {
	if !isFullHash(shortOrFull) {
		return "", fmt.Errorf("%w: %q", ErrShortHashUnexpandable, shortOrFull)
	}
	if _, err := run(ctx, "", "git", "init", "-q", fetchDir); err != nil {
		return "", fmt.Errorf("clone: initializing fetch dir %s: %w", fetchDir, err)
	}
	if _, err := run(ctx, "", "git", "-C", fetchDir, "fetch", "--depth=1", url, shortOrFull); err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrGithubUnreachable, url, err)
	}
	out, err := run(ctx, "", "git", "-C", fetchDir, "rev-parse", "--verify", shortOrFull+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("clone: fetched %s from %s but could not verify it as a commit: %w", shortOrFull, url, err)
	}
	return strings.TrimSpace(out), nil
}

// GithubRefusalMessage builds the exact R3 refusal text (WP1 §3) for a
// commit unreachable on the configured GitHub remote. shortHash/source
// describe where fullHash came from: when they equal fullHash/"" (the
// --ref=<full-hash> path, which bypasses disclosure — see ResolveRef), the
// "(from ..., reported by ...)" parenthetical is omitted rather than
// showing a redundant or empty provenance note.
func GithubRefusalMessage(fullHash, shortHash, source, githubRepoURL string) string {
	provenance := ""
	if shortHash != "" && shortHash != fullHash && source != "" {
		provenance = fmt.Sprintf(" (from %s, reported by %s)", shortHash, source)
	}
	return fmt.Sprintf(`teamster clone: commit %s%s is not reachable on %s.

The public GitHub mirror only ever receives squashed release commits —
day-to-day development happens on a private history that never reaches
GitHub. This is the normal case for an actively-developed source
instance, not a sign anything is wrong with it.

Retry with:
  --repo-dir=<path>

pointing at a local clone or worktree that has this commit in its
history (the private hub, or a checkout of it).
`, fullHash, provenance, githubRepoURL)
}

// ShortHashRefusalMessage builds the refusal text for --github-repo mode
// given a short hash it cannot expand (WP1 §1.2). Empirically confirmed
// live against github.com/bmjdotnet/teamster: a short hash always fails
// GitHub's direct-SHA fetch ("couldn't find remote ref"), even for a commit
// that fetches cleanly in full form — so this is the common case for
// --github-repo without an explicit full --ref, not a corner case.
func ShortHashRefusalMessage(shortHash, source string) string {
	return fmt.Sprintf(`teamster clone: commit %s (reported by %s) cannot be
resolved to a full commit hash without a local repository.

--github-repo mode fetches an exact commit SHA directly from GitHub, which
requires the full 40-character hash — there is no local primitive to
expand a short one without a repo that already has the object.

Retry with:
  --repo-dir=<path>

pointing at a local clone or worktree that has this commit in its
history, or supply the full hash explicitly:
  --ref=<full-40-character-hash>
`, shortHash, source)
}

// DefaultGithubRepoURL is the canonical public mirror used when
// --github-repo is passed with no explicit URL.
const DefaultGithubRepoURL = "https://github.com/bmjdotnet/teamster"

// sourceLabel renders a --source value for display/refusal text: "local
// (this host)" when unset, the value itself otherwise.
func sourceLabel(source string) string {
	if source == "" {
		return "local (this host)"
	}
	return source
}

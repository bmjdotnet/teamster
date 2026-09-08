package clone

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// SourceMode selects where clone ships code from — the CLI's mutually
// exclusive --repo-dir/--github-repo flags (DESIGN.md §2).
type SourceMode int

const (
	SourceModeRepoDir SourceMode = iota
	SourceModeGithubRepo
)

// ResolveRefOptions is the full set of inputs to Leg 1 (WP1 §1).
type ResolveRefOptions struct {
	RefOverride string // --ref; when set, skips the disclosure query entirely
	Source      string // --source; "" = local instance (the common path per R9)
	Mode        SourceMode
	RepoDir     string // required for SourceModeRepoDir
	GithubURL   string // for SourceModeGithubRepo; DefaultGithubRepoURL when empty
	Hostname    string // for isLocalSource's "am I the source host" check; "" = os.Hostname()
	LocalBinary string // passed through to ResolveBuild
}

// ResolveRefResult is Leg 1's output: the resolved full hash plus the
// disclosure that produced it (Disclosure is the zero value when --ref
// bypassed the query). Populated as far as resolution got even when
// ResolveRef returns an error — e.g. on a §3 GitHub-unreachable refusal,
// Disclosure and the attempted hash are still set, so a --dry-run caller
// can render the plan up through the Reachability: FAILED line (WP1 §10)
// instead of having nothing to show.
type ResolveRefResult struct {
	FullHash   string // the resolved (or, on a reachability failure, attempted) full hash
	Disclosure BuildDisclosure
	FetchDir   string // set only for SourceModeGithubRepo: the throwaway dir holding the fetched object, to be reused as the ship's RepoDir (WP1 §3 step 2). Caller must remove it once shipping is done.
}

// ResolveRef runs WP1 §1 in full: §1.1's disclosure query (unless --ref
// overrides it), then §1.2's re-resolution to a full hash inside whichever
// source the operator selected. Returns the exact §3 or "unexpandable
// short hash" refusal as the error's message when --github-repo mode can't
// proceed — callers should print err.Error() directly rather than wrap it
// further, since GithubRefusalMessage/ShortHashRefusalMessage already are
// the full user-facing text. The returned ResolveRefResult is populated as
// far as resolution reached even on error (see ResolveRefResult's doc).
func ResolveRef(ctx context.Context, deps Deps, opts ResolveRefOptions) (ResolveRefResult, error) {
	deps = deps.WithDefaults()

	shortOrFull := opts.RefOverride
	var disclosure BuildDisclosure
	if shortOrFull == "" {
		var err error
		disclosure, err = ResolveBuild(ctx, SourceQueryOptions{
			Source:      opts.Source,
			LocalBinary: opts.LocalBinary,
			Hostname:    opts.Hostname,
			Run:         deps.Run,
		})
		if err != nil {
			return ResolveRefResult{}, err
		}
		shortOrFull = disclosure.Commit
	}

	switch opts.Mode {
	case SourceModeRepoDir:
		fullHash, err := ResolveFullHashRepoDir(ctx, deps.Run, opts.RepoDir, shortOrFull)
		if err != nil {
			return ResolveRefResult{Disclosure: disclosure}, err
		}
		return ResolveRefResult{FullHash: fullHash, Disclosure: disclosure}, nil

	case SourceModeGithubRepo:
		githubURL := opts.GithubURL
		if githubURL == "" {
			githubURL = DefaultGithubRepoURL
		}
		if !isFullHash(shortOrFull) {
			return ResolveRefResult{Disclosure: disclosure, FullHash: shortOrFull}, &RefusalError{Err: ErrShortHashUnexpandable, Message: ShortHashRefusalMessage(shortOrFull, sourceLabel(opts.Source))}
		}
		fetchDir, err := os.MkdirTemp("", "teamster-clone-fetch-*")
		if err != nil {
			return ResolveRefResult{Disclosure: disclosure, FullHash: shortOrFull}, fmt.Errorf("clone: creating fetch dir: %w", err)
		}
		fullHash, err := ResolveFullHashGithubRepo(ctx, deps.Run, fetchDir, githubURL, shortOrFull)
		if err != nil {
			os.RemoveAll(fetchDir) //nolint:errcheck
			partial := ResolveRefResult{Disclosure: disclosure, FullHash: shortOrFull}
			if errors.Is(err, ErrGithubUnreachable) {
				// This path is only reachable via an explicit full --ref (a
				// disclosure-derived short hash always fails isFullHash above and
				// refuses earlier), so there is no separate short-hash provenance
				// to show — GithubRefusalMessage omits the parenthetical when
				// shortHash == fullHash.
				return partial, &RefusalError{Err: err, Message: GithubRefusalMessage(shortOrFull, shortOrFull, sourceLabel(opts.Source), githubURL)}
			}
			return partial, err
		}
		return ResolveRefResult{FullHash: fullHash, Disclosure: disclosure, FetchDir: fetchDir}, nil

	default:
		return ResolveRefResult{}, fmt.Errorf("clone: unknown source mode %v", opts.Mode)
	}
}

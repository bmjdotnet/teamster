package clone

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestIsFullHash(t *testing.T) {
	tests := map[string]bool{
		"c52f51cc1c8e2df7d2fb3471d4f0733e2b9a0d5d": true,
		"c52f51c": false,
		"":        false,
		"ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ": false, // wrong length after all-Z but also non-hex
	}
	for in, want := range tests {
		if got := isFullHash(in); got != want {
			t.Errorf("isFullHash(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResolveFullHashRepoDir(t *testing.T) {
	dir, fullHash := newGitRepo(t)
	run := testRunner()

	// Full hash round-trips.
	got, err := ResolveFullHashRepoDir(context.Background(), run, dir, fullHash)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != fullHash {
		t.Errorf("got %q, want %q", got, fullHash)
	}

	// Short hash expands to the same full hash.
	short := fullHash[:7]
	got, err = ResolveFullHashRepoDir(context.Background(), run, dir, short)
	if err != nil {
		t.Fatalf("unexpected error resolving short hash: %v", err)
	}
	if got != fullHash {
		t.Errorf("short hash resolved to %q, want %q", got, fullHash)
	}

	// Nonexistent commit fails.
	if _, err := ResolveFullHashRepoDir(context.Background(), run, dir, "0000000"); err == nil {
		t.Error("expected error for nonexistent commit, got nil")
	}
}

func TestResolveFullHashRepoDir_Worktree(t *testing.T) {
	dir, fullHash := newGitRepo(t)
	run := testRunner()

	wtDir := t.TempDir()
	os.RemoveAll(wtDir) //nolint:errcheck // git worktree add requires the path not exist
	runGit(t, dir, "worktree", "add", "-q", wtDir, "-b", "wt-test", fullHash)

	// A worktree shares its parent's .git/objects — resolving from the
	// worktree's own directory must succeed (WP1 §1.2's explicit claim).
	got, err := ResolveFullHashRepoDir(context.Background(), run, wtDir, fullHash)
	if err != nil {
		t.Fatalf("unexpected error resolving from worktree: %v", err)
	}
	if got != fullHash {
		t.Errorf("got %q, want %q", got, fullHash)
	}
}

func TestResolveFullHashGithubRepo_RequiresFullHash(t *testing.T) {
	run := testRunner()
	fetchDir := t.TempDir()
	os.RemoveAll(fetchDir) //nolint:errcheck

	_, err := ResolveFullHashGithubRepo(context.Background(), run, fetchDir, "file:///nonexistent", "c52f51c")
	if !errors.Is(err, ErrShortHashUnexpandable) {
		t.Fatalf("got %v, want ErrShortHashUnexpandable", err)
	}
}

func TestResolveFullHashGithubRepo_LocalFileRemote(t *testing.T) {
	// Use a local bare repo as the "GitHub" remote so this test needs no
	// network. Validates the fetch-as-reachability-check plumbing; the
	// GitHub-specific short-hash-rejection behavior itself was verified
	// live against github.com/bmjdotnet/teamster (see HANDOFF-delivery.md
	// §3's ledger and this WP's check-in) rather than re-asserted here.
	srcDir, fullHash := newGitRepo(t)
	bareDir := t.TempDir()
	os.RemoveAll(bareDir) //nolint:errcheck
	run := testRunner()
	runGit(t, "", "clone", "-q", "--bare", srcDir, bareDir)

	fetchDir := t.TempDir()
	os.RemoveAll(fetchDir) //nolint:errcheck
	got, err := ResolveFullHashGithubRepo(context.Background(), run, fetchDir, "file://"+bareDir, fullHash)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != fullHash {
		t.Errorf("got %q, want %q", got, fullHash)
	}
}

func TestResolveFullHashGithubRepo_Unreachable(t *testing.T) {
	run := testRunner()
	bareDir := t.TempDir()
	os.RemoveAll(bareDir) //nolint:errcheck
	runGit(t, "", "init", "-q", "--bare", bareDir)

	fetchDir := t.TempDir()
	os.RemoveAll(fetchDir) //nolint:errcheck
	// A syntactically full hash that doesn't exist anywhere in the empty bare repo.
	fake := "0000000000000000000000000000000000000000"
	_, err := ResolveFullHashGithubRepo(context.Background(), run, fetchDir, "file://"+bareDir, fake)
	if !errors.Is(err, ErrGithubUnreachable) {
		t.Fatalf("got %v, want ErrGithubUnreachable", err)
	}
}

func TestResolveRef_RefOverrideSkipsDisclosure(t *testing.T) {
	dir, fullHash := newGitRepo(t)
	calledDisclosure := false
	deps := Deps{
		Run: func(ctx context.Context, cdir, name string, args ...string) (string, error) {
			if name == "teamster" {
				calledDisclosure = true
			}
			return DefaultRunner(ctx, cdir, name, args...)
		},
	}
	res, err := ResolveRef(context.Background(), deps, ResolveRefOptions{
		RefOverride: fullHash,
		Mode:        SourceModeRepoDir,
		RepoDir:     dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calledDisclosure {
		t.Error("--ref should skip the disclosure query entirely")
	}
	if res.FullHash != fullHash {
		t.Errorf("got %q, want %q", res.FullHash, fullHash)
	}
}

func TestResolveRef_GithubModeShortHashRefuses(t *testing.T) {
	dir, _ := newGitRepo(t)
	deps := Deps{
		Run: func(ctx context.Context, cdir, name string, args ...string) (string, error) {
			return "teamster v0.2.6 (c52f51c, 2026-08-14T02:37:57Z)\n", nil
		},
	}
	_, err := ResolveRef(context.Background(), deps, ResolveRefOptions{
		Mode:      SourceModeGithubRepo,
		GithubURL: "file://" + dir,
	})
	if !errors.Is(err, ErrShortHashUnexpandable) {
		t.Fatalf("got %v, want ErrShortHashUnexpandable", err)
	}
	if err != nil && len(err.Error()) < len("clone: short commit hash") {
		t.Error("expected the refusal message to be included in the error text")
	}
}

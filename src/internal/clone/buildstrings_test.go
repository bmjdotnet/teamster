package clone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveBuildStrings_NoTag(t *testing.T) {
	dir, fullHash := newGitRepo(t)
	run := testRunner()

	bs, err := DeriveBuildStrings(context.Background(), run, dir, fullHash, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bs.FullHash != fullHash {
		t.Errorf("FullHash = %q, want %q", bs.FullHash, fullHash)
	}
	if !strings.HasPrefix(fullHash, bs.ShortHash) {
		t.Errorf("ShortHash %q is not a prefix of %q", bs.ShortHash, fullHash)
	}
	// No tags exist, and there's no VERSION file — falls all the way to "dev".
	if bs.VersionString != "dev" {
		t.Errorf("VersionString = %q, want %q", bs.VersionString, "dev")
	}
}

func TestDeriveBuildStrings_WithTag(t *testing.T) {
	dir, fullHash := newGitRepo(t)
	runGit(t, dir, "tag", "v1.2.3", fullHash)
	run := testRunner()

	bs, err := DeriveBuildStrings(context.Background(), run, dir, fullHash, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bs.VersionString != "v1.2.3" {
		t.Errorf("VersionString = %q, want %q", bs.VersionString, "v1.2.3")
	}
}

func TestDeriveBuildStrings_VersionFileFallback(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "VERSION", "v9.9.9\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "with version file")
	fullHash := runGit(t, dir, "rev-parse", "HEAD")
	run := testRunner()

	bs, err := DeriveBuildStrings(context.Background(), run, dir, fullHash, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No tag reaches this commit, so it falls to `git show <hash>:VERSION`.
	if bs.VersionString != "v9.9.9" {
		t.Errorf("VersionString = %q, want %q", bs.VersionString, "v9.9.9")
	}
}

func TestDeriveBuildStrings_AllowDirtyAppendsSuffix(t *testing.T) {
	dir, fullHash := newGitRepo(t)
	runGit(t, dir, "tag", "v1.2.3", fullHash)
	run := testRunner()

	bs, err := DeriveBuildStrings(context.Background(), run, dir, fullHash, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "v1.2.3-dirty"
	if bs.VersionString != want {
		t.Errorf("VersionString = %q, want %q", bs.VersionString, want)
	}
}

func TestDeriveBuildStrings_AnchoredAtHistoricalCommit(t *testing.T) {
	// The resolved commit is not guaranteed to be the source repo's current
	// checkout — DeriveBuildStrings must anchor at fullHash, not at the
	// working tree's on-disk VERSION/HEAD (WP1 §1.3's explicit point).
	dir, firstHash := newGitRepo(t)
	writeFile(t, dir, "hello.txt", "changed\n")
	runGit(t, dir, "commit", "-aq", "-m", "second commit")
	run := testRunner()

	bs, err := DeriveBuildStrings(context.Background(), run, dir, firstHash, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bs.FullHash != firstHash {
		t.Errorf("FullHash = %q, want the historical commit %q, not current HEAD", bs.FullHash, firstHash)
	}
}

// writeFile is a small local helper (distinct from newGitRepo's inline
// setup) for tests that need to add a file after the repo already exists.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

package clone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDirty_Clean(t *testing.T) {
	dir, _ := newGitRepo(t)
	status, err := CheckDirty(context.Background(), testRunner(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Dirty {
		t.Errorf("expected clean, got dirty: %v", status.Files)
	}
}

func TestCheckDirty_ModifiedAndUntracked(t *testing.T) {
	dir, _ := newGitRepo(t)
	writeFile(t, dir, "hello.txt", "modified\n")
	writeFile(t, dir, "new.txt", "new\n")

	status, err := CheckDirty(context.Background(), testRunner(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.Dirty {
		t.Fatal("expected dirty")
	}
	if len(status.Files) != 2 {
		t.Errorf("got %d changed files, want 2: %v", len(status.Files), status.Files)
	}
}

func TestEnumerateDirtyFiles_RespectsGitignore(t *testing.T) {
	dir, _ := newGitRepo(t)
	writeFile(t, dir, ".gitignore", "ignored.txt\n")
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-q", "-m", "add gitignore")
	writeFile(t, dir, "ignored.txt", "should not appear\n")
	writeFile(t, dir, "tracked-new.txt", "should appear\n")
	writeFile(t, dir, "hello.txt", "modified tracked file\n")

	files, err := EnumerateDirtyFiles(context.Background(), testRunner(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	set := map[string]bool{}
	for _, f := range files {
		set[f] = true
	}
	if set["ignored.txt"] {
		t.Error("ignored.txt should be excluded (respects .gitignore)")
	}
	if !set["tracked-new.txt"] {
		t.Error("tracked-new.txt (untracked, not ignored) should be included")
	}
	if !set["hello.txt"] {
		t.Error("hello.txt (tracked, modified) should be included")
	}
	if !set[".gitignore"] {
		t.Error(".gitignore itself is tracked and should be included")
	}
}

func TestEnumerateDirtyFiles_DeletedButUnstagedIsSkippedGracefully(t *testing.T) {
	dir, _ := newGitRepo(t)
	if err := os.Remove(filepath.Join(dir, "hello.txt")); err != nil {
		t.Fatal(err)
	}

	// git ls-files --cached still lists hello.txt (deleted-but-unstaged) —
	// EnumerateDirtyFiles itself just enumerates; skipping the missing file
	// is SourceFingerprintDirty/archiveDirty's job (WP1 §4's noted edge
	// case), exercised in TestSourceFingerprintDirty_SkipsMissingFile below.
	files, err := EnumerateDirtyFiles(context.Background(), testRunner(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, f := range files {
		if f == "hello.txt" {
			found = true
		}
	}
	if !found {
		t.Error("expected hello.txt still listed by git ls-files --cached despite being deleted on disk")
	}
}

func TestDirtyRefusalMessage_ContainsKeyFacts(t *testing.T) {
	msg := DirtyRefusalMessage("/home/testuser/repos/teamster", "c52f51cc1c8e2df7d2fb3471d4f0733e2b9a0d5d", 3)
	for _, want := range []string{"/home/testuser/repos/teamster", "c52f51cc1c8e2df7d2fb3471d4f0733e2b9a0d5d", "3 modified", "--allow-dirty"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message missing %q:\n%s", want, msg)
		}
	}
}

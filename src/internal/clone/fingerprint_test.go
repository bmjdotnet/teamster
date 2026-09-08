package clone

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// localScriptRunner runs an SSHScriptRunner-shaped script via local bash —
// no real SSH target needed. It exercises the actual remoteFingerprintScript
// content (real bash, real git hash-object), just without the network hop,
// which is exactly the property HANDOFF-delivery.md §2 flags as the one
// piece most worth prototyping in isolation.
func localScriptRunner(ctx context.Context, target, script string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", append([]string{"-s", "--"}, args...)...)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("bash -s: %w: %s", err, out)
	}
	return string(out), nil
}

func extractArchive(t *testing.T, run CommandRunner, repoDir, fullHash, destDir string) {
	t.Helper()
	tarball := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := archiveClean(context.Background(), run, repoDir, fullHash, tarball); err != nil {
		t.Fatalf("archiveClean: %v", err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("tar", "xzf", tarball, "-C", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar extract: %v: %s", err, out)
	}
}

func TestFingerprint_CleanModeRoundTrip(t *testing.T) {
	repoDir, fullHash := newGitRepo(t)
	run := testRunner()

	expected, err := SourceFingerprintClean(context.Background(), run, repoDir, fullHash)
	if err != nil {
		t.Fatalf("SourceFingerprintClean: %v", err)
	}
	if len(expected) != 64 {
		t.Fatalf("expected fingerprint should be a 64-char hex sha256, got %q", expected)
	}

	targetDir := t.TempDir()
	os.RemoveAll(targetDir) //nolint:errcheck
	extractArchive(t, run, repoDir, fullHash, targetDir)

	// This target dir has no .git anywhere above it — verifies the
	// no-repository property HANDOFF-delivery.md §2 confirms live.
	if _, err := exec.Command("git", "-C", targetDir, "rev-parse", "--show-toplevel").CombinedOutput(); err == nil {
		t.Fatal("expected extracted dir to NOT be inside a git repository")
	}

	actual, err := TargetFingerprint(context.Background(), localScriptRunner, "unused-target", targetDir)
	if err != nil {
		t.Fatalf("TargetFingerprint: %v", err)
	}
	if err := CompareFingerprints(expected, actual); err != nil {
		t.Errorf("fingerprints should match after an honest extraction: %v", err)
	}
}

func TestFingerprint_CleanModeDetectsTampering(t *testing.T) {
	repoDir, fullHash := newGitRepo(t)
	run := testRunner()

	expected, err := SourceFingerprintClean(context.Background(), run, repoDir, fullHash)
	if err != nil {
		t.Fatalf("SourceFingerprintClean: %v", err)
	}

	targetDir := t.TempDir()
	os.RemoveAll(targetDir) //nolint:errcheck
	extractArchive(t, run, repoDir, fullHash, targetDir)

	// Simulate a wrong-directory install / tampering: mutate a file post-extraction.
	if err := os.WriteFile(filepath.Join(targetDir, "hello.txt"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	actual, err := TargetFingerprint(context.Background(), localScriptRunner, "unused-target", targetDir)
	if err != nil {
		t.Fatalf("TargetFingerprint: %v", err)
	}
	err = CompareFingerprints(expected, actual)
	if !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("got %v, want ErrFingerprintMismatch", err)
	}
}

func TestFingerprint_DirtyModeRoundTrip(t *testing.T) {
	repoDir, _ := newGitRepo(t)
	run := testRunner()

	// Dirty the tree: modify a tracked file, add an untracked one.
	writeFile(t, repoDir, "hello.txt", "dirty content\n")
	writeFile(t, repoDir, "extra.txt", "extra\n")

	files, err := EnumerateDirtyFiles(context.Background(), run, repoDir)
	if err != nil {
		t.Fatalf("EnumerateDirtyFiles: %v", err)
	}
	expected, err := SourceFingerprintDirty(context.Background(), run, repoDir, files)
	if err != nil {
		t.Fatalf("SourceFingerprintDirty: %v", err)
	}

	tarball := filepath.Join(t.TempDir(), "dirty.tar.gz")
	if err := archiveDirty(context.Background(), repoDir, files, tarball); err != nil {
		t.Fatalf("archiveDirty: %v", err)
	}
	targetDir := t.TempDir()
	os.RemoveAll(targetDir) //nolint:errcheck
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("tar", "xzf", tarball, "-C", targetDir).CombinedOutput(); err != nil {
		t.Fatalf("tar extract: %v: %s", err, out)
	}

	actual, err := TargetFingerprint(context.Background(), localScriptRunner, "unused-target", targetDir)
	if err != nil {
		t.Fatalf("TargetFingerprint: %v", err)
	}
	if err := CompareFingerprints(expected, actual); err != nil {
		t.Errorf("dirty-mode fingerprints should match after an honest extraction: %v", err)
	}
}

func TestSourceFingerprintDirty_SkipsMissingFile(t *testing.T) {
	repoDir, _ := newGitRepo(t)
	run := testRunner()
	if err := os.Remove(filepath.Join(repoDir, "hello.txt")); err != nil {
		t.Fatal(err)
	}
	files, err := EnumerateDirtyFiles(context.Background(), run, repoDir)
	if err != nil {
		t.Fatalf("EnumerateDirtyFiles: %v", err)
	}
	// Should not error even though hello.txt is listed but missing on disk.
	if _, err := SourceFingerprintDirty(context.Background(), run, repoDir, files); err != nil {
		t.Fatalf("SourceFingerprintDirty should skip missing files, got error: %v", err)
	}
}

func TestSha256Lines_EmptyInputIsStable(t *testing.T) {
	got := sha256Lines(nil)
	if len(got) != 64 {
		t.Fatalf("expected 64-char hex digest, got %q", got)
	}
	// Must equal the shell's `printf '' | sha256sum` value, since the
	// target-side script's empty branch is built to match it exactly.
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("sha256Lines(nil) = %q, want the well-known empty-string sha256 %q", got, want)
	}
}

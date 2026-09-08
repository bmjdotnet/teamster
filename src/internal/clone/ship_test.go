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

// fakeRemote simulates a push-only SSH target as a local directory tree, so
// Ship's full orchestration (archive → transport → enforced-fresh extract →
// content-manifest gate) can be exercised end to end without a real SSH
// target. Every remote command Ship actually issues (sha256sum, mkdir, tar,
// rm) is handled for real against files under root — this is not a mock of
// the logic, just of the network hop.
type fakeRemote struct {
	root string
}

func (f *fakeRemote) local(remotePath string) string {
	return filepath.Join(f.root, strings.TrimPrefix(remotePath, "/"))
}

func (f *fakeRemote) upload(ctx context.Context, localPath, target, remotePath string) error {
	dst := f.local(remotePath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func (f *fakeRemote) sshRun(ctx context.Context, target string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("fakeRemote: empty command")
	}
	switch args[0] {
	case "sudo":
		// apt-get install git (or similar) — no-op in tests
		return "", nil
	case "sha256sum":
		sum, err := sha256File(f.local(args[1]))
		if err != nil {
			return "", err
		}
		return sum + "  " + args[1] + "\n", nil
	case "mkdir":
		if len(args) > 1 && args[1] == "-p" {
			return "", os.MkdirAll(f.local(args[2]), 0o755)
		}
		if err := os.Mkdir(f.local(args[1]), 0o755); err != nil {
			return "", err
		}
		return "", nil
	case "tar":
		// Ship's real "tar" invocations are "xzf <tarball> -C <targetDir>":
		// both path args need translating into the fake remote root, but
		// unlike remoteTarball ("/tmp/...", absolute), TargetDir in these
		// tests is deliberately relative ("clone-src/<hash>") — so
		// translate everything except the known flag tokens, not just
		// slash-prefixed args.
		knownFlags := map[string]bool{"xzf": true, "czf": true, "-C": true, "-f": true}
		translated := make([]string, len(args)-1)
		for i, a := range args[1:] {
			if knownFlags[a] {
				translated[i] = a
			} else {
				translated[i] = f.local(a)
			}
		}
		out, err := exec.CommandContext(ctx, "tar", translated...).CombinedOutput()
		if err != nil {
			return string(out), err
		}
		return string(out), nil
	case "rm":
		return "", os.Remove(f.local(args[len(args)-1]))
	default:
		return "", fmt.Errorf("fakeRemote: unsupported command %v", args)
	}
}

func (f *fakeRemote) sshScript(ctx context.Context, target, script string, args ...string) (string, error) {
	translated := make([]string, len(args))
	for i, a := range args {
		translated[i] = f.local(a)
	}
	return localScriptRunner(ctx, target, script, translated...)
}

func newFakeRemote(t *testing.T) *fakeRemote {
	t.Helper()
	root := t.TempDir()
	return &fakeRemote{root: root}
}

func TestShip_CleanMode_EndToEnd(t *testing.T) {
	repoDir, fullHash := newGitRepo(t)
	remote := newFakeRemote(t)
	if err := os.MkdirAll(filepath.Join(remote.root, "clone-src"), 0o755); err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Run:       testRunner(),
		SSHRun:    remote.sshRun,
		SSHScript: remote.sshScript,
		Upload:    remote.upload,
	}
	targetDir := "clone-src/" + fullHash
	result, err := Ship(context.Background(), deps, ShipOptions{
		RepoDir:   repoDir,
		FullHash:  fullHash,
		Target:    "user@fake-target",
		TargetDir: targetDir,
	})
	if err != nil {
		t.Fatalf("Ship failed: %v", err)
	}
	if result.FullHash != fullHash {
		t.Errorf("FullHash = %q, want %q", result.FullHash, fullHash)
	}
	if result.ExpectedFingerprint != result.ActualFingerprint {
		t.Error("fingerprints should match on an honest ship")
	}

	// The extracted content actually landed and is readable.
	got, err := os.ReadFile(remote.local(targetDir + "/hello.txt"))
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(got) != "hello\n" {
		t.Errorf("extracted content = %q, want %q", got, "hello\n")
	}
}

func TestShip_RefusesDirtyByDefault(t *testing.T) {
	repoDir, fullHash := newGitRepo(t)
	writeFile(t, repoDir, "hello.txt", "modified\n")
	remote := newFakeRemote(t)
	if err := os.MkdirAll(filepath.Join(remote.root, "clone-src"), 0o755); err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Run:       testRunner(),
		SSHRun:    remote.sshRun,
		SSHScript: remote.sshScript,
		Upload:    remote.upload,
	}
	_, err := Ship(context.Background(), deps, ShipOptions{
		RepoDir:   repoDir,
		FullHash:  fullHash,
		Target:    "user@fake-target",
		TargetDir: "clone-src/" + fullHash,
	})
	if err == nil {
		t.Fatal("expected Ship to refuse a dirty tree without AllowDirty")
	}
	if !strings.Contains(err.Error(), "--allow-dirty") {
		t.Errorf("expected refusal message to mention --allow-dirty, got: %v", err)
	}

	// No SSH mkdir should have happened — refusal occurs before any target
	// contact (WP1 §4's exit-code/side-effect contract).
	if _, statErr := os.Stat(remote.local("clone-src/" + fullHash)); statErr == nil {
		t.Error("target directory should not have been created on a dirty-ship refusal")
	}
}

func TestShip_AllowDirty_EndToEnd(t *testing.T) {
	repoDir, fullHash := newGitRepo(t)
	writeFile(t, repoDir, "hello.txt", "modified\n")
	writeFile(t, repoDir, "extra.txt", "extra\n")
	remote := newFakeRemote(t)
	if err := os.MkdirAll(filepath.Join(remote.root, "clone-src"), 0o755); err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Run:       testRunner(),
		SSHRun:    remote.sshRun,
		SSHScript: remote.sshScript,
		Upload:    remote.upload,
	}
	targetDir := "clone-src/" + fullHash
	result, err := Ship(context.Background(), deps, ShipOptions{
		RepoDir:    repoDir,
		FullHash:   fullHash,
		AllowDirty: true,
		Target:     "user@fake-target",
		TargetDir:  targetDir,
	})
	if err != nil {
		t.Fatalf("Ship (dirty) failed: %v", err)
	}
	if !strings.HasSuffix(result.VersionString, "-dirty") {
		t.Errorf("VersionString = %q, want -dirty suffix", result.VersionString)
	}
	got, err := os.ReadFile(remote.local(targetDir + "/hello.txt"))
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(got) != "modified\n" {
		t.Errorf("extracted content = %q, want the dirty content %q", got, "modified\n")
	}
}

func TestShip_RefusesToReuseExistingTargetDir(t *testing.T) {
	repoDir, fullHash := newGitRepo(t)
	remote := newFakeRemote(t)
	targetDir := "clone-src/" + fullHash
	if err := os.MkdirAll(remote.local(targetDir), 0o755); err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Run:       testRunner(),
		SSHRun:    remote.sshRun,
		SSHScript: remote.sshScript,
		Upload:    remote.upload,
	}
	_, err := Ship(context.Background(), deps, ShipOptions{
		RepoDir:   repoDir,
		FullHash:  fullHash,
		Target:    "user@fake-target",
		TargetDir: targetDir,
	})
	if err == nil {
		t.Fatal("expected Ship to refuse an already-existing target directory (no -p, structural fresh-dir enforcement)")
	}
}

func TestShip_DetectsFingerprintMismatchFromTampering(t *testing.T) {
	// A "wrong directory install" scenario: something writes into the target
	// dir after extraction but before the fingerprint check would normally
	// run. Since Ship runs the check itself right after extraction, simulate
	// this by tampering inside the fake SSHScript wrapper before it computes
	// the target fingerprint.
	repoDir, fullHash := newGitRepo(t)
	remote := newFakeRemote(t)
	if err := os.MkdirAll(filepath.Join(remote.root, "clone-src"), 0o755); err != nil {
		t.Fatal(err)
	}
	targetDir := "clone-src/" + fullHash

	tamper := func(ctx context.Context, target, script string, args ...string) (string, error) {
		if err := os.WriteFile(remote.local(targetDir+"/hello.txt"), []byte("tampered\n"), 0o644); err != nil {
			return "", err
		}
		return remote.sshScript(ctx, target, script, args...)
	}

	deps := Deps{
		Run:       testRunner(),
		SSHRun:    remote.sshRun,
		SSHScript: tamper,
		Upload:    remote.upload,
	}
	_, err := Ship(context.Background(), deps, ShipOptions{
		RepoDir:   repoDir,
		FullHash:  fullHash,
		Target:    "user@fake-target",
		TargetDir: targetDir,
	})
	if !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("got %v, want ErrFingerprintMismatch", err)
	}
}

// TestScriptForArgs_QuotesParensIntact guards the SQL-parens regression: a
// single-command arg like "SELECT MAX(version)" must survive quoting and
// remain syntactically valid bash — the old approach (concatenating a PATH
// prefix and quoted args into one string SSH re-parsed on the remote side)
// broke this. bash -n only checks syntax; it needs no real ssh target and
// no mysql binary.
func TestScriptForArgs_QuotesParensIntact(t *testing.T) {
	got := scriptForArgs([]string{"mysql", "-e", "SELECT MAX(version)"})
	if !strings.Contains(got, "export PATH=") {
		t.Error("expected PATH augmentation in generated script")
	}
	if !strings.Contains(got, "'SELECT MAX(version)'") {
		t.Errorf("expected the parens-bearing arg to be single-quoted intact, got script:\n%s", got)
	}
	if out, err := exec.Command("bash", "-n", "-c", got).CombinedOutput(); err != nil {
		t.Fatalf("generated script is not valid bash syntax: %v: %s", err, out)
	}
}

// TestScriptForArgs_RoundTripsEmbeddedSingleQuote actually executes the
// generated script (printf is always present) to prove shellQuote's
// escaping is correct end to end, not just syntactically parseable.
func TestScriptForArgs_RoundTripsEmbeddedSingleQuote(t *testing.T) {
	got := scriptForArgs([]string{"printf", "%s", "it's here"})
	out, err := exec.Command("bash", "-c", got).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v: %s", err, out)
	}
	if string(out) != "it's here" {
		t.Errorf("got %q, want %q", out, "it's here")
	}
}

// TestScriptForArgs_QuotingSuppressesTildeExpansion documents the other
// historical bug: quoting an arg containing a literal ~ suppresses tilde
// expansion, which is exactly why callers must never pass ~ in a path arg
// anymore — paths are always built from a probed absolute $HOME (TargetInfo)
// instead.
func TestScriptForArgs_QuotingSuppressesTildeExpansion(t *testing.T) {
	got := scriptForArgs([]string{"echo", "~/foo"})
	if !strings.Contains(got, "'~/foo'") {
		t.Errorf("expected ~ arg to be quoted literally (callers must never pass ~), got: %s", got)
	}
}

func TestWrapScript_PrependsPathAugmentation(t *testing.T) {
	got := wrapScript("echo hi\n")
	if !strings.Contains(got, "export PATH=") {
		t.Error("expected PATH augmentation")
	}
	if !strings.HasSuffix(got, "echo hi\n") {
		t.Errorf("expected original script body preserved at the end, got: %s", got)
	}
}

package clone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SSHRunner executes a single command on target over SSH and returns
// combined stdout+stderr. Injected for testability — no real SSH target
// needed to unit-test the orchestration logic.
type SSHRunner func(ctx context.Context, target string, args ...string) (output string, err error)

// shellQuote quotes s for a POSIX shell: wraps in single quotes, escaping
// any embedded single quotes. Safe for args containing parentheses, spaces,
// semicolons, tildes — anything — since the quoted result only ever lands
// inside a real script file or a single pre-assembled remote command string
// (see runScriptOnTarget), never re-split by a second layer of shell
// parsing the way SSH's own naive argv-join used to force.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// pathAugment is prepended to every generated remote script so
// user-installed binaries (e.g. ~/.local/bin/claude) are found in
// noninteractive SSH sessions, which get a minimal PATH.
const pathAugment = "export PATH=\"$HOME/.local/bin:$HOME/bin:$PATH\"\n"

// scriptForArgs builds a self-contained bash script that execs args, each
// individually shell-quoted. Callers must never pass a literal ~ in an
// argument — remote paths are always built from a probed absolute $HOME
// (see TargetInfo) — because quoting an argument, which this always does,
// suppresses tilde expansion.
func scriptForArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\n")
	b.WriteString(pathAugment)
	b.WriteString("exec ")
	b.WriteString(strings.Join(quoted, " "))
	b.WriteString("\n")
	return b.String()
}

// wrapScript prefixes a caller-supplied multi-line script (e.g.
// remoteFingerprintScript, installInvocationScript) with the same PATH
// augmentation scriptForArgs uses. Scripts that already set -euo pipefail
// themselves are unaffected — repeating it is harmless.
func wrapScript(script string) string {
	return "#!/bin/bash\nset -euo pipefail\n" + pathAugment + script
}

// runScriptOnTarget writes script to a local temp file, scp's it to a
// throwaway path on target (reusing the local temp file's random basename
// so concurrent invocations against the same target can't collide), then
// runs it with a single, already-quoted remote command string — never
// multiple argv elements handed to `ssh`, since ssh(1) concatenates
// anything beyond the target with a bare space and hands the result to the
// remote login shell for a second round of parsing. Building one
// pre-quoted string locally means that second round is a no-op: quoting is
// interpreted exactly once, by the remote bash actually running the
// script. This is what fixes the whole class of bugs the old
// concatenate-args-and-let-SSH-reparse approach kept hitting (PATH
// prefixes colliding with SQL parens, tilde expansion breaking under
// quoting, ...).
func runScriptOnTarget(ctx context.Context, target, script string, scriptArgs ...string) (string, error) {
	f, err := os.CreateTemp("", "teamster-clone-script-*.sh")
	if err != nil {
		return "", fmt.Errorf("clone: creating local script file: %w", err)
	}
	localPath := f.Name()
	defer os.Remove(localPath) //nolint:errcheck
	_, writeErr := f.WriteString(script)
	closeErr := f.Close()
	if writeErr != nil {
		return "", fmt.Errorf("clone: writing local script file: %w", writeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("clone: closing local script file: %w", closeErr)
	}

	remotePath := "/tmp/" + filepath.Base(localPath)
	if err := DefaultUploader(ctx, localPath, target, remotePath); err != nil {
		return "", fmt.Errorf("clone: uploading script to %s: %w", target, err)
	}
	defer func() {
		// Best-effort: a leaked /tmp file on a disposable clone target
		// isn't worth failing the run over.
		cleanup := exec.CommandContext(ctx, "ssh", target, "rm -f "+shellQuote(remotePath))
		_ = cleanup.Run()
	}()

	remoteCmd := "bash " + shellQuote(remotePath)
	for _, a := range scriptArgs {
		remoteCmd += " " + shellQuote(a)
	}
	cmd := exec.CommandContext(ctx, "ssh", target, remoteCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("ssh %s bash %s: %w: %s", target, remotePath, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// DefaultSSHRunner assembles args into a real script file (scriptForArgs)
// and runs it on target via runScriptOnTarget — see that function's
// comment for why a real script file, not concatenated SSH argv, is what
// makes this safe.
func DefaultSSHRunner(ctx context.Context, target string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("clone: DefaultSSHRunner: no command given")
	}
	out, err := runScriptOnTarget(ctx, target, scriptForArgs(args))
	if err != nil {
		return out, fmt.Errorf("clone: ssh %s %q: %w", target, strings.Join(args, " "), err)
	}
	return out, nil
}

// SSHScriptRunner executes a multi-line script on target, passing args as
// the script's positional parameters ($1, $2, ...). Used for remote logic
// (the fingerprint reconstruction, the installer invocation) that doesn't
// fit cleanly as a single command line.
type SSHScriptRunner func(ctx context.Context, target, script string, args ...string) (output string, err error)

// DefaultSSHScriptRunner is SSHScriptRunner's real implementation. Ships
// script to target as a real file (runScriptOnTarget) rather than piping it
// over SSH's stdin — the same file-based transport DefaultSSHRunner uses,
// for one consistent code path.
func DefaultSSHScriptRunner(ctx context.Context, target, script string, args ...string) (string, error) {
	return runScriptOnTarget(ctx, target, wrapScript(script), args...)
}

// Uploader pushes a local file to target:remotePath (I6: transport is
// push-only, source → target). Injected for testability.
type Uploader func(ctx context.Context, localPath, target, remotePath string) error

// DefaultUploader shells out to `scp`.
func DefaultUploader(ctx context.Context, localPath, target, remotePath string) error {
	cmd := exec.CommandContext(ctx, "scp", "-q", localPath, target+":"+remotePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("scp %s %s:%s: %w: %s", localPath, target, remotePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Deps bundles every external-effect seam ship/verify logic needs. Fields
// left nil get their Default* implementation (see WithDefaults).
type Deps struct {
	Run       CommandRunner   // local git/tar subprocesses
	SSHRun    SSHRunner       // single remote commands
	SSHScript SSHScriptRunner // multi-line remote scripts
	Upload    Uploader        // push-only file transport (I6)
}

// WithDefaults fills any nil field with its real (shells-out) implementation.
func (d Deps) WithDefaults() Deps {
	if d.Run == nil {
		d.Run = DefaultRunner
	}
	if d.SSHRun == nil {
		d.SSHRun = DefaultSSHRunner
	}
	if d.SSHScript == nil {
		d.SSHScript = DefaultSSHScriptRunner
	}
	if d.Upload == nil {
		d.Upload = DefaultUploader
	}
	return d
}

// sha256File checksums a local file — the transport-integrity check (WP1
// §2 step 6), independent of and in addition to the content-manifest check.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// archiveClean builds a git-archive tarball of fullHash at tarballPath
// (WP1 §2 step 3, productized from deliver-to-phantom.sh:82-83). No
// --prefix: the extracted tree lands as a bare repo-root layout.
func archiveClean(ctx context.Context, run CommandRunner, repoDir, fullHash, tarballPath string) error {
	f, err := os.Create(tarballPath)
	if err != nil {
		return fmt.Errorf("clone: creating tarball %s: %w", tarballPath, err)
	}
	defer f.Close() //nolint:errcheck
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "archive", "--format=tar.gz", fullHash)
	cmd.Stdout = f
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clone: git archive %s: %w: %s", fullHash, err, stderr.String())
	}
	return nil
}

// archiveDirty tars exactly the enumerated fileset (WP1 §4 Option 2) —
// tracked-as-modified plus untracked-but-not-ignored files, respecting
// .gitignore. Missing (deleted-but-unstaged) paths are skipped rather than
// failing the whole ship.
func archiveDirty(ctx context.Context, repoDir string, files []string, tarballPath string) error {
	var present []string
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(repoDir, f)); err == nil {
			present = append(present, f)
		}
	}
	args := append([]string{"czf", tarballPath, "-C", repoDir}, present...)
	cmd := exec.CommandContext(ctx, "tar", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("clone: tar dirty fileset: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ShipResult is what a successful ship (WP1 §2/§3) hands to WP2 (§8):
// a content-verified directory on the target, plus the three build-string
// values WP2 must export before invoking lib/installrunner.sh.
type ShipResult struct {
	BuildStrings
	TargetDir           string
	TransportChecksum   string // local tarball sha256, verified against a remote re-checksum
	ExpectedFingerprint string
	ActualFingerprint   string
}

// ShipOptions configures a ship run.
type ShipOptions struct {
	RepoDir    string // source of the archive; a github-repo fetch dir is passed here too (§3 step 2)
	FullHash   string
	AllowDirty bool
	Target     string // user@host
	// TargetDir is a fresh directory on target; caller picks the convention
	// ($HOME/clone-src/<full_hash>/). Must be an absolute path built from a
	// probed TargetInfo.Home (see ProbeTarget), never a literal ~ prefix —
	// SSH does not reliably expand a tilde once the argument carrying it
	// has been shell-quoted, which runScriptOnTarget always does.
	TargetDir string
}

// Ship executes WP1 §2 in full: dirty check (unless AllowDirty), archive,
// push-only transport with a checksum, enforced-fresh extraction, and the
// content-manifest provenance gate — all before the installer is ever
// invoked. Returns ErrFingerprintMismatch on a provenance failure.
//
// For --github-repo mode, callers first resolve+fetch into a throwaway
// directory (ResolveFullHashGithubRepo) and pass that directory as RepoDir
// — from here on, shipping is identical to --repo-dir (WP1 §3 step 2).
func Ship(ctx context.Context, deps Deps, opts ShipOptions) (ShipResult, error) {
	deps = deps.WithDefaults()

	source, err := DeriveBuildStrings(ctx, deps.Run, opts.RepoDir, opts.FullHash, opts.AllowDirty)
	if err != nil {
		return ShipResult{}, err
	}

	var expectedFP string
	tmpDir, err := os.MkdirTemp("", "teamster-clone-*")
	if err != nil {
		return ShipResult{}, fmt.Errorf("clone: creating local scratch dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck
	tarballPath := filepath.Join(tmpDir, "teamster-"+source.ShortHash+".tar.gz")

	if opts.AllowDirty {
		files, err := EnumerateDirtyFiles(ctx, deps.Run, opts.RepoDir)
		if err != nil {
			return ShipResult{}, err
		}
		if err := archiveDirty(ctx, opts.RepoDir, files, tarballPath); err != nil {
			return ShipResult{}, err
		}
		expectedFP, err = SourceFingerprintDirty(ctx, deps.Run, opts.RepoDir, files)
		if err != nil {
			return ShipResult{}, err
		}
	} else {
		status, err := CheckDirty(ctx, deps.Run, opts.RepoDir)
		if err != nil {
			return ShipResult{}, err
		}
		if status.Dirty {
			return ShipResult{}, &RefusalError{Err: ErrDirtyRefused, Message: DirtyRefusalMessage(opts.RepoDir, opts.FullHash, len(status.Files))}
		}
		if err := archiveClean(ctx, deps.Run, opts.RepoDir, opts.FullHash, tarballPath); err != nil {
			return ShipResult{}, err
		}
		expectedFP, err = SourceFingerprintClean(ctx, deps.Run, opts.RepoDir, opts.FullHash)
		if err != nil {
			return ShipResult{}, err
		}
	}

	localSum, err := sha256File(tarballPath)
	if err != nil {
		return ShipResult{}, fmt.Errorf("clone: checksumming tarball: %w", err)
	}

	remoteTarball := "/tmp/teamster-clone-" + source.ShortHash + ".tar.gz"
	if err := deps.Upload(ctx, tarballPath, opts.Target, remoteTarball); err != nil {
		return ShipResult{}, err
	}

	remoteSum, err := deps.SSHRun(ctx, opts.Target, "sha256sum", remoteTarball)
	if err != nil {
		return ShipResult{}, fmt.Errorf("clone: checksumming transported tarball on %s: %w", opts.Target, err)
	}
	if fields := strings.Fields(remoteSum); len(fields) == 0 || fields[0] != localSum {
		return ShipResult{}, fmt.Errorf("clone: transport integrity check failed: local sha256 %s, remote reports %q", localSum, strings.TrimSpace(remoteSum))
	}

	// Enforced-fresh extraction (WP1 §2 step 5, red-team §D3): mkdir with no
	// -p, so an existing path fails loudly rather than silently reusing a
	// stale directory. `mkdir` failing is itself the invariant — no separate
	// detection step. The parent (e.g. $HOME/clone-src/) is created with -p
	// since it won't exist on a fresh target; only the leaf must be
	// genuinely new.
	parentDir := filepath.Dir(opts.TargetDir)
	if _, err := deps.SSHRun(ctx, opts.Target, "mkdir", "-p", parentDir); err != nil {
		return ShipResult{}, fmt.Errorf("clone: creating parent directory %s on %s: %w", parentDir, opts.Target, err)
	}
	if _, err := deps.SSHRun(ctx, opts.Target, "mkdir", opts.TargetDir); err != nil {
		return ShipResult{}, fmt.Errorf("clone: target directory %s already exists (a clone is not idempotent — remove it manually before retrying) or could not be created: %w", opts.TargetDir, err)
	}
	if _, err := deps.SSHRun(ctx, opts.Target, "tar", "xzf", remoteTarball, "-C", opts.TargetDir); err != nil {
		return ShipResult{}, fmt.Errorf("clone: extracting on %s: %w", opts.Target, err)
	}
	if _, err := deps.SSHRun(ctx, opts.Target, "rm", "-f", remoteTarball); err != nil {
		return ShipResult{}, fmt.Errorf("clone: cleaning up remote tarball on %s: %w", opts.Target, err)
	}

	// Ensure git is available on target for the fingerprint check — a fresh
	// minimal VM may not have it, and the installer (which installs git as a
	// prereq) hasn't run yet at this point in the pipeline.
	if _, err := deps.SSHRun(ctx, opts.Target, "sudo", "apt-get", "install", "-y", "-qq", "git"); err != nil {
		return ShipResult{}, fmt.Errorf("clone: ensuring git on %s for fingerprint check: %w", opts.Target, err)
	}

	actualFP, err := TargetFingerprint(ctx, deps.SSHScript, opts.Target, opts.TargetDir)
	if err != nil {
		return ShipResult{}, err
	}
	if err := CompareFingerprints(expectedFP, actualFP); err != nil {
		return ShipResult{}, err
	}

	return ShipResult{
		BuildStrings:        source,
		TargetDir:           opts.TargetDir,
		TransportChecksum:   localSum,
		ExpectedFingerprint: expectedFP,
		ActualFingerprint:   actualFP,
	}, nil
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/backup"
	"github.com/bmjdotnet/teamster/internal/clone"
)

func TestAssertTargetSchemaCurrent_Match(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		if len(args) < 2 || args[0] != "~/teamster/bin/teamster" || args[1] != "sql" {
			t.Fatalf("unexpected command: %v", args)
		}
		return "63\n", nil
	}
	if err := assertTargetSchemaCurrent(context.Background(), sshRun, "user@chunk", "~/teamster/bin/teamster", 63); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAssertTargetSchemaCurrent_Mismatch(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return "40\n", nil
	}
	err := assertTargetSchemaCurrent(context.Background(), sshRun, "user@chunk", "~/teamster/bin/teamster", 63)
	if !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("got %v, want ErrSchemaVersionMismatch", err)
	}
	if !strings.Contains(err.Error(), "v40") || !strings.Contains(err.Error(), "v63") {
		t.Errorf("error should name both versions, got: %v", err)
	}
}

func TestAssertTargetSchemaCurrent_QueryFails(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return "connection refused", errors.New("exit status 1")
	}
	err := assertTargetSchemaCurrent(context.Background(), sshRun, "user@chunk", "~/teamster/bin/teamster", 63)
	if !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("got %v, want ErrSchemaVersionMismatch", err)
	}
}

func TestAssertTargetSchemaCurrent_UnparsableOutput(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return "NULL\n", nil
	}
	err := assertTargetSchemaCurrent(context.Background(), sshRun, "user@chunk", "~/teamster/bin/teamster", 63)
	if !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("got %v, want ErrSchemaVersionMismatch", err)
	}
}

func TestStopTemporaryDaemons(t *testing.T) {
	var gotArgs []string
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		gotArgs = args
		return "", nil
	}
	if err := stopTemporaryDaemons(context.Background(), sshRun, "user@chunk"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"sudo", "systemctl", "stop", "teamster-rollup.timer", "teamster-classify.timer", "teamster-health-collector.service"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("got args %v, want %v", gotArgs, want)
	}
}

func TestRestartTemporaryDaemons(t *testing.T) {
	var gotArgs []string
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		gotArgs = args
		return "", nil
	}
	if err := restartTemporaryDaemons(context.Background(), sshRun, "user@chunk"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotArgs[2] != "start" {
		t.Errorf("expected systemctl start, got args %v", gotArgs)
	}
}

// TestStopRestartTemporaryDaemons_NeverTouchSweepOrBackup guards R11/I-E:
// the temporary quiesce list must never include sweep/backup — those are
// permanently masked (maskDisposableTimers), never stopped-and-restarted.
func TestStopRestartTemporaryDaemons_NeverTouchSweepOrBackup(t *testing.T) {
	for _, u := range temporaryQuiesceUnits {
		if strings.Contains(u, "sweep") || strings.Contains(u, "backup") {
			t.Errorf("temporaryQuiesceUnits must never include sweep/backup (R11/I-E permanent-mask units), got %q", u)
		}
	}
}

// TestInvokeRestore_ForcePrecedesPositionalDir is load-bearing, not
// stylistic: Go's flag.FlagSet stops recognizing flags at the first
// non-flag token, so `restore <dir> --force` silently leaves force=false
// and falls through to the interactive confirmation prompt — which hangs
// forever inside an SSH pipeline with no stdin to read it. Verified
// directly against flag.FlagSet.Parse before writing this assertion.
func TestInvokeRestore_ForcePrecedesPositionalDir(t *testing.T) {
	var gotArgs []string
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		gotArgs = args
		return "restore complete", nil
	}
	if err := invokeRestore(context.Background(), sshRun, "user@chunk", "~/teamster/bin/teamster", "~/clone-data/abc123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"~/teamster/bin/teamster", "restore", "--force", "~/clone-data/abc123"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("got args %v, want %v (--force MUST precede the positional dir — see comment)", gotArgs, want)
	}

	// Cross-check against the real flag.FlagSet the restore CLI actually
	// uses (mirrors runRestore's own parse in cmd/teamster/backup.go), so
	// this test breaks if that CLI's flag-parsing behavior ever changes.
	fs := flag.NewFlagSet("teamster restore", flag.ContinueOnError)
	force := fs.Bool("force", false, "")
	// gotArgs[0] is the binary path, gotArgs[1] is the "restore" subcommand
	// token itself — neither is seen by runRestore's own fs.Parse, which
	// only receives the args after subcommand dispatch.
	if err := fs.Parse(gotArgs[2:]); err != nil {
		t.Fatalf("parsing invokeRestore's own args: %v", err)
	}
	if !*force {
		t.Fatal("invokeRestore's argument order does not actually set force=true when parsed the way the real CLI parses it")
	}
	if fs.NArg() != 1 || fs.Arg(0) != "~/clone-data/abc123" {
		t.Fatalf("expected exactly one positional arg (the restore dir), got NArg=%d Args=%v", fs.NArg(), fs.Args())
	}
}

func TestInvokeRestore_Failure(t *testing.T) {
	sshRun := func(ctx context.Context, target string, args ...string) (string, error) {
		return "restore: dump file not found", errors.New("exit status 1")
	}
	if err := invokeRestore(context.Background(), sshRun, "user@chunk", "~/teamster/bin/teamster", "~/clone-data/abc123"); err == nil {
		t.Fatal("expected error")
	}
}

func TestReadCloneVerifyROPassword(t *testing.T) {
	dir := t.TempDir()
	cloneDir := filepath.Join(dir, "var", "clone")
	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "clone_verify_ro_password"), []byte("s3kret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pw, err := readCloneVerifyROPassword(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pw != "s3kret" {
		t.Errorf("got %q, want %q", pw, "s3kret")
	}
}

func TestReadCloneVerifyROPassword_Missing(t *testing.T) {
	dir := t.TempDir()
	_, err := readCloneVerifyROPassword(dir)
	if !errors.Is(err, ErrCloneVerifyRONotProvisioned) {
		t.Fatalf("got %v, want ErrCloneVerifyRONotProvisioned", err)
	}
}

func TestTransferSnapshot(t *testing.T) {
	snapshotDir := t.TempDir()
	writeManifestFixture(t, snapshotDir, backup.Manifest{
		Stores: map[string]backup.StoreResult{
			"mysql":    {Status: "ok", Files: []string{"mysql/teamster.sql.gz"}},
			"teamster": {Status: "ok", Files: []string{"teamster/config.tar.gz", "teamster/state.tar.gz"}},
		},
	})
	if err := os.MkdirAll(filepath.Join(snapshotDir, "mysql"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "mysql", "teamster.sql.gz"), []byte("dump"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(snapshotDir, "teamster"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "teamster", "state.tar.gz"), []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	// config.tar.gz deliberately NOT written to disk — TransferCandidates
	// must exclude it before Transfer ever stats for it (I1).

	var uploaded []string
	var mkdirs []string
	deps := clone.Deps{
		Upload: func(ctx context.Context, localPath, target, remotePath string) error {
			uploaded = append(uploaded, remotePath)
			return nil
		},
		SSHRun: func(ctx context.Context, target string, args ...string) (string, error) {
			if len(args) >= 2 && args[0] == "mkdir" {
				mkdirs = append(mkdirs, args[len(args)-1])
			}
			return "", nil
		},
	}.WithDefaults()

	if err := transferSnapshot(context.Background(), deps, snapshotDir, "user@chunk", "~/clone-data/abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantUploaded := []string{
		"~/clone-data/abc/manifest.json",
		"~/clone-data/abc/mysql/teamster.sql.gz",
		"~/clone-data/abc/teamster/state.tar.gz",
	}
	if strings.Join(uploaded, ",") != strings.Join(wantUploaded, ",") {
		t.Errorf("uploaded = %v, want %v", uploaded, wantUploaded)
	}
	for _, want := range []string{"~/clone-data/abc", "~/clone-data/abc/mysql", "~/clone-data/abc/teamster"} {
		found := false
		for _, m := range mkdirs {
			if m == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected mkdir -p %s, got mkdirs %v", want, mkdirs)
		}
	}
}

func TestTransferSnapshot_MissingCandidateFileErrors(t *testing.T) {
	snapshotDir := t.TempDir()
	writeManifestFixture(t, snapshotDir, backup.Manifest{
		Stores: map[string]backup.StoreResult{
			"mysql": {Status: "ok", Files: []string{"mysql/teamster.sql.gz"}}, // never written to disk
		},
	})
	deps := clone.Deps{
		Upload: func(ctx context.Context, localPath, target, remotePath string) error { return nil },
		SSHRun: func(ctx context.Context, target string, args ...string) (string, error) { return "", nil },
	}.WithDefaults()

	if err := transferSnapshot(context.Background(), deps, snapshotDir, "user@chunk", "~/clone-data/abc"); err == nil {
		t.Fatal("expected error for a manifest-listed file missing on disk")
	}
}

func writeManifestFixture(t *testing.T, dir string, m backup.Manifest) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeLocalSQLBinary writes a tiny shell script that echoes back its args
// and $TEAMSTER_STORE_DSN so runLocalSQL/verifyRowCounts can be exercised
// against a real subprocess without a real database.
func fakeLocalSQLBinary(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script test fixture requires a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-teamster")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunLocalSQL_PassesDSNViaEnvNotArgv(t *testing.T) {
	bin := fakeLocalSQLBinary(t, `
if [ -n "$(printf '%s' "$*" | grep -F 's3kret')" ]; then
  echo "FAIL: password leaked into argv" >&2
  exit 1
fi
echo "DSN=$TEAMSTER_STORE_DSN"
echo "ARGS=$*"
`)
	out, err := runLocalSQL(context.Background(), bin, "mysql://clone_verify_ro:s3kret@localhost/teamster", "SHOW TABLES", true)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "DSN=mysql://clone_verify_ro:s3kret@localhost/teamster") {
		t.Errorf("expected DSN to reach the subprocess via env, got: %s", out)
	}
	if !strings.Contains(out, "--read-only") {
		t.Errorf("expected --read-only in args when readOnly=true, got: %s", out)
	}
}

func TestRunLocalSQL_ReadOnlyFalseOmitsFlag(t *testing.T) {
	bin := fakeLocalSQLBinary(t, `echo "ARGS=$*"`)
	out, err := runLocalSQL(context.Background(), bin, "mysql://u:p@localhost/teamster", "SELECT 1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "--read-only") {
		t.Errorf("expected no --read-only when readOnly=false, got: %s", out)
	}
}

func TestRunLocalSQL_Failure(t *testing.T) {
	bin := fakeLocalSQLBinary(t, `echo "boom" >&2; exit 1`)
	if _, err := runLocalSQL(context.Background(), bin, "mysql://u:p@localhost/teamster", "SELECT 1", false); err == nil {
		t.Fatal("expected error")
	}
}

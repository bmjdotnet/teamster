package installbackup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackup_NonexistentPathIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.toml")
	ts, err := Backup(path)
	if err != nil {
		t.Fatalf("Backup on nonexistent path: %v", err)
	}
	if ts != "" {
		t.Fatalf("expected empty timestamped path, got %q", ts)
	}
	if _, err := os.Stat(path + ".pre-teamster"); !os.IsNotExist(err) {
		t.Fatalf("expected no .pre-teamster to be created, stat err = %v", err)
	}
}

func TestBackup_FirstCallCreatesPreTeamster(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "model = \"gpt-5.5\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	ts, err := Backup(path)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if ts == "" {
		t.Fatal("expected a non-empty timestamped backup path")
	}

	preData, err := os.ReadFile(path + ".pre-teamster")
	if err != nil {
		t.Fatalf("read .pre-teamster: %v", err)
	}
	if string(preData) != original {
		t.Fatalf(".pre-teamster content = %q, want %q", preData, original)
	}

	tsData, err := os.ReadFile(ts)
	if err != nil {
		t.Fatalf("read timestamped backup: %v", err)
	}
	if string(tsData) != original {
		t.Fatalf("timestamped backup content = %q, want %q", tsData, original)
	}
}

// TestBackup_PreTeamsterNeverOverwritten proves the .pre-teamster copy is a
// permanent record of the very first content Backup ever saw — later calls
// (even after the live file has changed) must not touch it, while still
// producing a fresh timestamped backup of the current content each time.
func TestBackup_PreTeamsterNeverOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "model = \"gpt-5.5\"\n# operator comment\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Backup(path); err != nil {
		t.Fatalf("first Backup: %v", err)
	}

	// Simulate the installer overwriting the live file after backing it up.
	changed := "model = \"gpt-5.5\"\n# operator comment\n[mcp_servers.wms]\ncommand = \"x\"\n"
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}

	ts2, err := Backup(path)
	if err != nil {
		t.Fatalf("second Backup: %v", err)
	}
	if ts2 == "" {
		t.Fatal("expected a non-empty timestamped backup path on second call")
	}

	preData, err := os.ReadFile(path + ".pre-teamster")
	if err != nil {
		t.Fatalf("read .pre-teamster: %v", err)
	}
	if string(preData) != original {
		t.Fatalf(".pre-teamster was overwritten: got %q, want original %q", preData, original)
	}

	ts2Data, err := os.ReadFile(ts2)
	if err != nil {
		t.Fatalf("read second timestamped backup: %v", err)
	}
	if string(ts2Data) != changed {
		t.Fatalf("second timestamped backup content = %q, want %q (the pre-this-write state)", ts2Data, changed)
	}
}

func TestRestore_FromTimestampedBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "model = \"gpt-5.5\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := Backup(path)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a rejected write (e.g. failed doctor gate).
	if err := os.WriteFile(path, []byte("this is broken toml [[["), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Restore(ts, path); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("restored content = %q, want %q", restored, original)
	}
}

func TestRestore_EmptyBackupPathRemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	// Simulate WriteMCPServers creating a brand new file this run (Backup
	// returned "" because the file didn't exist beforehand) and then that
	// write getting rejected by the doctor gate.
	if err := os.WriteFile(path, []byte("newly created, then rejected"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Restore("", path); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected path to be removed, stat err = %v", err)
	}
}

func TestRestore_EmptyBackupPathOnAlreadyAbsentFileIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-existed.toml")
	if err := Restore("", path); err != nil {
		t.Fatalf("Restore on already-absent path should be a no-op, got: %v", err)
	}
}

func TestBackup_PreservesFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("secret = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts, err := Backup(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ts)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("timestamped backup mode = %o, want 0600", info.Mode().Perm())
	}
}

// TestBackup_TimestampFormatIsSortable is a light sanity check that repeated
// calls within the same test run don't collide on the same filename even
// when invoked in quick succession — the format truncates to whole seconds,
// so a caller invoking Backup twice in the same second on the same path
// would clobber its own prior timestamped backup. Document that limitation
// via this test rather than leave it silently discovered later.
func TestBackup_TimestampFormatIsSortable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := Backup(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse("20060102T150405Z", ts[len(path)+1:len(ts)-len(".bak")]); err != nil {
		t.Fatalf("timestamped backup name %q does not carry the expected UTC timestamp format: %v", ts, err)
	}
}

package codexconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRunDoctorGate_SkipsWhenCodexAbsent forces exec.LookPath("codex") to
// fail deterministically (an empty PATH pointing only at a fresh temp dir)
// so this test's outcome never depends on whether the host running the
// suite happens to have codex installed — the whole point of DoctorSkipped
// is to keep this package's test suite green on a codex-less CI/dev host.
func TestRunDoctorGate_SkipsWhenCodexAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	result, err := RunDoctorGate()
	if err != nil {
		t.Fatalf("RunDoctorGate with no codex on PATH should not error, got: %v", err)
	}
	if result.Status != DoctorSkipped {
		t.Fatalf("expected DoctorSkipped, got %+v", result)
	}
}

// codexAvailable reports whether a real `codex` binary is on PATH, so the
// remaining tests can exercise the real integration and still skip cleanly
// (not fail) on a host without Codex installed.
func codexAvailable(t *testing.T) bool {
	t.Helper()
	_, err := exec.LookPath("codex")
	return err == nil
}

func TestRunDoctorGate_RealCodex_ValidConfigLoads(t *testing.T) {
	if !codexAvailable(t) {
		t.Skip("codex not found in PATH — skipping real-binary integration test")
	}

	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	configPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(configPath, []byte("model = \"gpt-5.5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := RunDoctorGate()
	if err != nil {
		t.Fatalf("RunDoctorGate: %v", err)
	}
	if result.Status != DoctorOK {
		t.Fatalf("expected DoctorOK for a trivially valid config, got %+v", result)
	}
}

func TestRunDoctorGate_RealCodex_InvalidConfigFails(t *testing.T) {
	if !codexAvailable(t) {
		t.Skip("codex not found in PATH — skipping real-binary integration test")
	}

	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	configPath := filepath.Join(codexHome, "config.toml")
	// Same shape verified live in toml-lib-decision.md: a bare non-"none"
	// exporter-family string, properly nested under [otel], with no
	// accompanying table value on that key — confirmed to fail
	// codex's own config load (not just a TOML syntax error).
	broken := "model = \"gpt-5.5\"\n\n[otel]\nmetrics_exporter = \"otlp-http\"\n"
	if err := os.WriteFile(configPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := RunDoctorGate()
	if err != nil {
		t.Fatalf("RunDoctorGate: %v", err)
	}
	if result.Status != DoctorFailed {
		t.Fatalf("expected DoctorFailed for a config codex itself rejects, got %+v", result)
	}
	if result.Summary == "" {
		t.Error("expected a non-empty Summary on failure")
	}
}

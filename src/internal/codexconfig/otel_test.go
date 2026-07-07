package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOtelSpec_Render_MatchesVerifiedShape(t *testing.T) {
	spec := TeamsterOtelSpec("http://localhost:4329", "production")
	got := spec.render()
	want := `[otel]
environment = "production"
log_user_prompt = false
exporter = "none"
metrics_exporter = { otlp-http = { endpoint = "http://localhost:4329", protocol = "binary" } }
trace_exporter = "none"
`
	if got != want {
		t.Fatalf("render() =\n%q\nwant\n%q", got, want)
	}
}

// TestOtelSpec_Render_DefaultsProtocolForOtlpHttp is the load-bearing
// regression test for a live-verified footgun: omitting `protocol` on an
// otlp-http metrics_exporter is not merely a stylistic choice like it is for
// otlp-grpc — Codex's own config loader rejects it outright ("missing field
// `protocol` in `otel.metrics_exporter`", verified 2026-07-07 against Codex
// 0.137.0). A caller that constructs OtelSpec directly (bypassing
// TeamsterOtelSpec, which always sets it) must not be able to produce a
// non-loading config by simply leaving this field empty.
func TestOtelSpec_Render_DefaultsProtocolForOtlpHttp(t *testing.T) {
	spec := OtelSpec{Environment: "production", MetricsBackend: "otlp-http", MetricsEndpoint: "http://localhost:4329"}
	got := spec.render()
	if !strings.Contains(got, `metrics_exporter = { otlp-http = { endpoint = "http://localhost:4329", protocol = "binary" } }`) {
		t.Fatalf("expected protocol to default to \"binary\" for otlp-http, got:\n%s", got)
	}
}

func TestOtelSpec_Render_OtlpGrpcOmitsProtocol(t *testing.T) {
	spec := OtelSpec{Environment: "production", MetricsBackend: "otlp-grpc", MetricsEndpoint: "localhost:4327"}
	got := spec.render()
	if strings.Contains(got, "protocol") {
		t.Errorf("expected no protocol key for otlp-grpc, got:\n%s", got)
	}
	if !strings.Contains(got, `metrics_exporter = { otlp-grpc = { endpoint = "localhost:4327" } }`) {
		t.Errorf("expected otlp-grpc inline table, got:\n%s", got)
	}
}

// TestOtelSpec_Render_ExporterAlwaysNone is the load-bearing regression test
// for OtelSpec's core design decision: exporter (logs/traces) is always
// "none", never configurable, because enabling it alongside metrics_exporter
// on Codex's HTTP exporter (which always POSTs to the endpoint's bare root,
// live-verified 2026-07-07) would collide on the same URL path and panic the
// collector at startup ("multiple registrations for /").
func TestOtelSpec_Render_ExporterAlwaysNone(t *testing.T) {
	spec := TeamsterOtelSpec("http://localhost:4329", "production")
	got := spec.render()
	if !strings.Contains(got, "exporter = \"none\"\n") {
		t.Fatalf("expected exporter = \"none\", got:\n%s", got)
	}
}

func TestOtelSpec_Render_Deterministic(t *testing.T) {
	spec := TeamsterOtelSpec("http://localhost:4329", "production")
	if spec.render() != spec.render() {
		t.Fatal("render() is not deterministic across calls")
	}
}

func TestWriteOtelConfig_FreshFile_NoCodexOnPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PATH", t.TempDir()) // no codex — doctor gate must skip, not fail

	spec := TeamsterOtelSpec("http://localhost:4329", "production")
	result, err := WriteOtelConfig(path, spec)
	if err != nil {
		t.Fatalf("WriteOtelConfig: %v", err)
	}
	if result.Doctor.Status != DoctorSkipped {
		t.Fatalf("expected doctor gate to skip with no codex on PATH, got %+v", result.Doctor)
	}
	if !result.Servers["otel"].Changed {
		t.Errorf("expected Servers[otel].Changed=true on fresh write, got %+v", result.Servers["otel"])
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[otel]") {
		t.Fatalf("expected [otel] table in output:\n%s", data)
	}
}

// TestWriteOtelConfig_RerunReplacesInPlace is the load-bearing regression
// test for AlwaysUpsert on this section: a changed endpoint (e.g. an operator
// re-running the installer after moving otelcol to a different port) must
// replace the old block in place, never leave a stale duplicate.
func TestWriteOtelConfig_RerunReplacesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PATH", t.TempDir())

	first := TeamsterOtelSpec("http://localhost:4329", "production")
	if _, err := WriteOtelConfig(path, first); err != nil {
		t.Fatalf("first WriteOtelConfig: %v", err)
	}

	second := TeamsterOtelSpec("http://localhost:9999", "staging")
	result, err := WriteOtelConfig(path, second)
	if err != nil {
		t.Fatalf("second WriteOtelConfig: %v", err)
	}
	if !result.Servers["otel"].Changed {
		t.Fatalf("expected AlwaysUpsert rerun to report Changed=true, got %+v", result.Servers["otel"])
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Count(content, "[otel]") != 1 {
		t.Fatalf("expected exactly one [otel] header after rerun, got:\n%s", content)
	}
	if strings.Contains(content, "4329") || strings.Contains(content, "production") {
		t.Errorf("expected the old endpoint/environment to be gone after rerun, got:\n%s", content)
	}
	if !strings.Contains(content, "9999") || !strings.Contains(content, "staging") {
		t.Errorf("expected the new endpoint/environment to be present, got:\n%s", content)
	}
}

func TestWriteOtelConfig_UnmarkedCollisionNeverTouchesForeignOtelTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "model = \"gpt-5.5\"\n\n[otel]\nmetrics_exporter = \"none\"\n" // operator's own, unmarked
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	// PATH intentionally left alone, matching mcpserver_test.go's no-op
	// convention: the real assertion is that Doctor never runs, not that
	// codex happens to be absent.

	spec := TeamsterOtelSpec("http://localhost:4329", "production")
	result, err := WriteOtelConfig(path, spec)
	if err != nil {
		t.Fatalf("WriteOtelConfig: %v", err)
	}
	if !result.Servers["otel"].UnmarkedCollision {
		t.Fatalf("expected UnmarkedCollision (operator already has an unmarked [otel]), got %+v", result.Servers["otel"])
	}
	if result.Doctor != (DoctorResult{}) {
		t.Fatalf("expected the doctor gate to never run on a collision no-op, got %+v", result.Doctor)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("expected foreign [otel] table left byte-identical:\ngot:  %q\nwant: %q", data, original)
	}
	if _, statErr := os.Stat(path + ".pre-teamster"); !os.IsNotExist(statErr) {
		t.Fatalf("expected no backup on a collision no-op, stat err = %v", statErr)
	}
}

// TestWriteOtelConfig_RealCodex_DoctorGateAcceptsValidWrite is the full
// integration test: writes the [otel] table into an operator-authored config
// in an isolated CODEX_HOME and confirms `codex --strict-config doctor`
// actually accepts the result. Skips cleanly on a codex-less host.
func TestWriteOtelConfig_RealCodex_DoctorGateAcceptsValidWrite(t *testing.T) {
	if !codexAvailable(t) {
		t.Skip("codex not found in PATH — skipping real-binary integration test")
	}
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(path, []byte(readTestdata(t, "operator-authored.toml")), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := TeamsterOtelSpec("http://localhost:4329", "production")
	result, err := WriteOtelConfig(path, spec)
	if err != nil {
		t.Fatalf("WriteOtelConfig: %v", err)
	}
	if result.Doctor.Status != DoctorOK {
		t.Fatalf("expected DoctorOK, got %+v", result.Doctor)
	}
	if result.RolledBack {
		t.Fatal("did not expect a rollback on a valid write")
	}
}

// TestWriteOtelConfig_RealCodex_RollsBackOnDoctorRejection proves the
// rollback path end to end using a malformed OtelSpec (empty MetricsBackend,
// which renders an inline table with no key at all — invalid TOML,
// live-verified 2026-07-07 to fail codex's config load). Confirms
// WriteOtelConfig restores the pre-write bytes and reports an error, rather
// than leaving a host that can't start codex at all.
func TestWriteOtelConfig_RealCodex_RollsBackOnDoctorRejection(t *testing.T) {
	if !codexAvailable(t) {
		t.Skip("codex not found in PATH — skipping real-binary integration test")
	}
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "config.toml")
	preExisting := "model = \"gpt-5.5\"\n"
	if err := os.WriteFile(path, []byte(preExisting), 0o644); err != nil {
		t.Fatal(err)
	}

	broken := OtelSpec{Environment: "production", MetricsBackend: "", MetricsEndpoint: "http://localhost:4329", MetricsProtocol: "binary"}
	result, err := WriteOtelConfig(path, broken)
	if err == nil {
		t.Fatal("expected an error from a doctor-gate rejection")
	}
	if !result.RolledBack {
		t.Fatalf("expected RolledBack=true, got %+v", result)
	}
	if result.Doctor.Status != DoctorFailed {
		t.Fatalf("expected DoctorFailed, got %+v", result.Doctor)
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != preExisting {
		t.Fatalf("expected rollback to restore pre-write content exactly:\ngot:  %q\nwant: %q", data, preExisting)
	}
}

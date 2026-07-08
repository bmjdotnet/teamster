package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPServerSpec_Render_DeterministicEnvOrdering(t *testing.T) {
	spec := MCPServerSpec{
		ID:                       "wms",
		Command:                  "/home/testuser/teamster/bin/wms-mcp",
		DefaultToolsApprovalMode: "approve",
		Env: map[string]string{
			"TEAMSTER_STORE_DSN":      "mysql://user:pass@host/db",
			"TEAMSTER_RUNTIME":        "codex",
			"TEAMSTER_HOOK_SERVER_URL": "http://hub:9125/event",
			"TEAMSTER_HOST":           "testhost",
		},
	}
	got1 := spec.render()
	got2 := spec.render()
	if got1 != got2 {
		t.Fatalf("render() is not deterministic across calls:\n%q\nvs\n%q", got1, got2)
	}
	want := `[mcp_servers.wms]
command = "/home/testuser/teamster/bin/wms-mcp"
default_tools_approval_mode = "approve"
env = { TEAMSTER_HOOK_SERVER_URL = "http://hub:9125/event", TEAMSTER_HOST = "testhost", TEAMSTER_RUNTIME = "codex", TEAMSTER_STORE_DSN = "mysql://user:pass@host/db" }
`
	if got1 != want {
		t.Fatalf("render() =\n%q\nwant\n%q", got1, want)
	}
}

func TestMCPServerSpec_Render_OmitsEmptyApprovalMode(t *testing.T) {
	spec := MCPServerSpec{ID: "activity", Command: "/x/bin/activity-mcp"}
	got := spec.render()
	if strings.Contains(got, "default_tools_approval_mode") {
		t.Errorf("expected no approval-mode line when empty, got:\n%s", got)
	}
	if strings.Contains(got, "env =") {
		t.Errorf("expected no env line when Env is empty, got:\n%s", got)
	}
}

func TestMCPServerSpec_Render_QuotesSpecialCharacters(t *testing.T) {
	spec := MCPServerSpec{
		ID:      "wms",
		Command: `C:\path\with"quote`,
	}
	got := spec.render()
	if !strings.Contains(got, `command = "C:\\path\\with\"quote"`) {
		t.Errorf("expected escaped command value, got:\n%s", got)
	}
}

func TestWriteMCPServers_FreshFileNoOperatorContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	specs := []MCPServerSpec{
		{ID: "activity", Command: "/x/bin/activity-mcp", DefaultToolsApprovalMode: "approve"},
		{ID: "wms", Command: "/x/bin/wms-mcp", DefaultToolsApprovalMode: "approve", Env: map[string]string{"TEAMSTER_RUNTIME": "codex"}},
	}

	// Point RunDoctorGate at a codex-less PATH so this unit test doesn't
	// depend on (or require) a real codex binary — the doctor-gate
	// integration itself is covered separately below when codex is present.
	t.Setenv("PATH", t.TempDir())

	result, err := WriteMCPServers(path, specs)
	if err != nil {
		t.Fatalf("WriteMCPServers: %v", err)
	}
	if result.Doctor.Status != DoctorSkipped {
		t.Fatalf("expected doctor gate to skip with no codex on PATH, got %+v", result.Doctor)
	}
	for _, id := range []string{"activity", "wms"} {
		if !result.Servers[id].Changed {
			t.Errorf("expected Servers[%q].Changed=true on fresh write", id)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "[mcp_servers.activity]") || !strings.Contains(content, "[mcp_servers.wms]") {
		t.Fatalf("expected both mcp_servers tables in output:\n%s", content)
	}
}

func TestWriteMCPServers_RerunIsIdempotentAndPreservesOperatorContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := readTestdata(t, "operator-authored.toml")
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no codex — doctor gate skips

	specs := []MCPServerSpec{
		{ID: "wms", Command: "/x/bin/wms-mcp", DefaultToolsApprovalMode: "approve"},
	}

	if _, err := WriteMCPServers(path, specs); err != nil {
		t.Fatalf("first WriteMCPServers: %v", err)
	}
	result, err := WriteMCPServers(path, specs)
	if err != nil {
		t.Fatalf("second WriteMCPServers: %v", err)
	}
	if !result.Servers["wms"].SkippedExisting {
		t.Fatalf("expected SkippedExisting=true on rerun, got %+v", result.Servers["wms"])
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Count(content, "[mcp_servers.wms]") != 1 {
		t.Fatalf("expected exactly one [mcp_servers.wms] after rerun, got:\n%s", content)
	}
	for _, comment := range []string{
		"# My personal codex config",
		"# my custom search server, do not remove",
	} {
		if !strings.Contains(content, comment) {
			t.Errorf("operator comment lost: %q", comment)
		}
	}
}

func TestWriteMCPServers_NoOpWriteSkipsBackupAndDoctorGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[mcp_servers.wms]\ncommand = \"/x\"\n" // matches literal header, no markers
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	// PATH intentionally left alone — if the code path under test tried to
	// invoke a doctor gate on this no-op, and codex happens to be on PATH on
	// this host, we'd rather have that surface as an unexpected DoctorOK in
	// result.Doctor than crash; the real assertion is that Doctor stays at
	// its zero value below, proving the gate was never invoked.

	specs := []MCPServerSpec{{ID: "wms", Command: "/x/bin/wms-mcp"}}
	result, err := WriteMCPServers(path, specs)
	if err != nil {
		t.Fatalf("WriteMCPServers: %v", err)
	}
	if !result.Servers["wms"].UnmarkedCollision {
		t.Fatalf("expected UnmarkedCollision (operator already has an unmarked [mcp_servers.wms]), got %+v", result.Servers["wms"])
	}
	if result.Doctor != (DoctorResult{}) {
		t.Fatalf("expected the doctor gate to never run on a true no-op write, got %+v", result.Doctor)
	}
	if _, err := os.Stat(path + ".pre-teamster"); !os.IsNotExist(err) {
		t.Fatalf("expected no backup to be made on a no-op write, stat err = %v", err)
	}
}

// TestWriteMCPServers_RealCodex_DoctorGateAcceptsValidWrite is the full
// integration test: writes both WP2 tables into an operator-authored config
// in an isolated CODEX_HOME and confirms `codex --strict-config doctor`
// actually accepts the result. Skips cleanly on a codex-less host.
func TestWriteMCPServers_RealCodex_DoctorGateAcceptsValidWrite(t *testing.T) {
	if !codexAvailable(t) {
		t.Skip("codex not found in PATH — skipping real-binary integration test")
	}
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(path, []byte(readTestdata(t, "operator-authored.toml")), 0o644); err != nil {
		t.Fatal(err)
	}

	specs := []MCPServerSpec{
		{ID: "activity", Command: filepath.Join(codexHome, "bin", "activity-mcp"), DefaultToolsApprovalMode: "approve"},
		{
			ID: "wms", Command: filepath.Join(codexHome, "bin", "wms-mcp"), DefaultToolsApprovalMode: "approve",
			Env: map[string]string{"TEAMSTER_RUNTIME": "codex", "TEAMSTER_STORE_DSN": "mysql://user:pass@host/db"},
		},
	}
	result, err := WriteMCPServers(path, specs)
	if err != nil {
		t.Fatalf("WriteMCPServers: %v", err)
	}
	if result.Doctor.Status != DoctorOK {
		t.Fatalf("expected DoctorOK, got %+v", result.Doctor)
	}
	if result.RolledBack {
		t.Fatal("did not expect a rollback on a valid write")
	}
}

// TestWriteMCPServers_RealCodex_RollsBackOnDoctorRejection proves the
// rollback path end to end: seed a config the doctor gate is guaranteed to
// reject (a malformed command line is not enough — codex tolerates an
// unresolvable command path at config-load time; use the same otel bare-
// enum-without-table shape verified in doctor_test.go), confirm
// WriteMCPServers restores the pre-write bytes and reports an error.
func TestWriteMCPServers_RealCodex_RollsBackOnDoctorRejection(t *testing.T) {
	if !codexAvailable(t) {
		t.Skip("codex not found in PATH — skipping real-binary integration test")
	}
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(codexHome, "config.toml")
	// Seed a config that already fails to load — WriteMCPServers's job is
	// only to gate its OWN write, but this proves rollback restores
	// whatever was on disk before this run touched it, broken or not.
	preExisting := "model = \"gpt-5.5\"\n\n[otel]\nmetrics_exporter = \"otlp-http\"\n"
	if err := os.WriteFile(path, []byte(preExisting), 0o644); err != nil {
		t.Fatal(err)
	}

	specs := []MCPServerSpec{{ID: "wms", Command: filepath.Join(codexHome, "bin", "wms-mcp")}}
	result, err := WriteMCPServers(path, specs)
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

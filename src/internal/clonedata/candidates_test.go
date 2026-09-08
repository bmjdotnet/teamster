package clonedata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bmjdotnet/teamster/internal/backup"
)

func writeManifest(t *testing.T, dir string, m backup.Manifest) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTransferCandidates_ExcludesConfigTarGz is the load-bearing assertion
// for I1's v1 mechanism (R8): config.tar.gz must never be a candidate even
// though it's a real file in the teamster store's manifest entry.
func TestTransferCandidates_ExcludesConfigTarGz(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, backup.Manifest{
		Stores: map[string]backup.StoreResult{
			"teamster": {
				Status: "ok",
				Files:  []string{"teamster/config.tar.gz", "teamster/state.tar.gz"},
			},
		},
	})

	got, err := TransferCandidates(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"manifest.json", "teamster/state.tar.gz"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TransferCandidates = %v, want %v", got, want)
	}
}

// TestTransferCandidates_SkipsOutOfScopeStores confirms prometheus/grafana
// files never become candidates even when their manifest status is "ok" —
// out of v1 scope regardless of what a manifest contains (WP3-data-leg.md
// "What's restored vs refused" table).
func TestTransferCandidates_SkipsOutOfScopeStores(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, backup.Manifest{
		Stores: map[string]backup.StoreResult{
			"mysql":      {Status: "ok", Files: []string{"mysql/teamster.sql.gz", "mysql/claude_telemetry.sql.gz"}},
			"prometheus": {Status: "ok", Files: []string{"prometheus/metrics2.tar.gz"}},
			"grafana":    {Status: "ok", Files: []string{"grafana/grafana.db"}},
			"otel":       {Status: "ok", Files: []string{"otel/otelcol.yaml"}},
			"teamster":   {Status: "ok", Files: []string{"teamster/config.tar.gz", "teamster/state.tar.gz"}},
		},
	})

	got, err := TransferCandidates(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"manifest.json",
		"mysql/claude_telemetry.sql.gz",
		"mysql/teamster.sql.gz",
		"otel/otelcol.yaml",
		"teamster/state.tar.gz",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TransferCandidates = %v, want %v", got, want)
	}
}

// TestTransferCandidates_SkipsNonOkStores mirrors RunRestore's own plan
// logic (restore.go's add() closure): a store status other than "ok" (e.g.
// "error", "partial") is never a candidate even if it's in
// TransferableStores and lists files.
func TestTransferCandidates_SkipsNonOkStores(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, backup.Manifest{
		Stores: map[string]backup.StoreResult{
			"mysql": {Status: "error", Files: []string{"mysql/teamster.sql.gz"}},
		},
	})

	got, err := TransferCandidates(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"manifest.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TransferCandidates = %v, want %v", got, want)
	}
}

func TestTransferCandidates_MissingManifestErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := TransferCandidates(dir); err == nil {
		t.Fatal("expected an error for a missing manifest.json, got nil")
	}
}

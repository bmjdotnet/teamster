package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestPruneOrphanDashboards proves the dashboards-dir mirror: BASEDIR dashboard
// JSONs absent from skel are removed, skel-shipped ones are kept, and non-.json
// / other-dir files are never touched (narrow blast radius). This is the D2 fix
// — a reinstall must drop dashboards retired upstream.
func TestPruneOrphanDashboards(t *testing.T) {
	skel := t.TempDir()
	base := t.TempDir()

	skelDash := filepath.Join(skel, "etc", "grafana", "dashboards")
	baseDash := filepath.Join(base, "etc", "grafana", "dashboards")
	mustMkdir(t, skelDash)
	mustMkdir(t, baseDash)

	// skel ships exactly these two.
	mustWrite(t, filepath.Join(skelDash, "keep-a.json"), "{}")
	mustWrite(t, filepath.Join(skelDash, "keep-b.json"), "{}")

	// BASEDIR has the two kept plus two orphans, a non-json, and a subdir.
	mustWrite(t, filepath.Join(baseDash, "keep-a.json"), "{}")
	mustWrite(t, filepath.Join(baseDash, "keep-b.json"), "{}")
	mustWrite(t, filepath.Join(baseDash, "cost-per-wms.json"), "{}") // orphan
	mustWrite(t, filepath.Join(baseDash, "wms-pulse.json"), "{}")    // orphan
	mustWrite(t, filepath.Join(baseDash, "README.txt"), "notes")     // not .json — keep
	mustMkdir(t, filepath.Join(baseDash, "subdir"))                  // dir — keep

	n, err := pruneOrphanDashboards(skel, base)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}

	got := lsNames(t, baseDash)
	want := []string{"README.txt", "keep-a.json", "keep-b.json", "subdir"}
	if len(got) != len(want) {
		t.Fatalf("remaining = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("remaining = %v, want %v", got, want)
		}
	}
}

// TestPruneOrphanDashboardsMissingDirs is a no-op (no error) when skel ships no
// dashboards or BASEDIR has none — covers fresh installs and non-grafana stages.
func TestPruneOrphanDashboardsMissingDirs(t *testing.T) {
	// Neither dir exists.
	n, err := pruneOrphanDashboards(t.TempDir(), t.TempDir())
	if err != nil || n != 0 {
		t.Fatalf("missing dirs: n=%d err=%v, want 0,nil", n, err)
	}

	// skel has dashboards, BASEDIR does not → nothing to prune.
	skel := t.TempDir()
	mustMkdir(t, filepath.Join(skel, "etc", "grafana", "dashboards"))
	mustWrite(t, filepath.Join(skel, "etc", "grafana", "dashboards", "x.json"), "{}")
	n, err = pruneOrphanDashboards(skel, t.TempDir())
	if err != nil || n != 0 {
		t.Fatalf("no dst dir: n=%d err=%v, want 0,nil", n, err)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func lsNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

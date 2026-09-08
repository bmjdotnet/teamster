package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// dashboardPanel mirrors the subset of Grafana dashboard JSON this test
// needs: a panel carries zero or more SQL targets and may nest further
// panels (a "row" panel collapses its members into its own "panels" array).
type dashboardPanel struct {
	Title   string `json:"title"`
	Targets []struct {
		RawSQL string `json:"rawSql"`
	} `json:"targets"`
	Panels []dashboardPanel `json:"panels"`
}

type dashboardJSON struct {
	Panels []dashboardPanel `json:"panels"`
}

var (
	// fromJoinRe captures the identifier immediately following a FROM or JOIN
	// keyword. It deliberately does not match when that identifier is a `(`
	// (a subquery), so `FROM (SELECT ...) AS x` never yields a false table.
	fromJoinRe = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
	// cteAliasRe matches the "<name> AS (" shape used by both `WITH name AS
	// (...)` and later CTE members (`, name AS (...)`) — the only SQL
	// construct that puts an identifier directly before "AS (". Any FROM/JOIN
	// reference to such a name is a CTE self-reference, not a real table.
	cteAliasRe = regexp.MustCompile(`(?i)\b([a-zA-Z_][a-zA-Z0-9_]*)\s+AS\s*\(`)
)

// dashboardTableRefs walks a dashboard's panel tree (recursing into nested
// "panels", which is how collapsed rows carry their members) and returns
// every base-table/view name referenced via FROM or JOIN in any target's
// rawSql, mapped to the set of panel titles that reference it. CTE names and
// subquery aliases are excluded since they never need a GRANT.
func dashboardTableRefs(d dashboardJSON) map[string]map[string]bool {
	refs := map[string]map[string]bool{}
	var walk func([]dashboardPanel)
	walk = func(panels []dashboardPanel) {
		for _, p := range panels {
			for _, target := range p.Targets {
				sql := target.RawSQL
				if sql == "" {
					continue
				}
				cte := map[string]bool{}
				for _, m := range cteAliasRe.FindAllStringSubmatch(sql, -1) {
					cte[strings.ToLower(m[1])] = true
				}
				for _, m := range fromJoinRe.FindAllStringSubmatch(sql, -1) {
					tbl := strings.ToLower(m[1])
					if tbl == "dual" || cte[tbl] {
						continue
					}
					if refs[tbl] == nil {
						refs[tbl] = map[string]bool{}
					}
					refs[tbl][p.Title] = true
				}
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)
	return refs
}

// TestGrafanaGrantCoverage asserts every table/view a Grafana dashboard's
// MySQL panel queries via FROM/JOIN has a matching GRANT SELECT in
// grafana-readonly-user.sql. This is the complement of
// TestReadonlyGrantsMatchV3Schema (grafana_grants_test.go), which checks the
// grant list isn't stale against the schema; this one checks the grant list
// isn't missing anything the dashboards actually need. A missing grant does
// not error loudly — grafana_ro's query fails and Grafana just renders an
// empty panel, which is far easier to miss in production than a test
// failure here.
func TestGrafanaGrantCoverage(t *testing.T) {
	dashDir := filepath.Join("..", "..", "..", "skel", "etc", "grafana", "dashboards")
	entries, err := os.ReadDir(dashDir)
	if err != nil {
		t.Fatalf("read dashboards dir: %v", err)
	}

	grantSQLPath := filepath.Join("..", "..", "..", "skel", "etc", "grafana", "grafana-readonly-user.sql")
	b, err := os.ReadFile(grantSQLPath)
	if err != nil {
		t.Fatalf("read %s: %v", grantSQLPath, err)
	}
	granted := map[string]bool{}
	for _, m := range grantTableRe.FindAllStringSubmatch(string(b), -1) {
		granted[strings.ToLower(m[1])] = true
	}
	if len(granted) == 0 {
		t.Fatal("no GRANT SELECT statements found in grafana-readonly-user.sql — regex or file drifted")
	}
	// cost_facts is a VIEW (v29 migration), not a base table, but MySQL
	// grants SELECT on a view with the exact same syntax as a table, so it's
	// checked identically here — no special-casing needed.

	var dashboardFiles int
	var totalRefs int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		dashboardFiles++
		path := filepath.Join(dashDir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var d dashboardJSON
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for tbl, panelSet := range dashboardTableRefs(d) {
			totalRefs++
			if granted[tbl] {
				continue
			}
			panels := make([]string, 0, len(panelSet))
			for p := range panelSet {
				panels = append(panels, p)
			}
			sort.Strings(panels)
			t.Errorf("%s: panel(s) %v query table %q via FROM/JOIN, but grafana-readonly-user.sql "+
				"grants no SELECT on it — grafana_ro's query will fail and the panel will silently "+
				"render empty in production. Add a GRANT SELECT line for %q to grafana-readonly-user.sql.",
				e.Name(), panels, tbl, tbl)
		}
	}
	if dashboardFiles == 0 {
		t.Fatal("no dashboard JSON files found — dashboards dir or glob drifted")
	}
	if totalRefs == 0 {
		t.Fatal("no FROM/JOIN table references found across any dashboard — extraction regex or dashboard corpus drifted")
	}
}

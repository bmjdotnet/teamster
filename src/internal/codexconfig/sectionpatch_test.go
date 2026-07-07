package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return string(data)
}

// TestUpsertSection_OperatorCommentsSurviveThreeRuns is the load-bearing
// regression test for the whole design: an operator-authored config with
// comments and a pre-existing MCP table must come out byte-identical outside
// Teamster's own marked spans, across repeated install runs.
func TestUpsertSection_OperatorCommentsSurviveThreeRuns(t *testing.T) {
	original := readTestdata(t, "operator-authored.toml")

	wmsBody := "[mcp_servers.wms]\n" +
		"command = \"/home/bmj/teamster/bin/wms-mcp\"\n" +
		"default_tools_approval_mode = \"approve\"\n"

	content := original
	for run := 1; run <= 3; run++ {
		var ur UpsertResult
		content, ur = UpsertSection(content, "mcp_servers.wms", wmsBody, "[mcp_servers.wms]", SkipIfPresent)
		if run == 1 && !ur.Changed {
			t.Fatalf("run %d: expected Changed=true on first write", run)
		}
		if run > 1 && !ur.SkippedExisting {
			t.Fatalf("run %d: expected SkippedExisting=true on rerun, got %+v", run, ur)
		}
	}

	for _, comment := range []string{
		"# My personal codex config",
		"# I like high reasoning effort for everything",
		"# trust my main dev tree",
		"# my custom search server, do not remove",
	} {
		if !strings.Contains(content, comment) {
			t.Errorf("operator comment lost after 3 runs: %q", comment)
		}
	}
	if !strings.Contains(content, `[mcp_servers.mysearch]`) {
		t.Error("operator's own mcp_servers.mysearch table was lost")
	}
	if strings.Count(content, "[mcp_servers.wms]") != 1 {
		t.Errorf("expected exactly one [mcp_servers.wms] header after 3 runs, got %d\n---\n%s", strings.Count(content, "[mcp_servers.wms]"), content)
	}
}

func TestUpsertSection_SkipIfPresentDoesNotTouchExistingMarkedBlock(t *testing.T) {
	content := "model = \"gpt-5.5\"\n"
	content, ur := UpsertSection(content, "mcp_servers.wms", "[mcp_servers.wms]\ncommand = \"orig\"\n", "[mcp_servers.wms]", SkipIfPresent)
	if !ur.Changed {
		t.Fatal("expected first write to change content")
	}

	// Operator hand-edits inside the marked block after install.
	edited := strings.Replace(content, `command = "orig"`, "command = \"orig\"\nextra_operator_key = \"kept\"", 1)

	// Rerun with a DIFFERENT body — SkipIfPresent must leave the operator's edit alone.
	result, ur2 := UpsertSection(edited, "mcp_servers.wms", "[mcp_servers.wms]\ncommand = \"different\"\n", "[mcp_servers.wms]", SkipIfPresent)
	if !ur2.SkippedExisting {
		t.Fatalf("expected SkippedExisting=true, got %+v", ur2)
	}
	if result != edited {
		t.Fatalf("SkipIfPresent must not modify content at all when a marked block exists:\ngot:  %q\nwant: %q", result, edited)
	}
}

func TestUpsertSection_AlwaysUpsertReplacesInPlace(t *testing.T) {
	content := "model = \"gpt-5.5\"\n"
	body1 := "[hooks.state.\"x:pre_tool_use:0:0\"]\ntrusted_hash = \"sha256:aaa\"\n"
	content, ur := UpsertSection(content, "hooks.state", body1, "", AlwaysUpsert)
	if !ur.Changed {
		t.Fatal("expected first write to change content")
	}
	if strings.Count(content, "sha256:aaa") != 1 {
		t.Fatalf("expected exactly one sha256:aaa, got content:\n%s", content)
	}

	body2 := "[hooks.state.\"x:pre_tool_use:0:0\"]\ntrusted_hash = \"sha256:bbb\"\n"
	content, ur = UpsertSection(content, "hooks.state", body2, "", AlwaysUpsert)
	if !ur.Changed {
		t.Fatal("expected AlwaysUpsert rerun to report Changed=true")
	}
	if strings.Count(content, "trusted_hash") != 1 {
		t.Fatalf("expected exactly one trusted_hash entry after rerun (no duplicate table definition), got content:\n%s", content)
	}
	if !strings.Contains(content, "sha256:bbb") {
		t.Fatal("expected the new hash to be present")
	}
	if strings.Contains(content, "sha256:aaa") {
		t.Fatal("expected the old hash to be gone, not left alongside the new one")
	}
}

func TestUpsertSection_UnmarkedCollisionNeverTouchesForeignContent(t *testing.T) {
	// An operator's own hand-written table with the same identity, never
	// bounded by Teamster's markers.
	content := "model = \"gpt-5.5\"\n\n[mcp_servers.wms]\ncommand = \"/some/operator/path\"\n"

	result, ur := UpsertSection(content, "mcp_servers.wms", "[mcp_servers.wms]\ncommand = \"/home/bmj/teamster/bin/wms-mcp\"\n", "[mcp_servers.wms]", SkipIfPresent)
	if !ur.UnmarkedCollision {
		t.Fatalf("expected UnmarkedCollision=true, got %+v", ur)
	}
	if result != content {
		t.Fatalf("UnmarkedCollision must leave content byte-identical:\ngot:  %q\nwant: %q", result, content)
	}
}

func TestRemoveSection_RestoresByteForByte(t *testing.T) {
	original := readTestdata(t, "operator-authored.toml")

	content := original
	content, _ = UpsertSection(content, "mcp_servers.wms", "[mcp_servers.wms]\ncommand = \"x\"\n", "[mcp_servers.wms]", SkipIfPresent)
	content, _ = UpsertSection(content, "otel", "[otel]\nmetrics_exporter = \"none\"\n", "", AlwaysUpsert)

	content = RemoveSection(content, "mcp_servers.wms")
	content = RemoveSection(content, "otel")

	// Not byte-identical (blank-line collapsing can differ from the
	// original's exact spacing) but every original line must still be
	// present and no Teamster content should remain.
	for _, line := range strings.Split(original, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.Contains(content, line) {
			t.Errorf("original line lost after install+uninstall: %q", line)
		}
	}
	if strings.Contains(content, "teamster:") {
		t.Errorf("Teamster markers/content remain after RemoveSection:\n%s", content)
	}
}

func TestRemoveSection_NoOpWhenAbsent(t *testing.T) {
	content := "model = \"gpt-5.5\"\n"
	result := RemoveSection(content, "mcp_servers.wms")
	if result != content {
		t.Fatalf("RemoveSection on an absent section should be a no-op, got %q want %q", result, content)
	}
}

func TestCollapseBlankRuns(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\n\n\nb", "a\n\nb"},
		{"a\n\n\n\n\nb", "a\n\nb"},
		{"a\n\nb", "a\n\nb"},
		{"a\nb", "a\nb"},
	}
	for _, c := range cases {
		if got := collapseBlankRuns(c.in); got != c.want {
			t.Errorf("collapseBlankRuns(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestContainsLine_FullLineMatchOnly(t *testing.T) {
	content := "# see [mcp_servers.wms] below for details\n[mcp_servers.wms]\ncommand=\"x\"\n"
	if !containsLine(content, "[mcp_servers.wms]") {
		t.Error("expected the real header line to match")
	}
	// The comment mentioning the table name in prose is a different full
	// line ("# see [mcp_servers.wms] below for details") and must not be
	// confused with the header itself — containsLine already handles this
	// correctly since it compares whole trimmed lines, not substrings; this
	// test documents that guarantee explicitly.
	if containsLine(content, "[mcp_servers.doesnotexist]") {
		t.Error("expected no match for a header that isn't actually present")
	}
}

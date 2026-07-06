package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

func TestParseSearchTypes(t *testing.T) {
	cases := []struct {
		raw     string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{"all", []string{"all"}, false},
		{"outcomes,workunits", []string{"outcomes", "workunits"}, false},
		{" focus , outcomes ", []string{"focus", "outcomes"}, false},
		{"bogus", nil, true},
		{"outcomes,bogus", nil, true},
	}
	for _, c := range cases {
		got, err := parseSearchTypes(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSearchTypes(%q): want error, got nil", c.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSearchTypes(%q): unexpected error: %v", c.raw, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseSearchTypes(%q) = %v, want %v", c.raw, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseSearchTypes(%q) = %v, want %v", c.raw, got, c.want)
				break
			}
		}
	}
}

// TestRunSearchSessionsRequiresQueryOrTag exercises the guard at
// search.go:90-94, which runs before openTagsDB() — so this stays a no-DB
// unit test even though it drives runSearchSessions end to end.
func TestRunSearchSessionsRequiresQueryOrTag(t *testing.T) {
	if got := runSearchSessions(nil); got != 2 {
		t.Errorf("runSearchSessions(nil) = %d, want 2 (usage error for missing query/--tag)", got)
	}
	if got := runSearchSessions([]string{"--user", "bj"}); got != 2 {
		t.Errorf("runSearchSessions(--user only) = %d, want 2 (still no query or --tag)", got)
	}
}

func TestTagFilterFlagRepeatable(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var tags tagFilterFlag
	fs.Var(&tags, "tag", "")
	if err := fs.Parse([]string{"--tag", "research=gastown", "--tag", "role=lead"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"research=gastown", "role=lead"}
	if len(tags) != len(want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Errorf("tags[%d] = %q, want %q", i, tags[i], want[i])
		}
	}
}

func TestRenderMatched(t *testing.T) {
	cases := []struct {
		name string
		refs []wms.EntityRef
		want string
	}{
		{
			name: "single entity, no overflow",
			refs: []wms.EntityRef{{EntityType: "outcome", EntityID: "gastown-integration"}},
			want: "outcome:gastown-integration",
		},
		{
			name: "multiple entities collapse to overflow",
			refs: []wms.EntityRef{
				{EntityType: "outcome", EntityID: "gastown-integration"},
				{EntityType: "workunit", EntityID: "gs-events-costagg"},
				{EntityType: "workunit", EntityID: "gs-events-dedup"},
			},
			want: "outcome:gastown-integration (+2)",
		},
		{
			name: "focus ref shows focus text, not focus:<empty-id>",
			refs: []wms.EntityRef{{EntityType: "focus", EntityID: "", Why: "focus:teamster search sessions CLI"}},
			want: "teamster search sessions CLI",
		},
		{
			name: "empty",
			refs: nil,
			want: "",
		},
	}
	for _, c := range cases {
		if got := renderMatched(c.refs); got != c.want {
			t.Errorf("%s: renderMatched() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWriteSessionsTableNeverTruncatesSession(t *testing.T) {
	longSession := "45cc474f-f08d-4988-b60c-d3f33e9d3bab-with-a-very-long-suffix-appended-for-good-measure"
	sessions := []wms.SessionMatch{
		{
			User:      "bj",
			Host:      "hub01",
			SessionID: longSession,
			Status:    "active",
			LastSeen:  time.Now().Add(-12 * time.Hour),
			Matched: []wms.EntityRef{
				{EntityType: "outcome", EntityID: "gastown-integration"},
				{EntityType: "workunit", EntityID: "gs-events-costagg"},
				{EntityType: "workunit", EntityID: "gs-events-dedup"},
			},
		},
		{
			User:      "bj",
			Host:      "studio",
			SessionID: "892e1187-6361-4a2e-9f0c-1b2c3d4e5f60",
			Status:    "active",
			LastSeen:  time.Now().Add(-3 * 24 * time.Hour),
			Matched: []wms.EntityRef{
				{EntityType: "focus", EntityID: "", Why: "focus:teamster search sessions CLI"},
			},
		},
	}

	var buf bytes.Buffer
	writeSessionsTable(&buf, sessions)
	out := buf.String()

	if !strings.Contains(out, longSession) {
		t.Errorf("output does not contain full session id %q:\n%s", longSession, out)
	}
	if !strings.Contains(out, "outcome:gastown-integration (+2)") {
		t.Errorf("output missing expected MATCHED overflow rendering:\n%s", out)
	}
	if !strings.Contains(out, "teamster search sessions CLI") {
		t.Errorf("output missing focus-text rendering:\n%s", out)
	}
	if !strings.Contains(out, "2 sessions") || !strings.Contains(out, "2 hosts") {
		t.Errorf("output missing footer counts:\n%s", out)
	}
}

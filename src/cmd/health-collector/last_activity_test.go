package main

import (
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
)

func TestChooseLastActivity_NoExisting_UsesLedgerTs(t *testing.T) {
	ledgerTs := time.Now().Add(-5 * time.Second)
	ts, tool, display := chooseLastActivity(gauge.GaugeRow{}, false, ledgerTs)
	if !ts.Equal(ledgerTs) {
		t.Fatalf("ts = %v, want %v", ts, ledgerTs)
	}
	if tool != "" || display != "" {
		t.Fatalf("tool/display = %q/%q, want empty (nothing to preserve)", tool, display)
	}
}

func TestChooseLastActivity_PreservesHookdWrittenToolAndDisplay(t *testing.T) {
	// hookd's UpdateActivity wrote tool/display moments before this tick's
	// Upsert — the collector must carry them forward, not zero them out.
	hookdTs := time.Now().Add(-2 * time.Second)
	existing := gauge.GaugeRow{
		LastActivityTs:      &hookdTs,
		LastActivityTool:    "READ",
		LastActivityDisplay: "reading __foo.go__",
	}
	ledgerTs := time.Now().Add(-10 * time.Second) // older than hookd's write

	ts, tool, display := chooseLastActivity(existing, true, ledgerTs)
	if tool != "READ" || display != "reading __foo.go__" {
		t.Fatalf("tool/display = %q/%q, want preserved READ/reading __foo.go__", tool, display)
	}
	if !ts.Equal(hookdTs) {
		t.Fatalf("ts = %v, want the newer hookd ts %v", ts, hookdTs)
	}
}

func TestChooseLastActivity_LedgerNewerThanExisting_TsWinsButToolPreserved(t *testing.T) {
	existingTs := time.Now().Add(-1 * time.Hour)
	existing := gauge.GaugeRow{
		LastActivityTs:      &existingTs,
		LastActivityTool:    "TASK",
		LastActivityDisplay: "stale activity",
	}
	ledgerTs := time.Now()

	ts, tool, display := chooseLastActivity(existing, true, ledgerTs)
	if !ts.Equal(ledgerTs) {
		t.Fatalf("ts = %v, want the newer ledger ts %v", ts, ledgerTs)
	}
	// Tool/display are still carried forward — this collector never has a
	// fresher signal for them than what hookd already wrote.
	if tool != "TASK" || display != "stale activity" {
		t.Fatalf("tool/display = %q/%q, want preserved TASK/stale activity", tool, display)
	}
}

func TestChooseLastActivity_ExistingFoundButNoActivityYet_ZeroValue(t *testing.T) {
	ledgerTs := time.Now()
	ts, tool, display := chooseLastActivity(gauge.GaugeRow{}, true, ledgerTs)
	if !ts.Equal(ledgerTs) {
		t.Fatalf("ts = %v, want ledger ts %v", ts, ledgerTs)
	}
	if tool != "" || display != "" {
		t.Fatalf("tool/display = %q/%q, want empty", tool, display)
	}
}

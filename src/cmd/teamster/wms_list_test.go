package main

import (
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// TestFilterOutcomes_DefaultViewHidesTerminal is a regression test for WP9's
// terminal-status fix: the default (status=="") view must hide *both* done
// and abandoned outcomes, not just done. Before the fix, an abandoned
// outcome fell through the == wms.StatusDone check and stayed visible in
// the default `teamster wms list` output forever.
func TestFilterOutcomes_DefaultViewHidesTerminal(t *testing.T) {
	outcomes := []*wms.Outcome{
		{ID: "oc-1", Status: wms.StatusActive},
		{ID: "oc-2", Status: wms.StatusDone},
		{ID: "oc-3", Status: wms.StatusAbandoned},
		{ID: "oc-4", Status: wms.StatusOnHold},
	}

	got := filterOutcomes(outcomes, "", time.Time{})

	var ids []string
	for _, o := range got {
		ids = append(ids, o.ID)
	}
	if len(ids) != 2 || ids[0] != "oc-1" || ids[1] != "oc-4" {
		t.Errorf("default view = %v, want [oc-1 oc-4] (active and on_hold shown, done and abandoned hidden)", ids)
	}
}

func TestFilterOutcomes_ExplicitAbandonedStatusStillMatches(t *testing.T) {
	outcomes := []*wms.Outcome{
		{ID: "oc-1", Status: wms.StatusAbandoned},
		{ID: "oc-2", Status: wms.StatusDone},
	}

	got := filterOutcomes(outcomes, wms.StatusAbandoned, time.Time{})

	if len(got) != 1 || got[0].ID != "oc-1" {
		t.Errorf("explicit --status=abandoned filter = %v, want [oc-1]", got)
	}
}

func TestFilterWorkUnits_DefaultViewHidesTerminal(t *testing.T) {
	wus := []*wms.WorkUnit{
		{ID: "wu-1", Status: wms.StatusActive},
		{ID: "wu-2", Status: wms.StatusDone},
		{ID: "wu-3", Status: wms.StatusAbandoned},
	}

	got := filterWorkUnits(wus, "", time.Time{})

	if len(got) != 1 || got[0].ID != "wu-1" {
		t.Errorf("default view = %v, want [wu-1]", got)
	}
}

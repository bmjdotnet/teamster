package wms

import (
	"context"
	"testing"
)

// TestSetFocus_EntityTypeCaseInsensitive: an agent naturally capitalizes
// "WorkUnit"/"Outcome" the way the Go type names read; wms_setFocus used to
// reject anything but the lowercase store constants ("workunit"/"outcome").
func TestSetFocus_EntityTypeCaseInsensitive(t *testing.T) {
	store, oid := newStewardStore(t)
	const wuID = "wu-setfocus-case"
	if _, ce := call(t, store, ToolCreateWorkUnit, map[string]interface{}{"id": wuID, "title": wuID, "outcomeID": oid}); ce != nil {
		t.Fatalf("createWorkUnit: %v", ce)
	}

	if _, ce := call(t, store, ToolSetFocus, map[string]interface{}{
		"entityType": "WorkUnit", "entityID": wuID, "focus": "case-insensitive workunit",
	}); ce != nil {
		t.Fatalf("setFocus with entityType=WorkUnit: %v", ce)
	}
	wu, err := store.GetWorkUnit(context.Background(), wuID)
	if err != nil {
		t.Fatalf("GetWorkUnit: %v", err)
	}
	if wu.Focus != "case-insensitive workunit" {
		t.Errorf("workunit focus = %q, want %q", wu.Focus, "case-insensitive workunit")
	}

	if _, ce := call(t, store, ToolSetFocus, map[string]interface{}{
		"entityType": "Outcome", "entityID": oid, "focus": "case-insensitive outcome",
	}); ce != nil {
		t.Fatalf("setFocus with entityType=Outcome: %v", ce)
	}
	o, err := store.GetOutcome(context.Background(), oid)
	if err != nil {
		t.Fatalf("GetOutcome: %v", err)
	}
	if o.Focus != "case-insensitive outcome" {
		t.Errorf("outcome focus = %q, want %q", o.Focus, "case-insensitive outcome")
	}

	if _, ce := call(t, store, ToolSetFocus, map[string]interface{}{
		"entityType": "widget", "entityID": oid, "focus": "bad type",
	}); ce == nil {
		t.Fatal("setFocus with an invalid entityType returned no error")
	}
}

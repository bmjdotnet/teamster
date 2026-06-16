package main

import (
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/config"
)

// statusFakeDSN is a NON-real credential. The password must never appear in any
// rendered status column. Never put a live secret in a test fixture.
const (
	statusFakeDSN      = "mysql://teamster:FAKESTATUSPW@db.example.internal:3306/teamster"
	statusFakePassword = "FAKESTATUSPW"
)

// TestStatusRowsNeverRenderPassword proves `teamster status` shows the store as
// host:port only — the full DSN (which carries the password) must never reach a
// status column. Guards the NB-1 leak: status.go used to put cfg.StoreDSN.Primary
// straight into the endpoint column.
func TestStatusRowsNeverRenderPassword(t *testing.T) {
	cfg := config.Config{
		StoreDSN: config.StoreDSN{
			Driver:  config.StoreDriverMySQL,
			Primary: statusFakeDSN,
		},
		StoreMode: "managed",
	}

	rows := buildStatusRows(cfg)

	var storeRow *statusRow
	for i := range rows {
		if strings.Contains(rows[i].label, "WMS Store") {
			storeRow = &rows[i]
		}
		// No rendered column of any row may carry the password or the full DSN.
		for col, v := range map[string]string{
			"label":    rows[i].label,
			"status":   rows[i].status,
			"mode":     rows[i].mode,
			"endpoint": rows[i].endpoint,
		} {
			if strings.Contains(v, statusFakePassword) {
				t.Fatalf("row %q col %q leaks the password: %q", rows[i].label, col, v)
			}
			if strings.Contains(v, statusFakeDSN) {
				t.Fatalf("row %q col %q renders the full DSN: %q", rows[i].label, col, v)
			}
		}
	}

	if storeRow == nil {
		t.Fatal("no WMS Store row rendered")
	}
	// Positive assertion: the store endpoint is the host[:port] the helper returns.
	if want := "db.example.internal:3306"; storeRow.endpoint != want {
		t.Fatalf("store endpoint = %q, want %q (host:port only)", storeRow.endpoint, want)
	}
}

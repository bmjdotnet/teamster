// Package observability provides the Prometheus metric surface for the
// teamster class: the bridge gauge, WMS entity counts, and all supporting
// counters / histograms listed in SPEC §7.1.
package observability

import "fmt"

// LabelBundle carries the canonical label set for ALL teamster_* metric
// emission. It maps 1:1 to the bridge gauge label set. Focus is intentionally
// absent (stored separately; see SPEC §4.2). SessionID + AgentName are jointly
// required (both must be set for a valid bundle; "" is a valid AgentName
// representing the lead, and is its own distinct bucket). Non-empty AgentName
// values always carry the "@" prefix.
type LabelBundle struct {
	SessionID  string // required
	Host       string // required
	AgentName  string // required (may be "" for lead, "@<name>" for teammate)
	TeamName   string // optional, "" before TeamCreate fires
	OutcomeID  string // optional WMS context (v3 entity)
	WorkunitID string
}

// Validate returns an error if required fields are missing. AgentName is
// required-as-a-field but accepts empty string for the lead bucket.
func (b LabelBundle) Validate() error {
	if b.SessionID == "" {
		return fmt.Errorf("LabelBundle: SessionID is required")
	}
	if b.Host == "" {
		return fmt.Errorf("LabelBundle: Host is required")
	}
	return nil
}

// bridgeGaugeLabelNames is the canonical ordered label set for
// teamster_session_active. Every field in LabelBundle that is emitted as a
// label must appear here (tooth 2: compile-time check via labels_test.go).
var bridgeGaugeLabelNames = []string{
	"session_id",
	"host",
	"team_name",
	"agent_name",
	"outcome_id",
	"workunit_id",
}

// bridgeGaugeLabelValues returns label values in the same order as
// bridgeGaugeLabelNames.
func bridgeGaugeLabelValues(b LabelBundle) []string {
	return []string{
		b.SessionID,
		b.Host,
		b.TeamName,
		b.AgentName,
		b.OutcomeID,
		b.WorkunitID,
	}
}

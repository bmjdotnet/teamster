package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

var bridgeDesc = prometheus.NewDesc(
	"teamster_session_active",
	"Active session gauge — always 1 per live (session_id, agent_name) pair. "+
		"Join with claude_code_* metrics on session_id for blended cost-per-WMS queries.",
	bridgeGaugeLabelNames,
	nil,
)

// BridgeCollector is a custom prometheus.Collector that emits
// teamster_session_active for every live (session_id, agent_name) pair.
// Snapshot-under-lock-then-emit-without-lock per SPEC §4.4.
type BridgeCollector struct {
	tracker *SessionTracker
}

// NewBridgeCollector returns a Collector backed by tracker.
func NewBridgeCollector(tracker *SessionTracker) *BridgeCollector {
	return &BridgeCollector{tracker: tracker}
}

// Describe sends the descriptor to ch.
func (c *BridgeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- bridgeDesc
}

// Collect takes a snapshot (holding only the RLock for the copy) then emits
// one gauge series per active pair outside the lock so a slow Prom scrape
// never blocks sweep/write operations.
func (c *BridgeCollector) Collect(ch chan<- prometheus.Metric) {
	snapshots := c.tracker.Snapshot()
	for _, s := range snapshots {
		bundle := LabelBundle{
			SessionID:  s.SessionID,
			Host:       s.Host,
			TeamName:   s.TeamName,
			AgentName:  s.AgentName,
			OutcomeID:  s.OutcomeID,
			WorkunitID: s.WorkunitID,
		}
		ch <- prometheus.MustNewConstMetric(
			bridgeDesc,
			prometheus.GaugeValue,
			1,
			bridgeGaugeLabelValues(bundle)...,
		)
	}
}

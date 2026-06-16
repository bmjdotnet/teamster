package observability

import "github.com/prometheus/client_golang/prometheus"

var entitiesDesc = prometheus.NewDesc(
	"teamster_wms_entities",
	"Current count of WMS entities by type and status. Eager counter: "+
		"hydrated at hookd startup from the store, then updated live by both "+
		"the in-process observer (Path 1) and WMSStatusChange events (Path 2).",
	[]string{"entity_type", "status"},
	nil,
)

// EntitiesCollector is a custom prometheus.Collector for teamster_wms_entities.
// It reads the package-level entityCounts map via snapshotCounts.
type EntitiesCollector struct{}

// NewEntitiesCollector returns a Collector for the WMS entity gauge.
func NewEntitiesCollector() *EntitiesCollector {
	return &EntitiesCollector{}
}

// Describe sends the descriptor to ch.
func (c *EntitiesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- entitiesDesc
}

// Collect snapshots entity counts and emits one gauge series per (type, status)
// pair that has a non-zero count.
func (c *EntitiesCollector) Collect(ch chan<- prometheus.Metric) {
	snap := snapshotCounts()
	for k, v := range snap {
		ch <- prometheus.MustNewConstMetric(
			entitiesDesc,
			prometheus.GaugeValue,
			float64(v),
			k.EntityType,
			k.Status,
		)
	}
}

package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// WP1 §6 (R7): build_info is a second, independent-in-transport disclosure
// channel alongside /health — assert it's registered, always 1, and carries
// all three build fields as labels (not just commit).
func TestBuildInfoMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.BuildInfo.WithLabelValues("v0.2.6", "c52f51c", "2026-08-14T02:37:57Z").Set(1)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	var found *dto.MetricFamily
	for _, mf := range mfs {
		if mf.GetName() == "teamster_build_info" {
			found = mf
			break
		}
	}
	if found == nil {
		t.Fatal("teamster_build_info not registered")
	}
	if len(found.Metric) != 1 {
		t.Fatalf("expected exactly one build_info series, got %d", len(found.Metric))
	}
	metric := found.Metric[0]
	if got := metric.GetGauge().GetValue(); got != 1 {
		t.Errorf("value = %v, want 1", got)
	}
	labels := map[string]string{}
	for _, lp := range metric.Label {
		labels[lp.GetName()] = lp.GetValue()
	}
	want := map[string]string{
		"version":    "v0.2.6",
		"commit":     "c52f51c",
		"build_time": "2026-08-14T02:37:57Z",
	}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("label %q = %q, want %q", k, labels[k], v)
		}
	}
}

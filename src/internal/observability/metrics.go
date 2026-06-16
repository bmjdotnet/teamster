package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds all teamster_* Prometheus collectors. It is separate from
// prometheus.DefaultRegisterer so hookd does not pollute the process-global
// registry (important for tests running multiple instances).
var Registry = prometheus.NewRegistry()

// Metrics holds the standard Vec metrics for §7.1 (non-custom-collector types).
type Metrics struct {
	HookEventsTotal       *prometheus.CounterVec
	ToolCallsTotal        *prometheus.CounterVec
	ToolCallDuration      *prometheus.HistogramVec
	ActivityCallsTotal    *prometheus.CounterVec
	WMSStatusChangesTotal *prometheus.CounterVec
	SessionsTotal         *prometheus.CounterVec
	EventWriteErrorsTotal *prometheus.CounterVec
	SSESubscribers        prometheus.Gauge
	StoreQueryDuration    *prometheus.HistogramVec
	StoreDualWriteErrors  *prometheus.CounterVec
	ActiveSessionsPruned  *prometheus.CounterVec
}

// NewMetrics registers all standard Vec metrics on reg and returns them.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		HookEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teamster_hook_events_total",
			Help: "Total hook events received, by type.",
		}, []string{"event", "host", "agent_name"}),

		ToolCallsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teamster_tool_calls_total",
			Help: "Total tool calls observed.",
		}, []string{"tool", "host", "agent_name", "status"}),

		ToolCallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "teamster_tool_call_duration_seconds",
			Help:    "Tool call latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"tool", "host", "agent_name", "status"}),

		ActivityCallsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teamster_activity_calls_total",
			Help: "Total activity MCP calls (reportActivity/setOverallIntent/completeActivity).",
		}, []string{"method", "host", "agent_name"}),

		WMSStatusChangesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teamster_wms_status_changes_total",
			Help: "Total WMS entity status transitions.",
		}, []string{"entity_type", "old_status", "new_status"}),

		SessionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teamster_sessions_total",
			Help: "Total sessions seen by hookd.",
		}, []string{"host"}),

		EventWriteErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teamster_event_write_errors_total",
			Help: "Event write failures by reason.",
		}, []string{"reason"}),

		SSESubscribers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "teamster_sse_subscribers",
			Help: "Current number of active SSE subscriber connections.",
		}),

		StoreQueryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "teamster_store_query_duration_seconds",
			Help:    "Store query latency by kind and backend.",
			Buckets: prometheus.DefBuckets,
		}, []string{"query_kind", "backend"}),

		StoreDualWriteErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teamster_store_dual_write_errors_total",
			Help: "Dual-write secondary failures by backend and operation.",
		}, []string{"backend", "op"}),

		ActiveSessionsPruned: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "teamster_active_sessions_pruned_total",
			Help: "Sessions pruned from the active tracker by reason.",
		}, []string{"reason"}),
	}

	reg.MustRegister(
		m.HookEventsTotal,
		m.ToolCallsTotal,
		m.ToolCallDuration,
		m.ActivityCallsTotal,
		m.WMSStatusChangesTotal,
		m.SessionsTotal,
		m.EventWriteErrorsTotal,
		m.SSESubscribers,
		m.StoreQueryDuration,
		m.StoreDualWriteErrors,
		m.ActiveSessionsPruned,
	)
	return m
}

// Handler returns an http.Handler that serves the /metrics endpoint for reg.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: false,
	})
}

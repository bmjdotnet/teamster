package observability

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// SweepState is written by the rollup --sweep binary and read by this collector.
type SweepState struct {
	LastRunTimestamp float64            `json:"last_run_timestamp"`
	DurationSeconds float64            `json:"duration_seconds"`
	RecoveredTotal  map[string]float64 `json:"recovered_total"`
}

var (
	sweepLastRunDesc = prometheus.NewDesc(
		"teamster_sweep_last_run_timestamp",
		"Unix timestamp of the last sweep completion.",
		nil, nil,
	)
	sweepDurationDesc = prometheus.NewDesc(
		"teamster_sweep_duration_seconds",
		"Duration of the last sweep run in seconds.",
		nil, nil,
	)
	sweepRecoveredDesc = prometheus.NewDesc(
		"teamster_sweep_recovered_total",
		"Messages recovered per method in the last sweep run.",
		[]string{"method"}, nil,
	)
)

// SweepCollector reads sweep metrics from a JSON state file written by the
// rollup --sweep oneshot binary. The file is re-read at most every 60s.
type SweepCollector struct {
	path string

	mu        sync.Mutex
	lastRead  time.Time
	cached    *SweepState
	haveCache bool
}

func NewSweepCollector(stateFilePath string) *SweepCollector {
	return &SweepCollector{path: stateFilePath}
}

func (c *SweepCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- sweepLastRunDesc
	ch <- sweepDurationDesc
	ch <- sweepRecoveredDesc
}

func (c *SweepCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.snapshot()
	if s == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(sweepLastRunDesc, prometheus.GaugeValue, s.LastRunTimestamp)
	ch <- prometheus.MustNewConstMetric(sweepDurationDesc, prometheus.GaugeValue, s.DurationSeconds)
	for method, count := range s.RecoveredTotal {
		ch <- prometheus.MustNewConstMetric(sweepRecoveredDesc, prometheus.CounterValue, count, method)
	}
}

func (c *SweepCollector) snapshot() *SweepState {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.haveCache && time.Since(c.lastRead) < 60*time.Second {
		return c.cached
	}

	data, err := os.ReadFile(c.path)
	if err != nil {
		return c.cached
	}
	var s SweepState
	if json.Unmarshal(data, &s) != nil {
		return c.cached
	}
	c.cached = &s
	c.lastRead = time.Now()
	c.haveCache = true
	return c.cached
}

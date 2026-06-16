package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PromOTel is an OTelSource backed by a Prometheus HTTP API. It sums the
// claude_code_cost_usage_USD_total counter by session_id — the authoritative
// per-session cost magnitude (covering main + subagent + auxiliary query_source).
type PromOTel struct {
	baseURL string
	client  *http.Client
}

// NewPromOTel builds a Prometheus-backed OTel source. baseURL is the Prometheus
// root (e.g. http://localhost:9190).
func NewPromOTel(baseURL string) *PromOTel {
	return &PromOTel{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// otelCostLookback is the range selector passed to max_over_time when querying
// per-session OTel cost. An OTel exporter dies with its process, so any session
// that ended >5 min ago produces no live series under an instant query. Using
// max_over_time over a generous window recovers the last good sample from the
// TSDB, covering sessions that ended hours or days ago. 30 days comfortably
// exceeds any realistic session length and typical Prometheus retention; if a
// series has genuinely aged out of retention, the session simply won't appear
// (the GREATEST guard in Reconcile then keeps the previously recorded value).
const otelCostLookback = "30d"

// promResponse is the subset of the Prometheus instant-query response we read.
// max_over_time(metric[range]) is still sent to /api/v1/query (instant endpoint)
// and returns a vector — same "value" field as a plain instant query.
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"` // [ <unix_ts float>, "<sample string>" ]
		} `json:"result"`
	} `json:"data"`
}

// SessionCosts returns total OTel cost per session_id. It uses
// max_over_time(counter[lookback]) rather than a plain instant query so that
// sessions whose OTel exporter has already exited (process died, series absent
// from the last scrape window) are still recovered from the TSDB. Each agent
// process is its own OTel resource exporting a cumulative counter with no
// within-series resets; max_over_time per series then summed by session_id is
// therefore correct. Sessions that have fully aged out of Prometheus retention
// do not appear; Reconcile preserves any previously recorded non-zero value for
// those sessions rather than overwriting with zero.
func (p *PromOTel) SessionCosts(ctx context.Context) (map[string]float64, error) {
	q := `sum by (session_id) (max_over_time(claude_code_cost_usage_USD_total[` + otelCostLookback + `]))`
	u := p.baseURL + "/api/v1/query?" + url.Values{"query": {q}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus query returned %d", resp.StatusCode)
	}

	var pr promResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("prometheus query status %q", pr.Status)
	}

	out := make(map[string]float64, len(pr.Data.Result))
	for _, r := range pr.Data.Result {
		sid := r.Metric["session_id"]
		if sid == "" {
			continue
		}
		s, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		out[sid] = v
	}
	return out, nil
}

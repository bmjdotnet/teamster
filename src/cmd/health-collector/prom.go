package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// promClient queries Prometheus's HTTP API for per-agent tool call counts.
// Local to this binary, not a new internal package — mirrors the response
// shape of internal/rollup.PromOTel (a different metric/consumer) rather
// than sharing code with it, since there's no second consumer yet.
type promClient struct {
	baseURL string
	http    *http.Client
}

func newPromClient(baseURL string) *promClient {
	return &promClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// promQueryResponse is the subset of a Prometheus instant-query response
// (GET /api/v1/query) this client reads.
type promQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"` // [ <unix_ts float>, "<sample string>" ]
		} `json:"result"`
	} `json:"data"`
}

// ToolCounts queries teamster_tool_calls_total (internal/observability/metrics.go)
// for the given host and returns map[agent_name]map[tool]count. The metric
// carries labels {tool, host, agent_name, status} — no session_id, so this
// is a per-host, per-agent aggregate rather than per-session; agent_name is
// unique enough within a host for the collector's purposes (one live
// session per agent_name in normal operation).
func (c *promClient) ToolCounts(ctx context.Context, host string) (map[string]map[string]int64, error) {
	q := fmt.Sprintf(`sum by (tool, agent_name) (teamster_tool_calls_total{host=%q})`, host)
	u := c.baseURL + "/api/v1/query?" + url.Values{"query": {q}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus query returned %d", resp.StatusCode)
	}

	var pr promQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("prometheus query status %q", pr.Status)
	}

	out := make(map[string]map[string]int64)
	for _, r := range pr.Data.Result {
		tool := r.Metric["tool"]
		if tool == "" {
			continue
		}
		agent := r.Metric["agent_name"]
		s, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		if out[agent] == nil {
			out[agent] = make(map[string]int64)
		}
		out[agent][tool] = int64(v)
	}
	return out, nil
}

package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	metricCost   = "claude_code_cost_usage_USD_total"
	metricTokens = "claude_code_token_usage_tokens_total"

	defaultLookback  = 7 * 24 * time.Hour
	defaultStep      = time.Minute
	defaultBatchSize = 50
	stabilitySlack   = 5 * time.Minute
	// Prometheus rejects range queries above 11,000 points per series.
	maxRangePoints = 5000
)

var safeSessionID = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// PromHTTP is a PromReader over the Prometheus HTTP API.
//
// Cost and token totals use last_over_time over Lookback rather than a plain
// instant read, so a session stays readable if the collector drops its series.
// Counter stability is read from a range query over the summed token counters
// spanning the stability horizon plus a slack: the session is stable since the
// first sample after the last value change, saturating at the scan start.
type PromHTTP struct {
	BaseURL   string
	Client    *http.Client
	Lookback  time.Duration
	Step      time.Duration
	BatchSize int
}

// NewPromHTTP returns a reader for the Prometheus at baseURL (e.g. http://hub:9090).
func NewPromHTTP(baseURL string) *PromHTTP {
	return &PromHTTP{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// ReadSessions implements PromReader. Session ids that cannot be matched
// safely in a PromQL selector are skipped, so they come back absent.
func (p *PromHTTP) ReadSessions(ctx context.Context, sessionIDs []string, at time.Time, stabilityHorizon time.Duration) (map[string]VendorSession, error) {
	ids := make([]string, 0, len(sessionIDs))
	for _, id := range uniqueSorted(sessionIDs) {
		if safeSessionID.MatchString(id) {
			ids = append(ids, regexp.QuoteMeta(id))
		}
	}
	out := map[string]VendorSession{}
	batch := p.BatchSize
	if batch <= 0 {
		batch = defaultBatchSize
	}
	for start := 0; start < len(ids); start += batch {
		end := min(start+batch, len(ids))
		if err := p.readBatch(ctx, ids[start:end], at, stabilityHorizon, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (p *PromHTTP) readBatch(ctx context.Context, ids []string, at time.Time, stabilityHorizon time.Duration, out map[string]VendorSession) error {
	lookback := p.Lookback
	if lookback <= 0 {
		lookback = defaultLookback
	}
	step := p.Step
	if step <= 0 {
		step = defaultStep
	}
	sel := fmt.Sprintf(`{session_id=~"%s"}`, strings.ReplaceAll(strings.Join(ids, "|"), `\`, `\\`))
	rng := fmt.Sprintf("[%ds]", int64(lookback/time.Second))

	costs, err := p.queryVector(ctx, at, fmt.Sprintf(`sum by (session_id, model) (last_over_time(%s%s%s))`, metricCost, sel, rng))
	if err != nil {
		return err
	}
	tokens, err := p.queryVector(ctx, at, fmt.Sprintf(`sum by (session_id, model, type) (last_over_time(%s%s%s))`, metricTokens, sel, rng))
	if err != nil {
		return err
	}
	span := stabilityHorizon + stabilitySlack
	if floor := time.Duration(math.Ceil(float64(span/time.Second)/maxRangePoints)) * time.Second; step < floor {
		step = floor
	}
	scanStart := at.Add(-span)
	series, err := p.queryMatrix(ctx, scanStart, at, step, fmt.Sprintf(`sum by (session_id) (%s%s)`, metricTokens, sel))
	if err != nil {
		return err
	}

	for _, s := range costs {
		sid, model := s.metric["session_id"], s.metric["model"]
		if sid == "" || model == "" {
			continue
		}
		vs := ensureVendor(out, sid)
		m := vs.Models[model]
		m.CostUSD += s.value
		vs.Models[model] = m
	}
	for _, s := range tokens {
		sid, model := s.metric["session_id"], s.metric["model"]
		if sid == "" || model == "" {
			continue
		}
		vs := ensureVendor(out, sid)
		m := vs.Models[model]
		n := int64(math.Round(s.value))
		switch s.metric["type"] {
		case "input":
			m.Tokens.Input += n
		case "output":
			m.Tokens.Output += n
		case "cacheRead":
			m.Tokens.CacheRead += n
		case "cacheCreation":
			m.Tokens.CacheWrite += n
		}
		vs.Models[model] = m
	}

	stable := map[string]time.Time{}
	for _, s := range series {
		if sid := s.metric["session_id"]; sid != "" {
			stable[sid] = stableSince(s.points)
		}
	}
	for sid, vs := range out {
		if _, seen := stable[sid]; seen {
			vs.TokensStableSince = stable[sid]
		} else if vs.TokensStableSince.IsZero() {
			// No samples in the scan range: the series ended before it began.
			vs.TokensStableSince = scanStart
		}
		out[sid] = vs
	}
	return nil
}

func ensureVendor(out map[string]VendorSession, sid string) VendorSession {
	vs, ok := out[sid]
	if !ok {
		vs = VendorSession{SessionID: sid, Models: map[string]VendorModel{}}
		out[sid] = vs
	}
	return vs
}

type point struct {
	t time.Time
	v float64
}

type promSeries struct {
	metric map[string]string
	value  float64
	points []point
}

type promEnvelope struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string    `json:"metric"`
			Value  [2]json.RawMessage   `json:"value"`
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (p *PromHTTP) queryVector(ctx context.Context, at time.Time, query string) ([]promSeries, error) {
	form := url.Values{"query": {query}, "time": {formatUnix(at)}}
	env, err := p.post(ctx, "/api/v1/query", form)
	if err != nil {
		return nil, err
	}
	if env.Data.ResultType != "vector" {
		return nil, fmt.Errorf("prometheus: expected vector result, got %q", env.Data.ResultType)
	}
	out := make([]promSeries, 0, len(env.Data.Result))
	for _, r := range env.Data.Result {
		_, v, err := parseSample(r.Value)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out = append(out, promSeries{metric: r.Metric, value: v})
	}
	return out, nil
}

func (p *PromHTTP) queryMatrix(ctx context.Context, start, end time.Time, step time.Duration, query string) ([]promSeries, error) {
	form := url.Values{
		"query": {query},
		"start": {formatUnix(start)},
		"end":   {formatUnix(end)},
		"step":  {strconv.FormatInt(int64(step/time.Second), 10)},
	}
	env, err := p.post(ctx, "/api/v1/query_range", form)
	if err != nil {
		return nil, err
	}
	if env.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("prometheus: expected matrix result, got %q", env.Data.ResultType)
	}
	out := make([]promSeries, 0, len(env.Data.Result))
	for _, r := range env.Data.Result {
		pts := make([]point, 0, len(r.Values))
		for _, raw := range r.Values {
			t, v, err := parseSample(raw)
			if err != nil {
				return nil, err
			}
			if math.IsNaN(v) {
				continue
			}
			pts = append(pts, point{t: t, v: v})
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].t.Before(pts[j].t) })
		out = append(out, promSeries{metric: r.Metric, points: pts})
	}
	return out, nil
}

func (p *PromHTTP) post(ctx context.Context, path string, form url.Values) (*promEnvelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("prometheus: %s: read body: %w", path, err)
	}
	var env promEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("prometheus: %s: HTTP %d: undecodable response: %w", path, resp.StatusCode, err)
	}
	if env.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s: HTTP %d: %s: %s", path, resp.StatusCode, env.ErrorType, env.Error)
	}
	return &env, nil
}

func parseSample(raw [2]json.RawMessage) (time.Time, float64, error) {
	var ts float64
	if err := json.Unmarshal(raw[0], &ts); err != nil {
		return time.Time{}, 0, fmt.Errorf("prometheus: bad sample timestamp %s: %w", raw[0], err)
	}
	var s string
	if err := json.Unmarshal(raw[1], &s); err != nil {
		return time.Time{}, 0, fmt.Errorf("prometheus: bad sample value %s: %w", raw[1], err)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("prometheus: bad sample value %q: %w", s, err)
	}
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC(), v, nil
}

func formatUnix(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64)
}

// stableSince returns the time of the first sample after the last value
// change, or of the first sample if the value never changed. Zero when there
// are no samples.
func stableSince(pts []point) time.Time {
	if len(pts) == 0 {
		return time.Time{}
	}
	final := pts[len(pts)-1].v
	since := pts[0].t
	for i := len(pts) - 2; i >= 0; i-- {
		if pts[i].v != final {
			since = pts[i+1].t
			break
		}
	}
	return since
}

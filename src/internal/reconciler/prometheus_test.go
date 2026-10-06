package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type promCall struct {
	path string
	form url.Values
}

type fakePromServer struct {
	*httptest.Server
	mu    sync.Mutex
	calls []promCall

	cost   string // vector result array
	tokens string
	matrix string
	status int
	body   string // overrides everything when set
}

func newFakePromServer(t *testing.T) *fakePromServer {
	t.Helper()
	f := &fakePromServer{cost: `[]`, tokens: `[]`, matrix: `[]`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		f.mu.Lock()
		f.calls = append(f.calls, promCall{path: r.URL.Path, form: r.PostForm})
		f.mu.Unlock()

		if f.body != "" {
			w.WriteHeader(f.status)
			fmt.Fprint(w, f.body)
			return
		}
		q := r.PostForm.Get("query")
		switch {
		case r.URL.Path == "/api/v1/query_range":
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":%s}}`, f.matrixFrom(t, r.PostForm.Get("start")))
		case strings.Contains(q, metricCost):
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":%s}}`, f.cost)
		case strings.Contains(q, metricTokens):
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":%s}}`, f.tokens)
		default:
			t.Errorf("unexpected query %q", q)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// matrixFrom returns the canned matrix with samples before start dropped, the
// way a real range query only returns what falls inside the requested range.
func (f *fakePromServer) matrixFrom(t *testing.T, start string) string {
	t.Helper()
	from, err := strconv.ParseFloat(start, 64)
	if err != nil {
		t.Errorf("bad start %q", start)
		return `[]`
	}
	var series []struct {
		Metric map[string]string `json:"metric"`
		Values [][2]any          `json:"values"`
	}
	if err := json.Unmarshal([]byte(f.matrix), &series); err != nil {
		t.Fatalf("bad canned matrix: %v", err)
	}
	kept := series[:0]
	for _, s := range series {
		var vals [][2]any
		for _, v := range s.Values {
			if v[0].(float64) >= from {
				vals = append(vals, v)
			}
		}
		if len(vals) > 0 {
			s.Values = vals
			kept = append(kept, s)
		}
	}
	out, _ := json.Marshal(kept)
	if len(kept) == 0 {
		return `[]`
	}
	return string(out)
}

func (f *fakePromServer) callsTo(path string) []promCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []promCall
	for _, c := range f.calls {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

func vec(session, model, typ, val string) string {
	typePart := ""
	if typ != "" {
		typePart = fmt.Sprintf(`,"type":%q`, typ)
	}
	return fmt.Sprintf(`{"metric":{"session_id":%q,"model":%q%s},"value":[1791063341.693,%q]}`, session, model, typePart, val)
}

func TestPromHTTPReadSessions(t *testing.T) {
	at := time.Unix(1791063341, 0).UTC()
	f := newFakePromServer(t)
	f.cost = "[" + strings.Join([]string{
		vec("s1", "claude-opus-4-6[1m]", "", "38.6337462"),
		vec("s1", "claude-haiku-4-5-20251001", "", "0.035926"),
		vec("s2", "claude-sonnet-5-5", "", "31.3745668"),
	}, ",") + "]"
	f.tokens = "[" + strings.Join([]string{
		vec("s1", "claude-opus-4-6[1m]", "input", "780"),
		vec("s1", "claude-opus-4-6[1m]", "output", "441004"),
		vec("s1", "claude-opus-4-6[1m]", "cacheRead", "91218156"),
		vec("s1", "claude-opus-4-6[1m]", "cacheCreation", "2313383"),
		vec("s1", "claude-opus-4-6[1m]", "somethingNew", "999"),
		vec("s1", "claude-haiku-4-5-20251001", "input", "20921"),
		vec("s2", "claude-sonnet-5-5", "output", "509196"),
	}, ",") + "]"
	// s1: flat for the whole range. s2: last change 10 minutes before `at`.
	f.matrix = fmt.Sprintf(`[
		{"metric":{"session_id":"s1"},"values":[[%d,"100"],[%d,"100"],[%d,"100"]]},
		{"metric":{"session_id":"s2"},"values":[[%d,"50"],[%d,"60"],[%d,"60"]]}]`,
		at.Add(-20*time.Minute).Unix(), at.Add(-10*time.Minute).Unix(), at.Unix(),
		at.Add(-20*time.Minute).Unix(), at.Add(-10*time.Minute).Unix(), at.Unix())

	p := NewPromHTTP(f.URL + "/")
	got, err := p.ReadSessions(context.Background(), []string{"s1", "s2", "s-gone"}, at, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("sessions = %d, want 2 (absent session must be omitted): %+v", len(got), got)
	}

	s1 := got["s1"]
	opus := s1.Models["claude-opus-4-6[1m]"]
	wantTokens := TokenCounts{Input: 780, Output: 441004, CacheRead: 91218156, CacheWrite: 2313383}
	if opus.Tokens != wantTokens || opus.CostUSD != 38.6337462 {
		t.Errorf("s1 opus = %+v, want tokens %+v cost 38.6337462 (unknown token type must be ignored)", opus, wantTokens)
	}
	if h := s1.Models["claude-haiku-4-5-20251001"]; h.Tokens.Input != 20921 || h.CostUSD != 0.035926 {
		t.Errorf("s1 haiku = %+v", h)
	}
	if want := at.Add(-20 * time.Minute); !s1.TokensStableSince.Equal(want) {
		t.Errorf("s1 stable since %v, want %v (flat across the range)", s1.TokensStableSince, want)
	}
	if want := at.Add(-10 * time.Minute); !got["s2"].TokensStableSince.Equal(want) {
		t.Errorf("s2 stable since %v, want %v", got["s2"].TokensStableSince, want)
	}
	if got["s2"].Models["claude-sonnet-5-5"].Tokens.Output != 509196 {
		t.Errorf("s2 = %+v", got["s2"])
	}

	q := f.callsTo("/api/v1/query")
	if len(q) != 2 || q[0].form.Get("time") != "1791063341.000" {
		t.Fatalf("instant calls = %+v", q)
	}
	for _, c := range q {
		query := c.form.Get("query")
		if !strings.Contains(query, `session_id=~"s-gone|s1|s2"`) || !strings.Contains(query, "last_over_time") || !strings.Contains(query, "[604800s]") {
			t.Errorf("instant query = %s", query)
		}
	}
	r := f.callsTo("/api/v1/query_range")
	if len(r) != 1 {
		t.Fatalf("range calls = %d, want 1", len(r))
	}
	if r[0].form.Get("step") != "60" || r[0].form.Get("end") != "1791063341.000" ||
		r[0].form.Get("start") != formatUnix(at.Add(-20*time.Minute)) {
		t.Errorf("range form = %v (scan must span window 15m + 5m slack)", r[0].form)
	}
}

func TestPromHTTPEndedSeriesStableSinceScanStart(t *testing.T) {
	at := time.Unix(1791063341, 0).UTC()
	f := newFakePromServer(t)
	f.cost = "[" + vec("s1", "claude-opus-4-6", "", "10") + "]"
	f.tokens = "[" + vec("s1", "claude-opus-4-6", "input", "5") + "]"

	got, err := NewPromHTTP(f.URL).ReadSessions(context.Background(), []string{"s1"}, at, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := at.Add(-20 * time.Minute); !got["s1"].TokensStableSince.Equal(want) {
		t.Errorf("stable since %v, want scan start %v", got["s1"].TokensStableSince, want)
	}
}

func TestPromHTTPScanSpansTheRequestedHorizon(t *testing.T) {
	at := time.Unix(1791063341, 0).UTC()
	tests := []struct {
		name     string
		horizon  time.Duration
		wantFrom time.Time
		wantStep string
	}{
		{"6h grace horizon", 6 * time.Hour, at.Add(-6*time.Hour - 5*time.Minute), "60"},
		{"30 days clamps the step under the 11k point limit", 30 * 24 * time.Hour, at.Add(-30*24*time.Hour - 5*time.Minute), "519"},
	}
	for _, tc := range tests {
		f := newFakePromServer(t)
		if _, err := NewPromHTTP(f.URL).ReadSessions(context.Background(), []string{"s1"}, at, tc.horizon); err != nil {
			t.Fatal(err)
		}
		r := f.callsTo("/api/v1/query_range")[0].form
		if r.Get("start") != formatUnix(tc.wantFrom) || r.Get("step") != tc.wantStep {
			t.Errorf("%s: start=%s step=%s, want start=%s step=%s", tc.name, r.Get("start"), r.Get("step"), formatUnix(tc.wantFrom), tc.wantStep)
		}
	}
}

func TestPromHTTPBatchesAndDedupes(t *testing.T) {
	f := newFakePromServer(t)
	ids := make([]string, 0, 121)
	for i := 0; i < 120; i++ {
		ids = append(ids, fmt.Sprintf("session-%03d", i))
	}
	ids = append(ids, "session-007")

	p := NewPromHTTP(f.URL)
	p.BatchSize = 50
	if _, err := p.ReadSessions(context.Background(), ids, time.Now(), 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsTo("/api/v1/query")); n != 6 {
		t.Errorf("instant calls = %d, want 6 (3 batches x cost+tokens)", n)
	}
	if n := len(f.callsTo("/api/v1/query_range")); n != 3 {
		t.Errorf("range calls = %d, want 3", n)
	}
	seen := 0
	for _, c := range f.callsTo("/api/v1/query_range") {
		seen += strings.Count(c.form.Get("query"), "session-")
	}
	if seen != 120 {
		t.Errorf("ids across batches = %d, want 120 (duplicate must collapse)", seen)
	}
}

func TestPromHTTPSelectorSafety(t *testing.T) {
	f := newFakePromServer(t)
	_, err := NewPromHTTP(f.URL).ReadSessions(context.Background(),
		[]string{`a.b`, `x"} or vector(1) #`, "ok-1", "has space", "line\nbreak"}, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	q := f.callsTo("/api/v1/query")[0].form.Get("query")
	if !strings.Contains(q, `session_id=~"a\\.b|ok-1"`) {
		t.Errorf("selector = %s, want only the safe ids with the dot escaped for PromQL", q)
	}
	for _, bad := range []string{"vector(1)", "has space", "break"} {
		if strings.Contains(q, bad) {
			t.Errorf("unsafe id leaked into query: %s", q)
		}
	}
}

func TestPromHTTPNoSafeIDsMakesNoRequests(t *testing.T) {
	f := newFakePromServer(t)
	got, err := NewPromHTTP(f.URL).ReadSessions(context.Background(), []string{`bad"id`}, time.Now(), time.Minute)
	if err != nil || len(got) != 0 || len(f.calls) != 0 {
		t.Errorf("got=%v err=%v calls=%d", got, err, len(f.calls))
	}
}

func TestPromHTTPSkipsNaN(t *testing.T) {
	f := newFakePromServer(t)
	f.cost = "[" + vec("s1", "claude-opus-4-6", "", "NaN") + "," + vec("s1", "claude-haiku-4-5", "", "+Inf") + "," + vec("s1", "claude-sonnet-5", "", "2.5") + "]"
	got, err := NewPromHTTP(f.URL).ReadSessions(context.Background(), []string{"s1"}, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got["s1"].Models) != 1 || got["s1"].Models["claude-sonnet-5"].CostUSD != 2.5 {
		t.Errorf("models = %+v, want only the finite sample", got["s1"].Models)
	}
}

func TestPromHTTPErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"prometheus error", 400, `{"status":"error","errorType":"bad_data","error":"parse error"}`, "bad_data: parse error"},
		{"not json", 502, `<html>bad gateway</html>`, "HTTP 502"},
		{"wrong result type", 200, `{"status":"success","data":{"resultType":"scalar","result":[]}}`, "expected vector"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakePromServer(t)
			f.status, f.body = tc.status, tc.body
			got, err := NewPromHTTP(f.URL).ReadSessions(context.Background(), []string{"s1"}, time.Now(), time.Minute)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != nil {
				t.Errorf("got=%v err=%v, want error containing %q", got, err, tc.wantErr)
			}
		})
	}

	t.Run("connection refused", func(t *testing.T) {
		f := newFakePromServer(t)
		url := f.URL
		f.Close()
		if _, err := NewPromHTTP(url).ReadSessions(context.Background(), []string{"s1"}, time.Now(), time.Minute); err == nil {
			t.Error("want error from unreachable server")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		f := newFakePromServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := NewPromHTTP(f.URL).ReadSessions(ctx, []string{"s1"}, time.Now(), time.Minute); err == nil {
			t.Error("want error from cancelled context")
		}
	})
}

func TestStableSince(t *testing.T) {
	ts := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	pts := func(vals ...float64) []point {
		out := make([]point, len(vals))
		for i, v := range vals {
			out[i] = point{t: ts(i), v: v}
		}
		return out
	}
	tests := []struct {
		name string
		pts  []point
		want time.Time
	}{
		{"empty", nil, time.Time{}},
		{"single sample", pts(5), ts(0)},
		{"flat", pts(5, 5, 5, 5), ts(0)},
		{"changed mid-range", pts(1, 2, 3, 3, 3), ts(2)},
		{"changed on the last step", pts(3, 3, 3, 4), ts(3)},
		{"decrease counts as a change", pts(9, 9, 4, 4), ts(2)},
		{"earlier change then flat", pts(1, 2, 2, 2), ts(1)},
	}
	for _, tc := range tests {
		if got := stableSince(tc.pts); !got.Equal(tc.want) {
			t.Errorf("%s: stableSince = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPromHTTPResultFeedsReconciler(t *testing.T) {
	// End to end through the real HTTP reader with a fake ledger: a session
	// whose counters stopped 10 minutes ago is held back, one stable for the
	// whole scan is judged.
	at := time.Unix(1791063341, 0).UTC()
	f := newFakePromServer(t)
	cost := func(s string) string { return vec(s, "claude-opus-4-8", "", "100") }
	f.cost = "[" + cost("live") + "," + cost("done") + "]"
	f.tokens = "[" + strings.Join([]string{
		vec("live", "claude-opus-4-8", "output", "600000"),
		vec("done", "claude-opus-4-8", "output", "600000"),
	}, ",") + "]"
	f.matrix = fmt.Sprintf(`[
		{"metric":{"session_id":"live"},"values":[[%d,"500000"],[%d,"600000"],[%d,"600000"]]},
		{"metric":{"session_id":"done"},"values":[[%d,"600000"],[%d,"600000"]]}]`,
		at.Add(-20*time.Minute).Unix(), at.Add(-10*time.Minute).Unix(), at.Unix(),
		at.Add(-20*time.Minute).Unix(), at.Unix())

	mk := func(id string) LedgerSession {
		return LedgerSession{SessionID: id, Rows: 5, LastRowAt: at.Add(-3 * time.Hour), Models: map[string]LedgerModel{
			"claude-opus-4-8": {Tokens: TokenCounts{Output: 600000}, CostUSD: 100},
		}}
	}
	ledger := &fakeLedger{sessions: map[string]LedgerSession{"live": mk("live"), "done": mk("done")}}
	r := New(NewPromHTTP(f.URL), ledger, nil, Config{})
	r.now = func() time.Time { return at }

	got, err := r.Reconcile(context.Background(), []string{"live", "done"})
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]Verdict{}
	for _, g := range got {
		verdicts[g.SessionID] = g.Verdict
	}
	if verdicts["live"] != VerdictUnconverged || verdicts["done"] != VerdictWithinTolerance {
		raw, _ := json.Marshal(got)
		t.Errorf("verdicts = %v\n%s", verdicts, raw)
	}
}

// Regression for a gap found against live Prometheus: a session quiet for many
// hours must reach the capture-gap grace through the real reader, which only
// works if the reader scans out to the grace rather than the convergence window.
func TestPromHTTPLongQuietSessionReachesCaptureGapGrace(t *testing.T) {
	at := time.Unix(1791063341, 0).UTC()
	f := newFakePromServer(t)
	f.cost = "[" + vec("gap", "claude-opus-4-8", "", "94") + "]"
	f.tokens = "[" + vec("gap", "claude-opus-4-8", "cacheRead", "213954937") + "]"
	// Flat from the first sample of the scan: the series has not moved for the whole horizon.
	f.matrix = fmt.Sprintf(`[{"metric":{"session_id":"gap"},"values":[[%d,"213954937"],[%d,"213954937"]]}]`,
		at.Add(-6*time.Hour-5*time.Minute).Unix(), at.Unix())

	ledger := &fakeLedger{sessions: map[string]LedgerSession{"gap": {
		SessionID: "gap", Rows: 9, LastRowAt: at.Add(-17 * time.Hour),
		Models: map[string]LedgerModel{"claude-opus-4-8": {Tokens: TokenCounts{CacheRead: 208860870}, CostUSD: 154}},
	}}}
	r := New(NewPromHTTP(f.URL), ledger, nil, Config{})
	r.now = func() time.Time { return at }

	got, err := r.Reconcile(context.Background(), []string{"gap"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Verdict != VerdictDiverged || got[0].Details.Reason != ReasonCaptureGap {
		t.Errorf("verdict/reason = %s/%s, want diverged/capture_gap (vendor stable %ds)", got[0].Verdict, got[0].Details.Reason, got[0].Details.Gates.VendorStableForSec)
	}
}

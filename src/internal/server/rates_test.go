package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/pricing"
	"github.com/bmjdotnet/teamster/internal/store"
)

type fakeRatesStore struct {
	store.Store
	rates  []store.ModelRate
	err    error
	filter store.RateFilter
}

func (f *fakeRatesStore) ListRates(_ context.Context, filter store.RateFilter) ([]store.ModelRate, error) {
	f.filter = filter
	return f.rates, f.err
}

func getRates(t *testing.T, s *Server, target string) (*httptest.ResponseRecorder, ratesResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	s.handleRates(rec, req)
	var resp ratesResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v (body: %s)", err, rec.Body.String())
		}
	}
	return rec, resp
}

func TestRates_ReturnsSeededTable(t *testing.T) {
	ms := openTestObsStore(t)
	want, err := ms.ListRates(context.Background(), store.RateFilter{})
	if err != nil {
		t.Fatalf("ListRates: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("test store has no seeded rates")
	}

	rec, resp := getRates(t, &Server{obsStore: ms}, "/rates")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if resp.Count != len(want) || len(resp.Rates) != len(want) {
		t.Fatalf("count = %d, len(rates) = %d, want %d", resp.Count, len(resp.Rates), len(want))
	}
	if resp.FetchedAt.IsZero() {
		t.Error("top-level fetched_at is zero")
	}
	for i, r := range resp.Rates {
		if r.ID != want[i].ID || r.Runtime != want[i].Runtime || r.ModelKey != want[i].ModelKey {
			t.Fatalf("rate %d = {%d %s %s}, want store order {%d %s %s}",
				i, r.ID, r.Runtime, r.ModelKey, want[i].ID, want[i].Runtime, want[i].ModelKey)
		}
	}
	if !strings.Contains(rec.Body.String(), `"valid_to":null`) {
		t.Errorf("open-ended rows must serialize valid_to as null; body: %.300s", rec.Body.String())
	}
}

func TestRates_FilterByRuntime(t *testing.T) {
	ms := openTestObsStore(t)
	s := &Server{obsStore: ms}

	for _, runtime := range []string{store.RateRuntimeCodex, store.RateRuntimeClaudeCode} {
		rec, resp := getRates(t, s, "/rates?runtime="+runtime)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body: %s", runtime, rec.Code, rec.Body.String())
		}
		if resp.Count == 0 {
			t.Fatalf("%s: no rates returned", runtime)
		}
		for _, r := range resp.Rates {
			if r.Runtime != runtime {
				t.Errorf("runtime=%s returned a %s row (%s)", runtime, r.Runtime, r.ModelKey)
			}
		}
	}

	_, all := getRates(t, s, "/rates")
	_, codex := getRates(t, s, "/rates?runtime=codex")
	if codex.Count >= all.Count {
		t.Errorf("codex count %d not narrower than unfiltered %d", codex.Count, all.Count)
	}
}

func TestRates_FilterByModel(t *testing.T) {
	ms := openTestObsStore(t)
	rec, resp := getRates(t, &Server{obsStore: ms}, "/rates?model=claude-opus-4-6")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if resp.Count == 0 {
		t.Fatal("claude-opus-4-6 not found in seeded rates")
	}
	for _, r := range resp.Rates {
		if r.ModelKey != "claude-opus-4-6" {
			t.Errorf("model filter returned %q", r.ModelKey)
		}
	}
}

func TestRates_EmptyTableIsEmptyListNotError(t *testing.T) {
	rec, resp := getRates(t, &Server{obsStore: &fakeRatesStore{}}, "/rates")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if resp.Count != 0 || len(resp.Rates) != 0 {
		t.Errorf("count = %d, len = %d, want 0", resp.Count, len(resp.Rates))
	}
	if !strings.Contains(rec.Body.String(), `"rates":[]`) {
		t.Errorf(`empty table must serialize "rates":[] (not null); body: %s`, rec.Body.String())
	}
}

func TestRates_FilterMatchingNothingIsEmptyList(t *testing.T) {
	ms := openTestObsStore(t)
	rec, resp := getRates(t, &Server{obsStore: ms}, "/rates?model=no-such-model")
	if rec.Code != http.StatusOK || resp.Count != 0 {
		t.Fatalf("status = %d, count = %d, want 200 and 0", rec.Code, resp.Count)
	}
	if !strings.Contains(rec.Body.String(), `"rates":[]`) {
		t.Errorf(`want "rates":[]; body: %s`, rec.Body.String())
	}
}

func TestRates_RuntimeFilterPassedToStore(t *testing.T) {
	fs := &fakeRatesStore{}
	getRates(t, &Server{obsStore: fs}, "/rates?runtime=codex")
	if fs.filter.Runtime != "codex" {
		t.Errorf("store filter runtime = %q, want codex", fs.filter.Runtime)
	}
}

func TestRates_SerializesDecimalsAsStringsAndTimesAsUTC(t *testing.T) {
	validTo := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("x", -5*3600))
	fs := &fakeRatesStore{rates: []store.ModelRate{{
		ID: 7, Runtime: "claude_code", MatchKind: "exact", ModelKey: "m", Variant: "base",
		InputPerMtok: 10, OutputPerMtok: 50, CacheReadPerMtok: 0.25,
		CacheWrite5mPerMtok: 12.5, CacheWrite1hPerMtok: 20,
		ValidFrom: store.SeedRateValidFrom, ValidTo: &validTo,
		SourceURL: "https://example.test/pricing",
		FetchedAt: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
		Notes:     "n",
	}}}
	rec, _ := getRates(t, &Server{obsStore: fs}, "/rates")

	var raw struct {
		Rates []map[string]any `json:"rates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := raw.Rates[0]
	for field, want := range map[string]string{
		"input_per_mtok":          "10.000000",
		"output_per_mtok":         "50.000000",
		"cache_read_per_mtok":     "0.250000",
		"cache_write_5m_per_mtok": "12.500000",
		"cache_write_1h_per_mtok": "20.000000",
		"valid_from":              "1970-01-01T00:00:00Z",
		"valid_to":                "2026-09-01T17:00:00Z",
		"fetched_at":              "2026-10-03T00:00:00Z",
		"source_url":              "https://example.test/pricing",
		"notes":                   "n",
	} {
		if got[field] != want {
			t.Errorf("%s = %#v, want %q", field, got[field], want)
		}
	}
	if got["id"] != float64(7) {
		t.Errorf("id = %#v, want 7", got["id"])
	}
}

func TestRates_NoStoreIs503(t *testing.T) {
	rec, _ := getRates(t, &Server{}, "/rates")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	assertJSONError(t, rec)
}

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

// A store error can carry DSN fragments or hostnames; the client gets a
// generic message and the operator gets the detail in the log.
func TestRates_StoreErrorIs500WithoutLeakingDetail(t *testing.T) {
	logs := captureSlog(t)
	const secret = "dial tcp db.internal:3306: access denied for user 'root' (password hunter2)"
	rec, _ := getRates(t, &Server{obsStore: &fakeRatesStore{err: errors.New(secret)}}, "/rates")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	assertJSONError(t, rec)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"internal error"}` {
		t.Errorf("body = %s, want the generic internal error", got)
	}
	for _, leak := range []string{"hunter2", "db.internal", "3306", "root", "list rates"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("response leaks %q: %s", leak, rec.Body.String())
		}
	}
	if !strings.Contains(logs.String(), "hunter2") || !strings.Contains(logs.String(), "list rates") {
		t.Errorf("the real error must still be logged for the operator; log: %s", logs.String())
	}
}

func TestRates_EncodeErrorIs500WithoutLeakingDetail(t *testing.T) {
	logs := captureSlog(t)
	// time.Time cannot marshal a year beyond 9999, so this fails the encode step.
	fs := &fakeRatesStore{rates: []store.ModelRate{{
		Runtime: "claude_code", MatchKind: "exact", ModelKey: "m", Variant: "base",
		ValidFrom: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
	}}}
	rec, _ := getRates(t, &Server{obsStore: fs}, "/rates")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"internal error"}` {
		t.Errorf("body = %s, want the generic internal error", got)
	}
	if !strings.Contains(logs.String(), "encode rates") || !strings.Contains(logs.String(), "year outside") {
		t.Errorf("the real encode error must be logged; log: %s", logs.String())
	}
}

func TestRates_RejectsNonGet(t *testing.T) {
	s := &Server{obsStore: &fakeRatesStore{}}
	req := httptest.NewRequest(http.MethodPost, "/rates", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	s.handleRates(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", allow, "GET, HEAD")
	}
}

// TestRates_ServedInBothModes proves /rates is a pure read that read-only
// replicas keep serving, routed through RegisterRoutes rather than to the
// dashboard catch-all.
func TestRates_ServedInBothModes(t *testing.T) {
	for name, readOnly := range map[string]bool{"write-capable": false, "read-only": true} {
		t.Run(name, func(t *testing.T) {
			s := &Server{cfg: config.Config{ReadOnly: readOnly}, obsStore: openTestObsStore(t)}
			mux := http.NewServeMux()
			s.RegisterRoutes(mux)

			req := httptest.NewRequest(http.MethodGet, "/rates?runtime=codex", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %.200s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json (dashboard catch-all?)", ct)
			}
			var resp ratesResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Count == 0 {
				t.Fatalf("unmarshal err = %v, count = %d, body: %.200s", err, resp.Count, rec.Body.String())
			}
		})
	}
}

func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Errorf("want JSON {\"error\": ...}; err = %v, body: %s", err, rec.Body.String())
	}
}

func TestRatesContract_HTTPSourceRoundTrip(t *testing.T) {
	validTo := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("x", -5*3600))
	seed := []store.ModelRate{
		{
			ID: 1, Runtime: "claude_code", MatchKind: "exact", ModelKey: "claude-opus-4", Variant: "base",
			InputPerMtok: 15, OutputPerMtok: 75.125, CacheReadPerMtok: 1.5,
			CacheWrite5mPerMtok: 18.75, CacheWrite1hPerMtok: 30,
			ValidFrom: store.SeedRateValidFrom, ValidTo: &validTo,
			SourceURL: "https://example.test/pricing",
			FetchedAt: time.Date(2026, 10, 3, 1, 2, 3, 0, time.UTC),
			Notes:     "superseded",
		},
		{
			ID: 2, Runtime: "claude_code", MatchKind: "prefix", ModelKey: "claude-sonnet", Variant: "base",
			InputPerMtok: 3.075, OutputPerMtok: 15, CacheReadPerMtok: 0.3,
			CacheWrite5mPerMtok: 3.75, CacheWrite1hPerMtok: 6,
			ValidFrom: time.Date(2026, 9, 1, 17, 0, 0, 0, time.UTC),
			FetchedAt: time.Date(2026, 10, 3, 1, 2, 3, 0, time.UTC),
		},
		{
			ID: 3, Runtime: "codex", MatchKind: "exact", ModelKey: "gpt-5", Variant: "base",
			InputPerMtok: 1.25, OutputPerMtok: 10, CacheReadPerMtok: 0.125,
			ValidFrom: store.SeedRateValidFrom,
			FetchedAt: time.Date(2026, 10, 3, 1, 2, 3, 0, time.UTC),
		},
	}

	srv := &Server{obsStore: &fakeRatesStore{rates: seed}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handleRates))
	t.Cleanup(ts.Close)
	src := pricing.NewHTTPSource(ts.URL)

	sameTime := func(a, b time.Time) bool { return a.Equal(b) }
	assertRate := func(t *testing.T, got, want store.ModelRate) {
		t.Helper()
		if got.ID != want.ID || got.Runtime != want.Runtime || got.MatchKind != want.MatchKind ||
			got.ModelKey != want.ModelKey || got.Variant != want.Variant {
			t.Errorf("identity = %+v, want %+v", got, want)
		}
		for name, pair := range map[string][2]float64{
			"input_per_mtok":          {got.InputPerMtok, want.InputPerMtok},
			"output_per_mtok":         {got.OutputPerMtok, want.OutputPerMtok},
			"cache_read_per_mtok":     {got.CacheReadPerMtok, want.CacheReadPerMtok},
			"cache_write_5m_per_mtok": {got.CacheWrite5mPerMtok, want.CacheWrite5mPerMtok},
			"cache_write_1h_per_mtok": {got.CacheWrite1hPerMtok, want.CacheWrite1hPerMtok},
		} {
			if pair[0] != pair[1] {
				t.Errorf("%s %s = %v, want %v", want.ModelKey, name, pair[0], pair[1])
			}
		}
		if !sameTime(got.ValidFrom, want.ValidFrom) {
			t.Errorf("%s valid_from = %v, want %v", want.ModelKey, got.ValidFrom, want.ValidFrom)
		}
		if (got.ValidTo == nil) != (want.ValidTo == nil) || (want.ValidTo != nil && !sameTime(*got.ValidTo, *want.ValidTo)) {
			t.Errorf("%s valid_to = %v, want %v", want.ModelKey, got.ValidTo, want.ValidTo)
		}
		if !sameTime(got.FetchedAt, want.FetchedAt) {
			t.Errorf("%s fetched_at = %v, want %v", want.ModelKey, got.FetchedAt, want.FetchedAt)
		}
		if got.SourceURL != want.SourceURL || got.Notes != want.Notes {
			t.Errorf("%s source_url/notes = %q/%q, want %q/%q", want.ModelKey, got.SourceURL, got.Notes, want.SourceURL, want.Notes)
		}
	}

	t.Run("all fields round-trip", func(t *testing.T) {
		got, err := src.ListRates(context.Background(), store.RateFilter{})
		if err != nil {
			t.Fatalf("ListRates: %v", err)
		}
		if len(got) != len(seed) {
			t.Fatalf("got %d rates, want %d", len(got), len(seed))
		}
		for i := range seed {
			assertRate(t, got[i], seed[i])
		}
	})

	t.Run("wire field names match on both sides", func(t *testing.T) {
		rec, _ := getRates(t, srv, "/rates")
		var raw struct {
			Rates []map[string]json.RawMessage `json:"rates"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, key := range []string{
			"id", "runtime", "match_kind", "model_key", "variant", "input_per_mtok", "output_per_mtok",
			"cache_read_per_mtok", "cache_write_5m_per_mtok", "cache_write_1h_per_mtok",
			"valid_from", "valid_to", "source_url", "fetched_at", "notes",
		} {
			if _, ok := raw.Rates[0][key]; !ok {
				t.Errorf("server payload missing %q", key)
			}
		}
	})

	t.Run("runtime filter reaches the store", func(t *testing.T) {
		if _, err := src.ListRates(context.Background(), store.RateFilter{Runtime: "codex"}); err != nil {
			t.Fatalf("ListRates: %v", err)
		}
		if got := srv.obsStore.(*fakeRatesStore).filter.Runtime; got != "codex" {
			t.Errorf("store saw runtime %q, want codex", got)
		}
	})
}

func TestRatesContract_HTTPSourceNoStoreIsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc((&Server{}).handleRates))
	t.Cleanup(ts.Close)
	_, err := pricing.NewHTTPSource(ts.URL).ListRates(context.Background(), store.RateFilter{})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want one mentioning 503", err)
	}
}

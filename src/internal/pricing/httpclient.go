package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

const (
	ratesHTTPTimeout = 5 * time.Second
	// maxRatesBody bounds a /rates response; the real table is a few KiB.
	maxRatesBody = 4 << 20
	// maxErrorSnippet bounds how much of a non-200 body goes into the error.
	maxErrorSnippet = 256
)

// HTTPSource is a RateSource backed by hookd's GET /rates endpoint, for
// processes with no direct store connection (the scrapers).
type HTTPSource struct {
	ratesURL string
	client   *http.Client
}

// NewHTTPSource returns a source reading baseURL + "/rates", where baseURL is
// the hookd root (the configured hook URL without its "/event" suffix).
func NewHTTPSource(baseURL string) *HTTPSource {
	return &HTTPSource{
		ratesURL: strings.TrimRight(baseURL, "/") + "/rates",
		client:   &http.Client{Timeout: ratesHTTPTimeout},
	}
}

// wireRate mirrors one element of /rates' "rates" array (internal/server
// rates.go). Rates arrive as DECIMAL(12,6) strings.
type wireRate struct {
	ID                  int64      `json:"id"`
	Runtime             string     `json:"runtime"`
	MatchKind           string     `json:"match_kind"`
	ModelKey            string     `json:"model_key"`
	Variant             string     `json:"variant"`
	InputPerMtok        string     `json:"input_per_mtok"`
	OutputPerMtok       string     `json:"output_per_mtok"`
	CacheReadPerMtok    string     `json:"cache_read_per_mtok"`
	CacheWrite5mPerMtok string     `json:"cache_write_5m_per_mtok"`
	CacheWrite1hPerMtok string     `json:"cache_write_1h_per_mtok"`
	ValidFrom           time.Time  `json:"valid_from"`
	ValidTo             *time.Time `json:"valid_to"`
	SourceURL           string     `json:"source_url"`
	FetchedAt           time.Time  `json:"fetched_at"`
	Notes               string     `json:"notes"`
}

type wireRates struct {
	Rates []wireRate `json:"rates"`
}

// ListRates fetches the rate table, narrowed to filter.Runtime by the server.
// filter.At is applied here, with the same helpers the store backends use,
// because the endpoint has no ?at=. Any malformed row fails the whole fetch: a
// rate that cannot be parsed must never become a silent $0.
func (h *HTTPSource) ListRates(ctx context.Context, filter store.RateFilter) ([]store.ModelRate, error) {
	target := h.ratesURL
	if filter.Runtime != "" {
		target += "?" + url.Values{"runtime": {filter.Runtime}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch rates: %w", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch rates: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorSnippet))
		return nil, fmt.Errorf("fetch rates: %s returned %d: %s",
			h.ratesURL, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var body wireRates
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRatesBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("fetch rates: decode %s: %w", h.ratesURL, err)
	}

	rates := make([]store.ModelRate, 0, len(body.Rates))
	for _, w := range body.Rates {
		r, err := w.modelRate()
		if err != nil {
			return nil, fmt.Errorf("fetch rates: row id=%d %s/%s: %w", w.ID, w.Runtime, w.ModelKey, err)
		}
		rates = append(rates, r)
	}
	if filter.At != nil {
		rates = store.EffectiveRates(rates, *filter.At)
		store.SortRates(rates)
	}
	return rates, nil
}

func (w wireRate) modelRate() (store.ModelRate, error) {
	r := store.ModelRate{
		ID: w.ID, Runtime: w.Runtime, MatchKind: w.MatchKind, ModelKey: w.ModelKey, Variant: w.Variant,
		ValidFrom: w.ValidFrom, ValidTo: w.ValidTo,
		SourceURL: w.SourceURL, FetchedAt: w.FetchedAt, Notes: w.Notes,
	}
	for _, f := range []struct {
		name string
		raw  string
		dst  *float64
	}{
		{"input_per_mtok", w.InputPerMtok, &r.InputPerMtok},
		{"output_per_mtok", w.OutputPerMtok, &r.OutputPerMtok},
		{"cache_read_per_mtok", w.CacheReadPerMtok, &r.CacheReadPerMtok},
		{"cache_write_5m_per_mtok", w.CacheWrite5mPerMtok, &r.CacheWrite5mPerMtok},
		{"cache_write_1h_per_mtok", w.CacheWrite1hPerMtok, &r.CacheWrite1hPerMtok},
	} {
		v, err := strconv.ParseFloat(f.raw, 64)
		if err != nil {
			return store.ModelRate{}, fmt.Errorf("%s %q: %w", f.name, f.raw, err)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return store.ModelRate{}, fmt.Errorf("%s %q is not a finite non-negative rate", f.name, f.raw)
		}
		*f.dst = v
	}
	return r, nil
}

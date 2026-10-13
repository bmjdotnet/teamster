package server

import (
	"context"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
)

func TestEmbeddedFallbackRateIDResolvesSentinelPerRuntime(t *testing.T) {
	st := openTestObsStore(t)
	s := &Server{obsStore: st}

	ids := map[string]int64{}
	for _, runtime := range []string{store.RateRuntimeClaudeCode, store.RateRuntimeCodex} {
		id := s.embeddedFallbackRateID(runtime)
		if id <= 0 {
			t.Fatalf("%s: sentinel id = %d, want > 0", runtime, id)
		}
		ids[runtime] = id
	}
	if ids[store.RateRuntimeClaudeCode] == ids[store.RateRuntimeCodex] {
		t.Errorf("runtimes share sentinel id %d", ids[store.RateRuntimeCodex])
	}

	rates, err := st.ListRates(context.Background(), store.RateFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rates {
		if r.ID == ids[r.Runtime] && r.ModelKey != store.EmbeddedFallbackKey {
			t.Errorf("id %d resolved to %q, not the sentinel", r.ID, r.ModelKey)
		}
	}
}

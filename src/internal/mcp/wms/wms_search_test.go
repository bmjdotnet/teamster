package wms

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// stubSearchStore satisfies wms.Store via the embedded interface (left nil);
// only Search is overridden — any other method the wms_search dispatch does
// not exercise will panic if called, which is the point.
type stubSearchStore struct {
	wms.Store
	gotQuery wms.SearchQuery
	hits     []wms.Hit
	err      error
}

func (s *stubSearchStore) Search(_ context.Context, q wms.SearchQuery) ([]wms.Hit, error) {
	s.gotQuery = q
	return s.hits, s.err
}

func TestHandleToolCall_Search(t *testing.T) {
	store := &stubSearchStore{hits: []wms.Hit{{EntityType: "outcome", EntityID: "out-1", Title: "Found it"}}}
	params, _ := json.Marshal(map[string]interface{}{
		"name": ToolSearch,
		"arguments": map[string]interface{}{
			"query": "found",
			"type":  "outcomes, workunits",
			"tag":   []interface{}{"project=teamster"},
			"since": "72h",
			"limit": float64(5),
		},
	})

	result, callErr := HandleToolCall(store, noopEngine{}, params)
	if callErr != nil {
		t.Fatalf("HandleToolCall: %v", callErr)
	}

	if store.gotQuery.Query != "found" {
		t.Errorf("Query = %q, want %q", store.gotQuery.Query, "found")
	}
	if want := []string{"outcomes", "workunits"}; !reflect.DeepEqual(store.gotQuery.Types, want) {
		t.Errorf("Types = %v, want %v", store.gotQuery.Types, want)
	}
	if want := []string{"project=teamster"}; !reflect.DeepEqual(store.gotQuery.Tags, want) {
		t.Errorf("Tags = %v, want %v", store.gotQuery.Tags, want)
	}
	if store.gotQuery.Limit != 5 {
		t.Errorf("Limit = %d, want 5", store.gotQuery.Limit)
	}
	wantSince := time.Now().UTC().Add(-72 * time.Hour)
	if d := store.gotQuery.Since.Sub(wantSince); d < -time.Minute || d > time.Minute {
		t.Errorf("Since = %v, want ~%v", store.gotQuery.Since, wantSince)
	}

	if len(result.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(result.Content))
	}
	var hits []wms.Hit
	if err := json.Unmarshal([]byte(result.Content[0]["text"].(string)), &hits); err != nil {
		t.Fatalf("unmarshal hits: %v", err)
	}
	if len(hits) != 1 || hits[0].EntityID != "out-1" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestHandleToolCall_Search_RequiresQuery(t *testing.T) {
	store := &stubSearchStore{}
	params, _ := json.Marshal(map[string]interface{}{
		"name":      ToolSearch,
		"arguments": map[string]interface{}{},
	})

	if _, callErr := HandleToolCall(store, noopEngine{}, params); callErr == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestHandleToolCall_Search_BadSince(t *testing.T) {
	store := &stubSearchStore{}
	params, _ := json.Marshal(map[string]interface{}{
		"name": ToolSearch,
		"arguments": map[string]interface{}{
			"query": "found",
			"since": "not-a-time",
		},
	})

	// An unparseable since must error, not silently fall through as "no
	// filter" — that would return unfiltered results as if it had applied.
	if _, callErr := HandleToolCall(store, noopEngine{}, params); callErr == nil {
		t.Fatal("expected error for unparseable since")
	}
}

func TestToolDefs_HasSearch(t *testing.T) {
	for _, def := range ToolDefs {
		if def["name"] == ToolSearch {
			return
		}
	}
	t.Fatalf("%s not found in ToolDefs", ToolSearch)
}

package web

import (
	"encoding/json"
	"net/http"

	"github.com/bmjdotnet/teamster/internal/store"
)

// tagValue is one vocabulary row from the tags table.
type tagValue struct {
	TagValue    string `json:"tag_value"`
	IsSeed      bool   `json:"is_seed"`
	EntityCount int    `json:"entity_count"`
	Description string `json:"description"`
}

// tagKey groups all values sharing a tag_key.
type tagKey struct {
	TagKey      string     `json:"tag_key"`
	Category    string     `json:"category"`
	Cardinality string     `json:"cardinality"`
	Values      []tagValue `json:"values"`
}

// tagsAPIResponse is the JSON envelope for GET /wms/api/tags.
type tagsAPIResponse struct {
	Keys []tagKey `json:"keys"`
}

// HandleTagsPage serves the tag vocabulary browser HTML.
func HandleTagsPage(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("tags.html")
	if err != nil {
		http.Error(w, "tags page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data) //nolint:errcheck
}

// HandleTagsAPI returns an http.HandlerFunc that queries the tags vocabulary
// and returns JSON with entity counts per value.
func HandleTagsAPI(rep store.ReportingStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rep == nil {
			http.Error(w, `{"error":"WMS store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		rows, err := rep.TagsWithEntityCounts(r.Context())
		if err != nil {
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}

		// Accumulate rows into tagKey groups preserving order.
		var keys []tagKey
		keyIndex := map[string]int{}

		for _, tc := range rows {
			tv := tagValue{
				TagValue:    tc.Value,
				IsSeed:      tc.IsSeed,
				EntityCount: int(tc.EntityCount),
				Description: tc.Description,
			}
			if idx, ok := keyIndex[tc.Key]; ok {
				keys[idx].Values = append(keys[idx].Values, tv)
			} else {
				keyIndex[tc.Key] = len(keys)
				keys = append(keys, tagKey{
					TagKey:      tc.Key,
					Category:    tc.Category,
					Cardinality: tc.Cardinality,
					Values:      []tagValue{tv},
				})
			}
		}

		if keys == nil {
			keys = []tagKey{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tagsAPIResponse{Keys: keys}) //nolint:errcheck
	}
}

package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
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
func HandleTagsAPI(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, `{"error":"WMS store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		const q = `
SELECT t.tag_key, t.tag_value, t.is_seed, t.category, t.cardinality,
       COALESCE(t.description, ''),
       COUNT(et.tag_id) AS entity_count
FROM tags t
LEFT JOIN entity_tags et ON et.tag_id = t.id
GROUP BY t.id
ORDER BY t.category, t.tag_key, t.tag_value`

		rows, err := db.QueryContext(r.Context(), q)
		if err != nil {
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		// Accumulate rows into tagKey groups preserving order.
		var keys []tagKey
		keyIndex := map[string]int{}

		for rows.Next() {
			var (
				tagKeyStr   string
				tagValStr   string
				isSeedInt   int
				category    string
				cardinality string
				description string
				count       int
			)
			if err := rows.Scan(&tagKeyStr, &tagValStr, &isSeedInt, &category, &cardinality, &description, &count); err != nil {
				http.Error(w, `{"error":"scan failed"}`, http.StatusInternalServerError)
				return
			}
			tv := tagValue{
				TagValue:    tagValStr,
				IsSeed:      isSeedInt == 1,
				EntityCount: count,
				Description: description,
			}
			if idx, ok := keyIndex[tagKeyStr]; ok {
				keys[idx].Values = append(keys[idx].Values, tv)
			} else {
				keyIndex[tagKeyStr] = len(keys)
				keys = append(keys, tagKey{
					TagKey:      tagKeyStr,
					Category:    category,
					Cardinality: cardinality,
					Values:      []tagValue{tv},
				})
			}
		}
		if err := rows.Err(); err != nil {
			http.Error(w, `{"error":"rows error"}`, http.StatusInternalServerError)
			return
		}

		if keys == nil {
			keys = []tagKey{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tagsAPIResponse{Keys: keys}) //nolint:errcheck
	}
}

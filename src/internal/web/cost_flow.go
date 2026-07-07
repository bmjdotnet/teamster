package web

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

// costFlowNode is a node in the Sankey diagram.
type costFlowNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group"`
}

// costFlowLink is a directed edge in the Sankey diagram.
type costFlowLink struct {
	Source string  `json:"source"`
	Target string  `json:"target"`
	Value  float64 `json:"value"`
}

// costFlowResponse is the JSON envelope returned by /wms/api/cost-flow.
type costFlowResponse struct {
	Nodes    []costFlowNode   `json:"nodes"`
	Links    []costFlowLink   `json:"links"`
	Metadata costFlowMetadata `json:"metadata"`
}

type costFlowMetadata struct {
	From         string  `json:"from"`
	To           string  `json:"to"`
	View         string  `json:"view"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// HandleCostFlowPage serves the Sankey HTML page.
func HandleCostFlowPage(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("cost_flow.html")
	if err != nil {
		http.Error(w, "cost_flow page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data) //nolint:errcheck
}

// HandleCostFlowAPI returns an http.HandlerFunc that queries cost-flow data
// and returns Sankey-format JSON.
func HandleCostFlowAPI(rep store.ReportingStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rep == nil {
			http.Error(w, `{"error":"WMS store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		q := r.URL.Query()
		fromStr := q.Get("from")
		toStr := q.Get("to")
		view := q.Get("view")

		now := time.Now()
		if fromStr == "" {
			fromStr = now.AddDate(0, 0, -30).Format("2006-01-02")
		}
		if toStr == "" {
			toStr = now.Format("2006-01-02")
		}
		switch view {
		case "project-worktype-model", "entity-agent":
		default:
			view = "project-phase-agent"
		}

		from, err := time.Parse("2006-01-02", fromStr)
		if err != nil {
			http.Error(w, `{"error":"invalid from date"}`, http.StatusBadRequest)
			return
		}
		to, err := time.Parse("2006-01-02", toStr)
		if err != nil {
			http.Error(w, `{"error":"invalid to date"}`, http.StatusBadRequest)
			return
		}

		graph, err := rep.CostFlowSankey(r.Context(), view, from, to)
		if err != nil {
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}

		nodes := make([]costFlowNode, 0, len(graph.Nodes))
		for _, n := range graph.Nodes {
			nodes = append(nodes, costFlowNode{ID: n.ID, Label: n.Label, Group: n.Group})
		}
		links := make([]costFlowLink, 0, len(graph.Links))
		total := 0.0
		for _, l := range graph.Links {
			links = append(links, costFlowLink{Source: l.Source, Target: l.Target, Value: l.Value})
			if l.Source == "total" {
				total += l.Value
			}
		}

		resp := costFlowResponse{
			Nodes: nodes,
			Links: links,
			Metadata: costFlowMetadata{
				From:         fromStr,
				To:           toStr,
				View:         view,
				TotalCostUSD: total,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}
}

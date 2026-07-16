package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPromClient_ToolCounts_ParsesResultVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == "" {
			t.Fatal("expected a non-empty query param")
		}
		fmt.Fprint(w, `{
			"status": "success",
			"data": {
				"result": [
					{"metric": {"tool": "Read", "agent_name": "@engine"}, "value": [1720000000, "42"]},
					{"metric": {"tool": "Bash", "agent_name": "@engine"}, "value": [1720000000, "7"]},
					{"metric": {"tool": "Read", "agent_name": ""}, "value": [1720000000, "3"]}
				]
			}
		}`)
	}))
	defer srv.Close()

	c := newPromClient(srv.URL)
	counts, err := c.ToolCounts(context.Background(), "hub01")
	if err != nil {
		t.Fatalf("ToolCounts: %v", err)
	}

	if counts["@engine"]["Read"] != 42 {
		t.Errorf("@engine Read = %d, want 42", counts["@engine"]["Read"])
	}
	if counts["@engine"]["Bash"] != 7 {
		t.Errorf("@engine Bash = %d, want 7", counts["@engine"]["Bash"])
	}
	if counts[""]["Read"] != 3 {
		t.Errorf("lead (\"\") Read = %d, want 3", counts[""]["Read"])
	}
}

func TestPromClient_ToolCounts_NonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status": "error"}`)
	}))
	defer srv.Close()

	c := newPromClient(srv.URL)
	if _, err := c.ToolCounts(context.Background(), "hub01"); err == nil {
		t.Fatal("expected an error for status != success")
	}
}

func TestPromClient_ToolCounts_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newPromClient(srv.URL)
	if _, err := c.ToolCounts(context.Background(), "hub01"); err == nil {
		t.Fatal("expected an error for non-200 response")
	}
}

func TestPromClient_ToolCounts_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status": "success", "data": {"result": []}}`)
	}))
	defer srv.Close()

	c := newPromClient(srv.URL)
	counts, err := c.ToolCounts(context.Background(), "hub01")
	if err != nil {
		t.Fatalf("ToolCounts: %v", err)
	}
	if len(counts) != 0 {
		t.Fatalf("counts = %+v, want empty", counts)
	}
}

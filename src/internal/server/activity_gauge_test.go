package server

import (
	"context"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/hook"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
)

func TestActivityFromData(t *testing.T) {
	cases := []struct {
		name        string
		data        map[string]interface{}
		wantTag     string
		wantDisplay string
	}{
		{
			name:        "tool_tag/tool_display",
			data:        map[string]interface{}{"_tool_tag": "READ", "_tool_display": "reading __foo.go__"},
			wantTag:     "READ",
			wantDisplay: "reading __foo.go__",
		},
		{
			name:        "reportActivity thought",
			data:        map[string]interface{}{"_thought": "fixing auth bug"},
			wantTag:     "THNK",
			wantDisplay: "fixing auth bug",
		},
		{
			name:        "completeActivity done",
			data:        map[string]interface{}{"_done": "fixed auth bug, tests pass"},
			wantTag:     "DONE",
			wantDisplay: "fixed auth bug, tests pass",
		},
		{
			name:        "nothing enriched",
			data:        map[string]interface{}{"_focus": "outcome muster-must-fixes: fix it"},
			wantTag:     "",
			wantDisplay: "",
		},
		{
			name:        "nil data",
			data:        nil,
			wantTag:     "",
			wantDisplay: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, display := activityFromData(tc.data)
			if tag != tc.wantTag || display != tc.wantDisplay {
				t.Fatalf("activityFromData() = (%q, %q), want (%q, %q)", tag, display, tc.wantTag, tc.wantDisplay)
			}
		})
	}
}

// TestDispatchObservability_UpdatesGaugeActivity is an end-to-end check (via
// the fakeGaugeStore from health_api_test.go) that a PreToolUse event carrying
// hook.EnrichRecord's enrichment fields results in a GaugeStore.UpdateActivity
// call — the fix for the ACTIVITY column always being empty in ctop/health.html.
func TestDispatchObservability_UpdatesGaugeActivity(t *testing.T) {
	gs := newFakeGaugeStore()
	s := &Server{
		cfg:        config.Config{Host: "testhost"},
		sessions:   observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		metrics:    observability.NewMetrics(prometheus.NewRegistry()),
		gaugeStore: gs,
	}

	data := map[string]interface{}{
		"hook_event_name": "PreToolUse",
		"session_id":      "s1",
		"tool_name":       "Read",
		"_tool_tag":       "READ",
		"_tool_display":   "reading __foo.go__",
	}
	s.dispatchObservability(hook.HookEvent{
		HookEventName: "PreToolUse",
		SessionID:     "s1",
		ToolName:      "Read",
	}, data)

	key := gauge.GaugeKey{Host: "testhost", SessionID: "s1", AgentName: ""}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if row, found, _ := gs.Get(context.Background(), key); found && row.LastActivityDisplay != "" {
			if row.LastActivityDisplay != "reading __foo.go__" || row.LastActivityTool != "READ" {
				t.Fatalf("gauge row = %+v, want display=%q tool=READ", row, "reading __foo.go__")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gauge row never got last_activity fields within deadline")
}

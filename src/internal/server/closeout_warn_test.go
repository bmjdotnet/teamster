package server

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/hook"
	"github.com/bmjdotnet/teamster/internal/observability"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
	"github.com/prometheus/client_golang/prometheus"
)

// TestMissingRequiredKeys covers the W2 soft close-out computation: which
// required keys lack a bound tag on the workunit. Order follows `required`.
func TestMissingRequiredKeys(t *testing.T) {
	tag := func(k string) wms.EntityTag { return wms.EntityTag{TagKey: k} }

	cases := []struct {
		name     string
		required []string
		present  []wms.EntityTag
		want     []string
	}{
		{
			name:     "all present",
			required: []string{"work-type", "phase"},
			present:  []wms.EntityTag{tag("work-type"), tag("phase")},
			want:     nil,
		},
		{
			name:     "one missing",
			required: []string{"work-type", "phase"},
			present:  []wms.EntityTag{tag("phase")},
			want:     []string{"work-type"},
		},
		{
			name:     "all missing preserves required order",
			required: []string{"work-type", "phase", "product"},
			present:  nil,
			want:     []string{"work-type", "phase", "product"},
		},
		{
			name:     "no required keys",
			required: nil,
			present:  []wms.EntityTag{tag("phase")},
			want:     nil,
		},
		{
			name:     "extra unrelated tags are ignored",
			required: []string{"work-type"},
			present:  []wms.EntityTag{tag("work-type"), tag("priority"), tag("product")},
			want:     nil,
		},
		{
			name:     "duplicate present tag still satisfies its key",
			required: []string{"work-type", "phase"},
			present:  []wms.EntityTag{tag("work-type"), tag("work-type")},
			want:     []string{"phase"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingRequiredKeys(tc.required, tc.present)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("missingRequiredKeys() = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeCloseoutStore satisfies store.Store via the embedded interface (left nil:
// any method the close-out path does not exercise will panic if called, which is
// the desired failure signal). It overrides only the three methods the
// WMSStatusChange→done→workunit branch touches.
type fakeCloseoutStore struct {
	store.Store
	required []string
	tags     []wms.EntityTag
}

func (f *fakeCloseoutStore) ListRequiredTagKeys(context.Context) ([]string, error) {
	return f.required, nil
}

func (f *fakeCloseoutStore) GetEntityTags(_ context.Context, _, _ string) ([]wms.EntityTag, error) {
	return f.tags, nil
}

func (f *fakeCloseoutStore) CloseFocusInterval(context.Context, store.SessionKey) error {
	return nil
}

func (f *fakeCloseoutStore) CloseFocusIntervalForEntity(context.Context, store.SessionKey, string, string) error {
	return nil
}

// TestEmitCloseOutWarning_WMSStatusChange exercises the full emit path: a
// workunit→done WMSStatusChange whose entity is missing a required tag must
// land a WMSCloseOutWarning record in the JSONL log carrying the entity id and
// the missing keys. Closes the gap the pure missingRequiredKeys test leaves —
// the goroutine, store reads, and buildRecord/emit are all in scope here.
func TestEmitCloseOutWarning_WMSStatusChange(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		logFile:  f,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		obsStore: &fakeCloseoutStore{
			required: []string{"work-type", "phase"},
			tags:     []wms.EntityTag{{TagKey: "phase"}}, // work-type missing
		},
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-test",
		"wms_old_status":  "active",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  "s1",
		"wms_agent_name":  "store",
	})

	rec := waitForCloseOutWarning(t, logPath)
	if got := rec["entity_id"]; got != "wu-test" {
		t.Errorf("entity_id = %v, want %q", got, "wu-test")
	}
	missing, _ := rec["missing"].([]interface{})
	if len(missing) != 1 || missing[0] != "work-type" {
		t.Errorf("missing = %v, want [work-type]", rec["missing"])
	}
	if got := rec["session"]; got != "s1" {
		t.Errorf("session = %v, want %q (should carry triggering session, not hardcoded 'wms')", got, "s1")
	}
}

// TestEmitCloseOutWarning_NoWarnWhenSatisfied is the negative case: a workunit
// with every required tag set produces no WMSCloseOutWarning record.
func TestEmitCloseOutWarning_NoWarnWhenSatisfied(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	s := &Server{
		cfg:      config.Config{Host: "testhost"},
		logFile:  f,
		metrics:  observability.NewMetrics(prometheus.NewRegistry()),
		sessions: observability.NewSessionTracker("testhost", time.Minute, time.Minute, nil),
		obsStore: &fakeCloseoutStore{
			required: []string{"work-type"},
			tags:     []wms.EntityTag{{TagKey: "work-type"}},
		},
	}
	s.bus.subscribers = make(map[uint64]chan ssePayload)

	s.dispatchObservability(hook.HookEvent{HookEventName: "WMSStatusChange"}, map[string]interface{}{
		"hook_event_name": "WMSStatusChange",
		"wms_entity_type": wms.EntityWorkUnit,
		"wms_entity_id":   "wu-ok",
		"wms_new_status":  wms.StatusDone,
		"wms_session_id":  "s1",
	})

	// Give the detached goroutine time to (not) write.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if findCloseOutWarning(logPath) != nil {
			t.Fatal("unexpected WMSCloseOutWarning emitted when required tags satisfied")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForCloseOutWarning polls the JSONL log until a WMSCloseOutWarning record
// appears (the emit runs in a detached goroutine) or a 2s deadline elapses.
func waitForCloseOutWarning(t *testing.T, logPath string) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec := findCloseOutWarning(logPath); rec != nil {
			return rec
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no WMSCloseOutWarning record in %s within deadline", logPath)
	return nil
}

// findCloseOutWarning scans the JSONL log for a record whose event field is
// WMSCloseOutWarning and returns it, or nil if none is present.
func findCloseOutWarning(logPath string) map[string]interface{} {
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["event"] == "WMSCloseOutWarning" {
			return rec
		}
	}
	return nil
}

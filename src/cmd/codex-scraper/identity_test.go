package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fixtureSubagentMeta is the real first rollout line of the Codex 0.160.0
// subagent thread 01a108e6 ("Avicenna"), captured on chunk 2026-10-05
// (base_instructions and creator ids redacted).
const fixtureSubagentMeta = `{"timestamp":"2026-10-04T21:51:32.368Z","type":"session_meta","payload":{"creator_user_id":"user-REDACTED","creator_account_id":"REDACTED","session_id":"01a108dd-60b4-7722-b2d1-36ee22189a03","id":"01a108e6-9810-7cd1-a1a2-2d7d1eee6a0b","parent_thread_id":"01a108dd-60b4-7722-b2d1-36ee22189a03","timestamp":"2026-10-04T21:51:32.368Z","cwd":"/home/claude/teamster","runtime_workspace_roots":["/home/claude/teamster"],"originator":"codex-tui","cli_version":"0.160.0","source":{"subagent":{"thread_spawn":{"parent_thread_id":"01a108dd-60b4-7722-b2d1-36ee22189a03","depth":1,"agent_path":"/root/review_data_analysis","agent_nickname":"Avicenna","agent_role":null}}},"thread_source":"subagent","agent_nickname":"Avicenna","agent_path":"/root/review_data_analysis","model_provider":"openai","base_instructions":"<redacted>","history_mode":"paginated","multi_agent_version":"v2","context_window":{"window_id":"01a108e6-9810-7cd1-a1a2-2d80da665f51"}}}`

func metaLine(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"timestamp": "2026-10-04T21:51:32.368Z", "type": "session_meta", "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestProcessLine_SubagentIdentity(t *testing.T) {
	const (
		rootID = "01a108dd-60b4-7722-b2d1-36ee22189a03"
		subID  = "01a108e6-9810-7cd1-a1a2-2d7d1eee6a0b"
	)
	sub := func(extra map[string]any) map[string]any {
		p := map[string]any{"id": subID, "session_id": rootID, "parent_thread_id": rootID, "thread_source": "subagent"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	cases := []struct {
		name        string
		line        func(*testing.T) []byte
		wantName    string
		wantSessID  string
		wantRel     string
		wantAgentID string
	}{
		{"fixture 0.160.0 subagent", func(*testing.T) []byte { return []byte(fixtureSubagentMeta) }, "@Avicenna", rootID, "subagent", subID},
		{"nickname wins over role", func(t *testing.T) []byte {
			return metaLine(t, sub(map[string]any{"agent_role": "reviewer", "agent_nickname": "Avicenna"}))
		}, "@Avicenna", rootID, "subagent", subID},
		{"nickname with spaces", func(t *testing.T) []byte {
			return metaLine(t, sub(map[string]any{"agent_nickname": "Avicenna the 2nd"}))
		}, "@Avicenna-the-2nd", rootID, "subagent", subID},
		{"role only", func(t *testing.T) []byte {
			return metaLine(t, sub(map[string]any{"agent_role": "reviewer"}))
		}, "@reviewer", rootID, "subagent", subID},
		{"neither nickname nor role", func(t *testing.T) []byte { return metaLine(t, sub(nil)) }, "@01a108e6", rootID, "subagent", subID},
		{"thread_source subagent without parent id", func(t *testing.T) []byte {
			return metaLine(t, map[string]any{"id": subID, "session_id": rootID, "thread_source": "subagent", "agent_nickname": "Mill"})
		}, "@Mill", rootID, "subagent", subID},
		{"root thread", func(t *testing.T) []byte {
			return metaLine(t, map[string]any{"id": rootID, "session_id": rootID, "thread_source": "user", "agent_nickname": "ignored"})
		}, "", rootID, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = map[string]any{}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer ts.Close()

			s, _, _ := newTestScraper(t)
			s.st = &httpSessionUpserter{client: ts.Client(), sessionURL: ts.URL}
			cur := &cursorEntry{}
			if err := s.processLine(context.Background(), tc.line(t), cur, "x.jsonl"); err != nil {
				t.Fatal(err)
			}
			if cur.SessionID != tc.wantSessID {
				t.Errorf("SessionID = %q, want %q", cur.SessionID, tc.wantSessID)
			}
			if tc.wantAgentID != "" && cur.ThreadID != tc.wantAgentID {
				t.Errorf("ThreadID = %q, want %q", cur.ThreadID, tc.wantAgentID)
			}
			if cur.AgentName != tc.wantName {
				t.Errorf("AgentName = %q, want %q", cur.AgentName, tc.wantName)
			}

			s.upsertCodexSession(context.Background(), cur)
			if got == nil {
				t.Fatal("no session POST received")
			}
			if got["agent_name"] != tc.wantName {
				t.Errorf("body agent_name = %v, want %q", got["agent_name"], tc.wantName)
			}
			rel, hasRel := got["relationship"]
			aid, hasAID := got["agent_id"]
			if tc.wantRel == "" {
				if hasRel || hasAID {
					t.Errorf("root thread body carries relationship=%v agent_id=%v, want both omitted", rel, aid)
				}
				return
			}
			if rel != tc.wantRel || aid != tc.wantAgentID {
				t.Errorf("body relationship=%v agent_id=%v, want %q/%q", rel, aid, tc.wantRel, tc.wantAgentID)
			}
		})
	}
}

func TestSubagentName_ASCIIWhitespaceRule(t *testing.T) {
	const id = "01a108e6-9810-7cd1-a1a2-2d7d1eee6a0b"
	cases := []struct{ nick, role, want string }{
		{" ", "", "@01a108e6"},
		{" ", "rev", "@rev"},
		{"\x1cA\x1cB", "", "@\x1cA\x1cB"},
		{"A B", "", "@A B"},
		{"A\t\tB", "", "@A-B"},
		{"  A \n B\v", "", "@A-B"},
	}
	for _, tc := range cases {
		got := subagentName(sessionMetaPayload{ID: id, AgentNickname: tc.nick, AgentRole: tc.role})
		if got != tc.want {
			t.Errorf("nick=%q role=%q: got %q, want %q", tc.nick, tc.role, got, tc.want)
		}
	}
}

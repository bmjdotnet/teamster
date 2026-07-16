package roster

import (
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

func TestComputeLiveness(t *testing.T) {
	now := time.Now().UTC()
	ptr := func(s string) *string { return &s }
	timePtr := func(t time.Time) *time.Time { return &t }

	tests := []struct {
		name    string
		entry   store.RosterEntry
		session *store.Session
		want    string
	}{
		{
			name: "unbound — no session_id",
			entry: store.RosterEntry{
				RosterID:  "r-1",
				CreatedAt: now,
			},
			want: LivenessUnbound,
		},
		{
			name: "live — last_seen within 15s",
			entry: store.RosterEntry{
				RosterID:  "r-2",
				SessionID: ptr("sess-2"),
				BoundAt:   timePtr(now),
			},
			session: &store.Session{
				LastSeen: now.Add(-5 * time.Second),
				Status:   store.SessionStatusActive,
			},
			want: LivenessLive,
		},
		{
			name: "idle — last_seen between 15s and 5min",
			entry: store.RosterEntry{
				RosterID:  "r-3",
				SessionID: ptr("sess-3"),
				BoundAt:   timePtr(now),
			},
			session: &store.Session{
				LastSeen: now.Add(-2 * time.Minute),
				Status:   store.SessionStatusActive,
			},
			want: LivenessIdle,
		},
		{
			name: "stale — last_seen over 5min",
			entry: store.RosterEntry{
				RosterID:  "r-4",
				SessionID: ptr("sess-4"),
				BoundAt:   timePtr(now),
			},
			session: &store.Session{
				LastSeen: now.Add(-10 * time.Minute),
				Status:   store.SessionStatusActive,
			},
			want: LivenessStale,
		},
		{
			name: "closed — session status closed",
			entry: store.RosterEntry{
				RosterID:  "r-5",
				SessionID: ptr("sess-5"),
				BoundAt:   timePtr(now),
			},
			session: &store.Session{
				LastSeen: now.Add(-1 * time.Minute),
				Status:   store.SessionStatusClosed,
			},
			want: LivenessClosed,
		},
		{
			name: "bound but no session row — falls back to bound_at",
			entry: store.RosterEntry{
				RosterID:  "r-6",
				SessionID: ptr("sess-6"),
				BoundAt:   timePtr(now.Add(-3 * time.Minute)),
			},
			session: nil,
			want:    LivenessIdle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeLiveness(tt.entry, tt.session)
			if got != tt.want {
				t.Fatalf("ComputeLiveness = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaultLivenessSet(t *testing.T) {
	if !DefaultLivenessSet[LivenessLive] {
		t.Fatal("live should be in default set")
	}
	if !DefaultLivenessSet[LivenessIdle] {
		t.Fatal("idle should be in default set")
	}
	if !DefaultLivenessSet[LivenessUnbound] {
		t.Fatal("unbound should be in default set")
	}
	if DefaultLivenessSet[LivenessClosed] {
		t.Fatal("closed should NOT be in default set")
	}
	if DefaultLivenessSet[LivenessStale] {
		t.Fatal("stale should NOT be in default set")
	}
}

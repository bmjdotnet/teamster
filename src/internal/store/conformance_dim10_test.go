// Conformance dimension 10: VerificationStore — the OTel reconciler's verdict
// table. Exercises both backends via run().
package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

var verT0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func verRow(session, verdict string, evaluated time.Time, converged *time.Time) store.Verification {
	return store.Verification{
		SessionID: session, Runtime: "claude_code", HubUSD: 154.07, VendorUSD: 94.08, DeltaUSD: 59.99,
		Verdict: verdict, ConvergedAt: converged, EvaluatedAt: evaluated,
	}
}

func tp(t time.Time) *time.Time { return &t }

func listAll(t *testing.T, s store.Store, runtime string) []store.Verification {
	t.Helper()
	got, err := s.ListVerifications(context.Background(), runtime, time.Time{})
	if err != nil {
		t.Fatalf("ListVerifications(%q): %v", runtime, err)
	}
	return got
}

func TestVerificationRoundTrip(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		want := store.Verification{
			SessionID: "d6bae2d7", Runtime: "claude_code",
			HubUSD: 154.07, VendorUSD: 94.08, DeltaUSD: 59.99,
			Verdict: "diverged", ConvergedAt: tp(verT0.Add(-time.Hour)), EvaluatedAt: verT0,
			Details: []byte(`{"reason":"capture_gap","models":[{"model":"claude-opus-5-5","hub_usd":71.06}]}`),
		}
		neg := store.Verification{
			SessionID: "neg", Runtime: "claude_code", HubUSD: 771.86, VendorUSD: 772.14, DeltaUSD: -0.28,
			Verdict: "within_tolerance", EvaluatedAt: verT0.Add(time.Second),
		}
		if err := s.UpsertVerifications(context.Background(), []store.Verification{want, neg}); err != nil {
			t.Fatalf("UpsertVerifications: %v", err)
		}
		got := listAll(t, s, "claude_code")
		if !reflect.DeepEqual(got, []store.Verification{want, neg}) {
			t.Errorf("round trip differs:\n got  %+v\n want %+v", got, []store.Verification{want, neg})
		}
	})
}

func TestVerificationUpsertReplacesInPlace(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		first := verRow("s1", "unconverged", verT0, nil)
		first.Details = []byte(`{"reason":"vendor_active"}`)
		if err := s.UpsertVerifications(ctx, []store.Verification{first}); err != nil {
			t.Fatal(err)
		}
		second := verRow("s1", "diverged", verT0.Add(10*time.Minute), tp(verT0.Add(10*time.Minute)))
		second.HubUSD, second.VendorUSD, second.DeltaUSD = 10, 8.5, 1.5
		if err := s.UpsertVerifications(ctx, []store.Verification{second}); err != nil {
			t.Fatal(err)
		}
		got := listAll(t, s, "claude_code")
		if len(got) != 1 || !reflect.DeepEqual(got[0], second) {
			t.Fatalf("want one row equal to the second write (details cleared), got %+v", got)
		}
	})
}

func TestVerificationConvergedAtHasMemory(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		at := func(m int) time.Time { return verT0.Add(time.Duration(m) * time.Minute) }
		step := func(name string, v store.Verification, wantConverged *time.Time) {
			t.Helper()
			if err := s.UpsertVerifications(ctx, []store.Verification{v}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got := listAll(t, s, "claude_code")
			if len(got) != 1 {
				t.Fatalf("%s: rows = %d, want 1", name, len(got))
			}
			if !reflect.DeepEqual(got[0].ConvergedAt, wantConverged) {
				t.Errorf("%s: converged_at = %v, want %v", name, deref(got[0].ConvergedAt), deref(wantConverged))
			}
			if !got[0].EvaluatedAt.Equal(v.EvaluatedAt) {
				t.Errorf("%s: evaluated_at = %v, want %v", name, got[0].EvaluatedAt, v.EvaluatedAt)
			}
		}

		step("first converged", verRow("s1", "within_tolerance", at(0), tp(at(0))), tp(at(0)))
		step("re-evaluated converged keeps the first time", verRow("s1", "within_tolerance", at(10), tp(at(10))), tp(at(0)))
		step("falls out of convergence clears it", verRow("s1", "unconverged", at(20), nil), nil)
		step("converges again takes the new time", verRow("s1", "diverged", at(30), tp(at(30))), tp(at(30)))
		step("never-converged row stays nil", verRow("s1", "unverifiable", at(40), nil), nil)
	})
}

func deref(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func TestVerificationRuntimeDefaultAndFilter(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		implicit := verRow("s1", "within_tolerance", verT0, nil)
		implicit.Runtime = ""
		codex := verRow("s1", "diverged", verT0, nil)
		codex.Runtime = "codex"
		if err := s.UpsertVerifications(ctx, []store.Verification{implicit, codex}); err != nil {
			t.Fatal(err)
		}

		claude := listAll(t, s, "")
		if len(claude) != 1 || claude[0].Runtime != "claude_code" || claude[0].Verdict != "within_tolerance" {
			t.Errorf(`runtime "" must list claude_code only, got %+v`, claude)
		}
		cx := listAll(t, s, "codex")
		if len(cx) != 1 || cx[0].Verdict != "diverged" {
			t.Errorf("codex list = %+v", cx)
		}
		if got := listAll(t, s, "gemini"); len(got) != 0 {
			t.Errorf("unknown runtime must list nothing, got %+v", got)
		}

		bad := verRow("s2", "diverged", verT0, nil)
		bad.Runtime = "gemini"
		if err := s.UpsertVerifications(ctx, []store.Verification{bad}); err == nil {
			t.Error("upsert with an unknown runtime must fail")
		}
	})
}

func TestVerificationListFilterAndOrder(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		// Inserted b, c, a with evaluated_at running opposite to session_id, so
		// neither insertion order nor an evaluated_at index scan yields the
		// documented session_id order by accident.
		rows := []store.Verification{
			verRow("b", "unconverged", verT0.Add(time.Minute), nil),
			verRow("c", "unconverged", verT0, nil),
			verRow("a", "unconverged", verT0.Add(2*time.Minute), nil),
		}
		if err := s.UpsertVerifications(ctx, rows); err != nil {
			t.Fatal(err)
		}
		ids := func(since time.Time) string {
			got, err := s.ListVerifications(ctx, "claude_code", since)
			if err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for _, v := range got {
				b.WriteString(v.SessionID)
			}
			return b.String()
		}
		if got := ids(time.Time{}); got != "abc" {
			t.Errorf("order = %q, want abc (by session_id)", got)
		}
		if got := ids(verT0.Add(time.Minute)); got != "ab" {
			t.Errorf("since is inclusive: got %q, want ab", got)
		}
		if got := ids(verT0.Add(3 * time.Minute)); got != "" {
			t.Errorf("since past every row: got %q, want none", got)
		}
	})
}

func TestVerificationDetailsNilAndEmptyStoreNull(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		a := verRow("nil", "unconverged", verT0, nil)
		b := verRow("empty", "unconverged", verT0, nil)
		b.Details = []byte{}
		if err := s.UpsertVerifications(context.Background(), []store.Verification{a, b}); err != nil {
			t.Fatal(err)
		}
		for _, v := range listAll(t, s, "claude_code") {
			if v.Details != nil {
				t.Errorf("%s: details = %q, want nil", v.SessionID, v.Details)
			}
		}
	})
}

func TestVerificationInvalidRowsRejectedWholeBatch(t *testing.T) {
	good := verRow("good", "within_tolerance", verT0, nil)
	tests := map[string]func(*store.Verification){
		"empty session":         func(v *store.Verification) { v.SessionID = "" },
		"session too long":      func(v *store.Verification) { v.SessionID = strings.Repeat("x", 65) },
		"empty verdict":         func(v *store.Verification) { v.Verdict = "" },
		"verdict too long":      func(v *store.Verification) { v.Verdict = strings.Repeat("v", 33) },
		"zero evaluated_at":     func(v *store.Verification) { v.EvaluatedAt = time.Time{} },
		"zero-time converged":   func(v *store.Verification) { v.ConvergedAt = tp(time.Time{}) },
		"details not json":      func(v *store.Verification) { v.Details = []byte(`{"unterminated`) },
		"details bare garbage":  func(v *store.Verification) { v.Details = []byte(`not json`) },
		"unknown runtime value": func(v *store.Verification) { v.Runtime = "other" },
	}
	for name, corrupt := range tests {
		corrupt := corrupt
		t.Run(name, func(t *testing.T) {
			run(t, func(t *testing.T, s store.Store) {
				bad := verRow("bad", "diverged", verT0, nil)
				corrupt(&bad)
				err := s.UpsertVerifications(context.Background(), []store.Verification{good, bad})
				if err == nil {
					t.Fatal("want an error")
				}
				if !strings.Contains(err.Error(), "row 1") {
					t.Errorf("error should name the offending row index: %v", err)
				}
				if got := listAll(t, s, "claude_code"); len(got) != 0 {
					t.Errorf("a bad row must reject the whole batch, but %d row(s) were written", len(got))
				}
			})
		})
	}
}

func TestVerificationEmptyBatchIsNoOp(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		if err := s.UpsertVerifications(context.Background(), nil); err != nil {
			t.Fatalf("nil batch: %v", err)
		}
		if err := s.UpsertVerifications(context.Background(), []store.Verification{}); err != nil {
			t.Fatalf("empty batch: %v", err)
		}
		if got := listAll(t, s, "claude_code"); len(got) != 0 {
			t.Errorf("rows = %+v, want none", got)
		}
	})
}

func TestVerificationDuplicateKeyInOneBatchLastWins(t *testing.T) {
	run(t, func(t *testing.T, s store.Store) {
		a := verRow("dup", "unconverged", verT0, nil)
		b := verRow("dup", "diverged", verT0.Add(time.Minute), tp(verT0.Add(time.Minute)))
		if err := s.UpsertVerifications(context.Background(), []store.Verification{a, b}); err != nil {
			t.Fatal(err)
		}
		got := listAll(t, s, "claude_code")
		if len(got) != 1 || got[0].Verdict != "diverged" || got[0].ConvergedAt == nil {
			t.Errorf("got %+v, want the later duplicate to win", got)
		}
	})
}

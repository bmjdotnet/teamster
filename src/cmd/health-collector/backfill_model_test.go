package main

import (
	"context"
	"errors"
	"testing"

	"github.com/bmjdotnet/teamster/internal/agenthealth/gauge"
	"github.com/bmjdotnet/teamster/internal/store"
)

// fakeSessionReader and fakeGaugeStore are minimal in-memory fakes —
// backfillModelFromSession's parameters (sessionModelReader/gaugeModelStore)
// are narrowed exactly so these don't need to implement the full
// store.Store/gauge.GaugeStore aggregate interfaces.
type fakeSessionReader struct {
	sess store.Session
	err  error
}

func (f *fakeSessionReader) GetSession(ctx context.Context, key store.SessionKey) (store.Session, error) {
	return f.sess, f.err
}

type fakeGaugeStore struct {
	row       gauge.GaugeRow
	found     bool
	getErr    error
	upserted  *gauge.GaugeRow
	upsertErr error
}

func (f *fakeGaugeStore) Get(ctx context.Context, key gauge.GaugeKey) (gauge.GaugeRow, bool, error) {
	return f.row, f.found, f.getErr
}

func (f *fakeGaugeStore) Upsert(ctx context.Context, row gauge.GaugeRow) error {
	f.upserted = &row
	return f.upsertErr
}

func TestBackfillModelFromSessionPatchesEmptyModel(t *testing.T) {
	gs := &fakeGaugeStore{row: gauge.GaugeRow{Host: "hub01", SessionID: "s1", TokensInTotal: 42}, found: true}
	st := &fakeSessionReader{sess: store.Session{Model: "claude-opus-4-6"}}

	backfillModelFromSession(context.Background(), st, gs, "hub01", "s1", "")

	if gs.upserted == nil {
		t.Fatal("expected Upsert to be called")
	}
	if gs.upserted.Model != "claude-opus-4-6" {
		t.Errorf("upserted.Model = %q, want %q", gs.upserted.Model, "claude-opus-4-6")
	}
	if gs.upserted.TokensInTotal != 42 {
		t.Errorf("upserted.TokensInTotal = %d, want 42 (every other field must survive untouched)", gs.upserted.TokensInTotal)
	}
}

func TestBackfillModelFromSessionNoOpWhenNoGaugeRow(t *testing.T) {
	gs := &fakeGaugeStore{found: false}
	st := &fakeSessionReader{sess: store.Session{Model: "claude-opus-4-6"}}

	backfillModelFromSession(context.Background(), st, gs, "hub01", "s1", "")

	if gs.upserted != nil {
		t.Error("expected no Upsert when no gauge row exists yet")
	}
}

func TestBackfillModelFromSessionNoOpWhenGaugeAlreadyHasModel(t *testing.T) {
	gs := &fakeGaugeStore{row: gauge.GaugeRow{Model: "claude-sonnet-5"}, found: true}
	st := &fakeSessionReader{sess: store.Session{Model: "claude-opus-4-6"}}

	backfillModelFromSession(context.Background(), st, gs, "hub01", "s1", "")

	if gs.upserted != nil {
		t.Error("expected no Upsert when the gauge row already has a model — never clobber a known value")
	}
}

func TestBackfillModelFromSessionNoOpWhenSessionHasNoModelEither(t *testing.T) {
	gs := &fakeGaugeStore{row: gauge.GaugeRow{}, found: true}
	st := &fakeSessionReader{sess: store.Session{Model: ""}}

	backfillModelFromSession(context.Background(), st, gs, "hub01", "s1", "")

	if gs.upserted != nil {
		t.Error("expected no Upsert when the sessions table has nothing to backfill from")
	}
}

func TestBackfillModelFromSessionNoOpOnSessionLookupError(t *testing.T) {
	gs := &fakeGaugeStore{row: gauge.GaugeRow{}, found: true}
	st := &fakeSessionReader{err: errors.New("not found")}

	backfillModelFromSession(context.Background(), st, gs, "hub01", "s1", "")

	if gs.upserted != nil {
		t.Error("expected no Upsert when the session lookup errors")
	}
}

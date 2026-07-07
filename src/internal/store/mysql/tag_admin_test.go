package mysql

import (
	"context"
	"errors"
	"testing"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// Phase 06: TagAdminStore — CLI/TUI tag-administration port. Reuses
// newTestStore/boundValues from tag_vocab_test.go (Stream A's shared harness).

func TestAddTagValue(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{Key: "widget", Category: "context", Cardinality: "multi"}); err != nil {
		t.Fatalf("DefineTag: %v", err)
	}
	if err := s.AddTagValue(ctx, "widget", "gadget", "a gadget"); err != nil {
		t.Fatalf("AddTagValue: %v", err)
	}
	values, err := s.TagValues(ctx, "widget")
	if err != nil {
		t.Fatalf("TagValues: %v", err)
	}
	found := false
	for _, v := range values {
		if v.Value == "gadget" {
			found = true
			if v.Description != "a gadget" {
				t.Errorf("description = %q, want %q", v.Description, "a gadget")
			}
			if v.Category != "context" || v.Cardinality != "multi" {
				t.Errorf("value did not inherit key's category/cardinality: got %q/%q", v.Category, v.Cardinality)
			}
		}
	}
	if !found {
		t.Errorf("gadget value not found under widget key")
	}

	if err := s.AddTagValue(ctx, "no-such-key", "x", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AddTagValue on missing key: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteTagKey(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{Key: "scratch", Category: "context", Cardinality: "multi", Values: []string{"a", "b"}}); err != nil {
		t.Fatalf("DefineTag: %v", err)
	}
	if err := s.TagEntity(ctx, "outcome", oid, "scratch", "a", "manual", ""); err != nil {
		t.Fatalf("TagEntity: %v", err)
	}

	n, err := s.DeleteTagKey(ctx, "scratch")
	if err != nil {
		t.Fatalf("DeleteTagKey: %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteTagKey rows = %d, want 2", n)
	}
	values, err := s.TagValues(ctx, "scratch")
	if err != nil {
		t.Fatalf("TagValues after delete: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("scratch key still has %d values after DeleteTagKey", len(values))
	}
	if bound := boundValues(t, s, "outcome", oid, "scratch"); len(bound) != 0 {
		t.Errorf("entity_tags binding survived DeleteTagKey: %v", bound)
	}
}

func TestDeleteTagValue(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{Key: "scratch2", Category: "context", Cardinality: "multi", Values: []string{"a"}}); err != nil {
		t.Fatalf("DefineTag: %v", err)
	}
	if err := s.TagEntity(ctx, "outcome", oid, "scratch2", "a", "manual", ""); err != nil {
		t.Fatalf("TagEntity: %v", err)
	}

	if err := s.DeleteTagValue(ctx, "scratch2", "a"); err != nil {
		t.Fatalf("DeleteTagValue: %v", err)
	}
	if bound := boundValues(t, s, "outcome", oid, "scratch2"); len(bound) != 0 {
		t.Errorf("entity_tags binding survived DeleteTagValue: %v", bound)
	}

	if err := s.DeleteTagValue(ctx, "scratch2", "a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteTagValue on already-deleted row: err = %v, want ErrNotFound", err)
	}
}

func TestUpdateTagDescription(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{Key: "described", Category: "context", Cardinality: "single", Values: []string{"a", "b"}}); err != nil {
		t.Fatalf("DefineTag: %v", err)
	}
	if err := s.UpdateTagDescription(ctx, "described", "a key-level rule"); err != nil {
		t.Fatalf("UpdateTagDescription: %v", err)
	}
	// No-op re-write of the same description must not be mistaken for not-found.
	if err := s.UpdateTagDescription(ctx, "described", "a key-level rule"); err != nil {
		t.Errorf("UpdateTagDescription no-op re-write: %v", err)
	}
	if err := s.UpdateTagDescription(ctx, "does-not-exist", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateTagDescription on missing key: err = %v, want ErrNotFound", err)
	}
}

func TestSetTagRequired(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{Key: "reqkey", Category: "context", Cardinality: "multi", Values: []string{"a", "b"}}); err != nil {
		t.Fatalf("DefineTag: %v", err)
	}
	if err := s.SetTagRequired(ctx, "reqkey", true); err != nil {
		t.Fatalf("SetTagRequired: %v", err)
	}
	keys, err := s.TagKeys(ctx)
	if err != nil {
		t.Fatalf("TagKeys: %v", err)
	}
	var got *wms.TagKeySummary
	for i := range keys {
		if keys[i].Key == "reqkey" {
			got = &keys[i]
		}
	}
	if got == nil {
		t.Fatalf("reqkey not found in TagKeys")
	}
	if !got.Required {
		t.Errorf("reqkey.Required = false after SetTagRequired(true)")
	}
	if got.ValueCount != 2 {
		t.Errorf("reqkey.ValueCount = %d, want 2", got.ValueCount)
	}
}

func TestTagValueDetail(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{Key: "detailed", Category: "context", Cardinality: "multi", Values: []string{"a"}, Description: "d"}); err != nil {
		t.Fatalf("DefineTag: %v", err)
	}
	if err := s.TagEntity(ctx, "outcome", oid, "detailed", "a", "manual", ""); err != nil {
		t.Fatalf("TagEntity: %v", err)
	}

	detail, err := s.TagValueDetail(ctx, "detailed", "a")
	if err != nil {
		t.Fatalf("TagValueDetail: %v", err)
	}
	if detail.EntityCount != 1 {
		t.Errorf("EntityCount = %d, want 1", detail.EntityCount)
	}
	if len(detail.BoundEntities) != 1 || detail.BoundEntities[0].EntityID != oid {
		t.Errorf("BoundEntities = %+v, want one ref to %s", detail.BoundEntities, oid)
	}
	if !detail.IsSeed {
		t.Errorf("IsSeed = false, want true (DefineTag seeds is_seed=1)")
	}

	if _, err := s.TagValueDetail(ctx, "detailed", "missing-value"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("TagValueDetail on missing value: err = %v, want ErrNotFound", err)
	}
}

func TestTagEntityCountsAndBindingCount(t *testing.T) {
	s, oid := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{Key: "counted", Category: "context", Cardinality: "multi", Values: []string{"a", "b"}}); err != nil {
		t.Fatalf("DefineTag: %v", err)
	}
	if err := s.TagEntity(ctx, "outcome", oid, "counted", "a", "manual", ""); err != nil {
		t.Fatalf("TagEntity a: %v", err)
	}
	if err := s.TagEntity(ctx, "outcome", oid, "counted", "b", "manual", ""); err != nil {
		t.Fatalf("TagEntity b: %v", err)
	}

	rows, err := s.TagEntityCounts(ctx)
	if err != nil {
		t.Fatalf("TagEntityCounts: %v", err)
	}
	total := 0
	for _, r := range rows {
		if r.TagKey == "counted" {
			total += int(r.Count)
			if r.EntityType != "outcome" {
				t.Errorf("EntityType = %q, want outcome", r.EntityType)
			}
		}
	}
	if total != 2 {
		t.Errorf("counted total bindings = %d, want 2", total)
	}

	n, err := s.TagBindingCount(ctx, "counted")
	if err != nil {
		t.Fatalf("TagBindingCount: %v", err)
	}
	if n != 2 {
		t.Errorf("TagBindingCount = %d, want 2", n)
	}
}

func TestSeedTagsProductsIntegrationKeys(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	specs := []wms.TagSpec{
		{Key: "seeded-key", Category: "context", Cardinality: "single", Description: "d1"},
	}
	if err := s.SeedTags(ctx, specs); err != nil {
		t.Fatalf("SeedTags: %v", err)
	}
	keys, err := s.TagKeys(ctx)
	if err != nil {
		t.Fatalf("TagKeys: %v", err)
	}
	found := false
	for _, k := range keys {
		if k.Key == "seeded-key" {
			found = true
		}
	}
	if !found {
		t.Errorf("seeded-key not present after SeedTags")
	}

	if err := s.SeedProductValues(ctx, []string{"teamster", "homelab"}); err != nil {
		t.Fatalf("SeedProductValues: %v", err)
	}
	products, err := s.TagValues(ctx, "product")
	if err != nil {
		t.Fatalf("TagValues(product): %v", err)
	}
	names := map[string]bool{}
	for _, p := range products {
		names[p.Value] = true
		if p.Value == "teamster" && p.IsSeed {
			t.Errorf("product value seeded via SeedProductValues should have is_seed=0")
		}
	}
	if !names["teamster"] || !names["homelab"] {
		t.Errorf("products = %v, want teamster and homelab present", names)
	}

	if err := s.SeedIntegrationKeys(ctx, []store.IntegrationKey{{Key: "github.repo", Description: "repo"}}); err != nil {
		t.Fatalf("SeedIntegrationKeys: %v", err)
	}
	ghValues, err := s.TagValues(ctx, "github.repo")
	if err != nil {
		t.Fatalf("TagValues(github.repo): %v", err)
	}
	if len(ghValues) != 1 || !ghValues[0].IsSeed {
		t.Errorf("github.repo values = %+v, want one is_seed stub row", ghValues)
	}
}

func TestUpdateTagConventions(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.DefineTag(ctx, wms.TagSpec{Key: "conv", Category: "context", Cardinality: "single"}); err != nil {
		t.Fatalf("DefineTag: %v", err)
	}
	if err := s.UpdateTagConventions(ctx, "conv", "outcome", "work-scope", "git", "propose"); err != nil {
		t.Fatalf("UpdateTagConventions: %v", err)
	}
	keys, err := s.TagKeys(ctx)
	if err != nil {
		t.Fatalf("TagKeys: %v", err)
	}
	for _, k := range keys {
		if k.Key != "conv" {
			continue
		}
		if k.Scope != "outcome" || k.ExclusionGroup != "work-scope" || k.AutoExtract != "git" || k.Interview != "propose" {
			t.Errorf("conventions = %+v, want scope=outcome exclusionGroup=work-scope autoExtract=git interview=propose", k)
		}
		return
	}
	t.Fatalf("conv key not found in TagKeys")
}

package wms

import "context"

// InheritedEntityTags is one tag binding in effect for an entity, annotated
// with where it actually lives: Inherited=false and Origin=the queried
// entity's own ID for a direct binding, Inherited=true and Origin=the parent
// Outcome's ID for one a WorkUnit inherits.
type InheritedEntityTags struct {
	EntityTag
	Inherited bool
	Origin    string
}

// ResolveEntityTags returns every tag binding in effect for entityType/
// entityID: its own direct bindings, plus — for a WorkUnit only — any tag key
// from its single parent Outcome that the WorkUnit does not itself bind (a
// WorkUnit's own binding for a key always shadows the Outcome's inherited row
// for that same key). Outcomes do not inherit further. entityID's existence
// is checked via GetWorkUnit/GetOutcome, so an unknown entity surfaces the
// store's not-found error rather than silently returning an empty list.
//
// This is the one place that walks a WorkUnit up to its Outcome for tag
// resolution. Close-out enforcement (warnMissingRequiredTags in
// internal/server, and the store's RequireTagsOnDone hard reject) and
// wms_getEntityTags both delegate here, so a required key set only on the
// parent Outcome reads as present everywhere a caller checks "does this
// entity have tag X" — previously warnMissingRequiredTags and the hard
// reject each called GetEntityTags directly and missed inheritance,
// producing a spurious close-out warning on nearly every WorkUnit.
func ResolveEntityTags(ctx context.Context, r Reader, entityType, entityID string) ([]InheritedEntityTags, error) {
	var outcomeID string
	if entityType == EntityWorkUnit {
		wu, err := r.GetWorkUnit(ctx, entityID)
		if err != nil {
			return nil, err
		}
		outcomeID = wu.OutcomeID
	} else {
		if _, err := r.GetOutcome(ctx, entityID); err != nil {
			return nil, err
		}
	}

	direct, err := r.GetEntityTags(ctx, entityType, entityID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(direct))
	out := make([]InheritedEntityTags, 0, len(direct))
	for _, et := range direct {
		seen[et.TagKey] = true
		out = append(out, InheritedEntityTags{EntityTag: et, Inherited: false, Origin: entityID})
	}
	if outcomeID == "" {
		return out, nil
	}
	parentTags, err := r.GetEntityTags(ctx, EntityOutcome, outcomeID)
	if err != nil {
		return nil, err
	}
	for _, et := range parentTags {
		if seen[et.TagKey] {
			continue // per-key override: the workunit's own binding wins
		}
		out = append(out, InheritedEntityTags{EntityTag: et, Inherited: true, Origin: outcomeID})
	}
	return out, nil
}

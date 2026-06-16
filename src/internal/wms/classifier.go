package wms

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Classifier classifies a work entity and applies tags describing how the work
// was done. The interface allows swapping the rule-based implementation for an
// LLM-based one without changing callers.
type Classifier interface {
	Classify(ctx context.Context, entityType, entityID string) (*ClassifierResult, error)
}

// ClassifierResult is the output of a Classify call.
type ClassifierResult struct {
	Applied []AppliedTag `json:"applied"`
	Skipped []SkippedTag `json:"skipped"`
}

// AppliedTag is a tag that was successfully applied to the entity.
type AppliedTag struct {
	TagKey     string  `json:"tag_key"`
	TagValue   string  `json:"tag_value"`
	Confidence float64 `json:"confidence"`
}

// SkippedTag is a tag that was not applied, with a reason.
type SkippedTag struct {
	TagKey string `json:"tag_key"`
	Reason string `json:"reason"`
}

// RuleClassifier is the rule-based Classifier implementation. It reads
// wms_intervals (kind='state') from the store and JSONL activity signals from
// disk via signalReader. The JSONL file must be accessible from the current process
// (hub deployment only — wms-mcp runs on the same host as hookd).
type RuleClassifier struct {
	store        Store
	signalReader SignalReader
	logFile      string
}

// NewRuleClassifier constructs a RuleClassifier. logFile is the path to the
// JSONL event log (config.LogFile).
func NewRuleClassifier(store Store, sr SignalReader, logFile string) *RuleClassifier {
	return &RuleClassifier{store: store, signalReader: sr, logFile: logFile}
}

// Classify classifies the entity and applies tags. It returns an empty Applied
// list with a skip reason when no JSONL activity is found (review fix #6).
func (c *RuleClassifier) Classify(ctx context.Context, entityType, entityID string) (*ClassifierResult, error) {
	result := &ClassifierResult{}

	// 0. Fetch the entity's existing tags once and collect the keys an operator
	// pinned manually. The classifier manages only its own source="classifier"
	// tags and must never overwrite a manual binding (proposal §13.6). Skip
	// granularity is the tag_key, not (key,value): a manual work-type=bug must
	// block a later classifier work-type=feature on the same key. Best-effort —
	// if the read fails, fall back to today's unconditional behavior rather than
	// suppressing all tags.
	manualKeys := map[string]bool{}
	if existing, err := c.store.GetEntityTags(ctx, entityType, entityID); err != nil {
		slog.Warn("classifier: read existing tags (proceeding unconditionally)", "entity", entityID, "err", err)
	} else {
		for _, t := range existing {
			if t.Source == "manual" {
				manualKeys[t.TagKey] = true
			}
		}
	}

	// 1. Fetch event records to build session windows and check for re-entry.
	records, err := c.store.ListEventRecords(ctx, entityType, entityID, 200)
	if err != nil {
		return nil, err
	}

	if len(records) == 0 {
		result.Skipped = append(result.Skipped, SkippedTag{TagKey: "*", Reason: "no event records found for this entity"})
		return result, nil
	}

	// 2. Derive session windows and detect re-entry (Rule 2 / phase=rework).
	var (
		sessions    []SessionWindow
		reEntry     bool
		windowStart = time.Now()
		windowEnd   time.Time
	)
	seenDoneOrReview := false

	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]

		if rec.StartedAt.Before(windowStart) {
			windowStart = rec.StartedAt
		}
		endT := rec.StartedAt // use startedAt as a fallback end for open records
		if rec.EndedAt != nil {
			endT = *rec.EndedAt
		}
		if endT.After(windowEnd) {
			windowEnd = endT
		}

		// Re-entry: transition from done/review back to active
		if seenDoneOrReview && rec.State == StatusActive {
			reEntry = true
		}
		if rec.State == StatusDone || rec.State == StatusReview {
			seenDoneOrReview = true
		}

		// Build session window from this record. The window owner may be the lead,
		// whose canonical agent_name is the empty string, so we gate only on a
		// present session id — NOT on a non-empty agent name. signals.go pairs a
		// JSONL row to a window on exact agent_name equality, and the lead emits a
		// "" JSONL agent_name (enrich.go sets _agent_name only for a non-empty
		// agent_type), so a "" window correctly matches the lead's "" rows. The
		// old `AgentName != ""` clause silently starved a lead-only (single-agent)
		// session of all work-type signal.
		if rec.SessionID != "" {
			prefix := rec.SessionID
			if len(prefix) > 12 {
				prefix = prefix[:12]
			}
			end := endT
			agentName := rec.AgentName
			if agentName != "" && agentName[0] != '@' {
				agentName = "@" + agentName
			}
			sessions = append(sessions, SessionWindow{
				SessionPrefix: prefix,
				AgentName:     agentName,
				Start:         rec.StartedAt,
				End:           end,
			})
		}
	}

	if windowEnd.IsZero() {
		windowEnd = time.Now().UTC()
	}

	// 3. Apply Rule 2 (phase=rework) independently — binary signal from event records.
	if reEntry {
		if err := c.applyTag(ctx, result, manualKeys, entityType, entityID, "phase", "rework", 1.0); err != nil {
			slog.Warn("classifier: apply phase=rework tag", "entity", entityID, "err", err)
		}
	}

	// 4. Collect JSONL signals for work-type rules.
	signals, err := c.signalReader.ReadSignals(ctx, sessions, c.logFile)
	if err != nil {
		slog.Warn("classifier: read signals", "entity", entityID, "err", err)
		result.Skipped = append(result.Skipped, SkippedTag{TagKey: "work-type", Reason: "signal read error: " + err.Error()})
		return result, nil
	}

	if signals.TotalEvents == 0 {
		result.Skipped = append(result.Skipped, SkippedTag{TagKey: "work-type", Reason: "no activity data found for this entity"})
		return result, nil
	}

	// 5. Work-type rules: priority order research > test > docs > feature, first match wins.
	workTypeKey := "work-type"
	applied := false

	// Rule 1: READ+GREP > 50% of tool tags → work-type=research
	totalTags := 0
	for _, n := range signals.ToolTagCounts {
		totalTags += n
	}
	if totalTags > 0 {
		readGrep := signals.ToolTagCounts["READ"] + signals.ToolTagCounts["GREP"]
		ratio := float64(readGrep) / float64(totalTags)
		if ratio > 0.50 {
			if err := c.applyTag(ctx, result, manualKeys, entityType, entityID, workTypeKey, "research", ratio); err != nil {
				slog.Warn("classifier: apply work-type=research", "entity", entityID, "err", err)
			}
			applied = true
		}
	}

	// Rule 3: test-matching bash commands > 60% of bash commands → work-type=test
	if !applied && len(signals.BashCommands) > 0 {
		testCount := 0
		for _, cmd := range signals.BashCommands {
			if strings.Contains(cmd, "test") {
				testCount++
			}
		}
		ratio := float64(testCount) / float64(len(signals.BashCommands))
		if ratio > 0.60 {
			if err := c.applyTag(ctx, result, manualKeys, entityType, entityID, workTypeKey, "test", ratio); err != nil {
				slog.Warn("classifier: apply work-type=test", "entity", entityID, "err", err)
			}
			applied = true
		}
	}

	// Rule 4: .md files > 40% of files touched → work-type=docs
	if !applied && len(signals.FilesTouched) > 0 {
		totalFiles := 0
		for _, n := range signals.FilesTouched {
			totalFiles += n
		}
		if totalFiles > 0 {
			mdCount := signals.FilesTouched[".md"]
			ratio := float64(mdCount) / float64(totalFiles)
			if ratio > 0.40 {
				if err := c.applyTag(ctx, result, manualKeys, entityType, entityID, workTypeKey, "docs", ratio); err != nil {
					slog.Warn("classifier: apply work-type=docs", "entity", entityID, "err", err)
				}
				applied = true
			}
		}
	}

	// Rule 5: fallback → work-type=feature (only when there IS activity data)
	if !applied {
		if err := c.applyTag(ctx, result, manualKeys, entityType, entityID, workTypeKey, "feature", 0.5); err != nil {
			slog.Warn("classifier: apply work-type=feature", "entity", entityID, "err", err)
		}
	}

	return result, nil
}

// applyTag applies a tag to the entity via the store and records it in result.
// It uses source="classifier" — the store's TagEntity is idempotent; re-tagging
// with the same (key, value) only refreshes the applied_at timestamp.
// If tagKey is present in manualKeys (an operator set it with source="manual"),
// the tag is skipped and recorded in result.Skipped — the classifier manages
// only its own tags and never overwrites a manual binding. A prior
// source="classifier" binding does NOT pin the key, so re-classification
// (including a changed value on a second pass) stays allowed.
func (c *RuleClassifier) applyTag(ctx context.Context, result *ClassifierResult, manualKeys map[string]bool, entityType, entityID, tagKey, tagValue string, confidence float64) error {
	if manualKeys[tagKey] {
		result.Skipped = append(result.Skipped, SkippedTag{TagKey: tagKey, Reason: "manual tag present; classifier deferring"})
		return nil
	}
	if err := c.store.TagEntity(ctx, entityType, entityID, tagKey, tagValue, "classifier", ""); err != nil {
		return err
	}
	result.Applied = append(result.Applied, AppliedTag{
		TagKey:     tagKey,
		TagValue:   tagValue,
		Confidence: confidence,
	})
	return nil
}

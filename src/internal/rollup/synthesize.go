package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/bmjdotnet/teamster/internal/store"
)

const (
	synthesisMethod = "synthesized_outcome"
	skipMethod      = "sweep_skipped"
)

// OrphanSession is one session with unallocated cost and no setFocus anywhere in
// its transcript — the Objective 2 target set. The LLM orchestrator reads these
// sessions' transcript heads and synthesizes an outcome+tags for each.
type OrphanSession struct {
	SessionID string
	Host      string
	Username  string
	MsgCount  int
	CostUSD   float64
}

// OrphanSessions returns sessions that have method='unallocated' rows AND no
// setFocus in any thread's transcript — the no-focus bucket that only LLM
// synthesis can attribute. Host-scoped: only sessions local to opts are
// returned, so the caller only sees sessions whose transcripts it can read.
func (r *Runner) OrphanSessions(ctx context.Context, opts RecoverOptions) ([]OrphanSession, error) {
	src := opts.Source
	if src == nil {
		src = defaultTranscriptSource
	}

	sessions, err := r.rec.UnallocatedSessions(ctx, store.UnallocatedFilter{})
	if err != nil {
		return nil, fmt.Errorf("list unallocated sessions: %w", err)
	}

	var orphans []OrphanSession
	for _, s := range sessions {
		if opts.scoped() && (s.Host != opts.Host || !localToUser(s.Username, opts.User)) {
			continue
		}

		tl, err := src(s.SessionID, opts.ProjectsDir)
		if err != nil {
			r.log.Warn("orphan-check: timeline build failed; skipping",
				"session_id", s.SessionID, "error", err)
			continue
		}

		// A session is an orphan iff its timeline has NO setFocus on ANY thread.
		if len(tl.Events) > 0 {
			continue
		}

		orphans = append(orphans, OrphanSession{
			SessionID: s.SessionID, Host: s.Host, Username: s.Username,
			MsgCount: int(s.MessageCount), CostUSD: s.CostUSD,
		})
	}
	return orphans, nil
}

// SynthesisMapping is one session→outcome mapping produced by the LLM orchestrator
// and consumed by the deterministic --synthesize-focus mode. The orchestrator
// creates the Outcome in WMS, tags it, and writes this mapping to a JSON file;
// the Go mode mechanically applies it to usage_attribution.
type SynthesisMapping struct {
	SessionID       string `json:"session_id"`
	EntityType      string `json:"entity_type"`
	EntityID        string `json:"entity_id"`
	Confidence      string `json:"confidence"`
	EvidenceExcerpt string `json:"evidence_excerpt"`
}

// SynthesizeOptions configures a synthesis pass.
type SynthesizeOptions struct {
	MappingFile string
	ProjectsDir string
	Host        string
	User        string
	DryRun      bool
	Source      FocusTimelineSource
}

// SynthesizeStats summarizes one synthesis pass.
type SynthesizeStats struct {
	MappingsLoaded int
	Sessions       int
	Examined       int
	Recovered      int
	Skipped        int
}

// LoadMappings reads the session→outcome mapping file produced by the LLM
// orchestrator.
func LoadMappings(path string) ([]SynthesisMapping, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mapping file: %w", err)
	}
	var mappings []SynthesisMapping
	if err := json.Unmarshal(data, &mappings); err != nil {
		return nil, fmt.Errorf("parse mapping file: %w", err)
	}
	return mappings, nil
}

// SynthesizeFocus consumes a session→outcome mapping file (produced by the LLM
// orchestrator) and re-attributes each mapped session's method='unallocated'
// messages to the synthesized outcome with method='synthesized_outcome'. This
// is the deterministic write path — the LLM judgment lives entirely in the
// orchestrator that produced the mapping file; this mode is mechanical.
//
// CONSERVATION: ApplyRecovery's in-place UPDATE is scoped to
// method='unallocated'; one row/message, weight 1.0.
// AUDITABLE: synthesis_evidence records the mapping source, confidence, and
// evidence excerpt per recovered message.
func (r *Runner) SynthesizeFocus(ctx context.Context, opts SynthesizeOptions) (SynthesizeStats, error) {
	mappings, err := LoadMappings(opts.MappingFile)
	if err != nil {
		return SynthesizeStats{}, err
	}

	src := opts.Source
	if src == nil {
		src = defaultTranscriptSource
	}

	var stats SynthesizeStats
	stats.MappingsLoaded = len(mappings)

	bySession := map[string]*SynthesisMapping{}
	for i := range mappings {
		m := &mappings[i]
		if m.SessionID == "" || m.EntityType == "" || m.EntityID == "" {
			r.log.Warn("synthesize: skipping incomplete mapping",
				"session_id", m.SessionID, "entity_type", m.EntityType, "entity_id", m.EntityID)
			stats.Skipped++
			continue
		}
		if _, dup := bySession[m.SessionID]; dup {
			r.log.Warn("synthesize: duplicate session_id in mapping file; last entry wins",
				"session_id", m.SessionID)
		}
		bySession[m.SessionID] = m
	}

	sessions, err := r.rec.UnallocatedSessions(ctx, store.UnallocatedFilter{})
	if err != nil {
		return stats, fmt.Errorf("list unallocated sessions: %w", err)
	}

	for _, s := range sessions {
		mapping, ok := bySession[s.SessionID]
		if !ok {
			continue
		}

		// Handle skip entries: entity_type=="skip" means the LLM examined this
		// session and determined it can't be attributed. Mark it so future
		// sweeps don't re-examine it.
		if mapping.EntityType == "skip" {
			stats.Sessions++
			msgs, skipErr := r.rec.ReclaimableMessages(ctx, s.SessionID, "", false, []string{"unallocated"})
			if skipErr != nil {
				return stats, fmt.Errorf("session %s: list unallocated messages: %w", s.SessionID, skipErr)
			}
			stats.Examined += len(msgs)
			if opts.DryRun {
				stats.Skipped += len(msgs)
				r.log.Info("synthesize (dry-run): would mark as skipped",
					"session_id", s.SessionID, "count", len(msgs), "reason", mapping.EvidenceExcerpt)
				continue
			}
			if len(msgs) > 0 {
				msgIDs := make([]string, len(msgs))
				for i, m := range msgs {
					msgIDs[i] = m.MessageID
				}
				if err := r.rec.ApplyRecovery(ctx, store.RecoveryBatch{
					Strategy:   "skip",
					Method:     skipMethod,
					MessageIDs: msgIDs,
					Evidence: map[string]any{
						"session_id":       s.SessionID,
						"confidence":       "skip",
						"evidence_excerpt": mapping.EvidenceExcerpt,
						"mapping_source":   opts.MappingFile,
					},
				}); err != nil {
					return stats, fmt.Errorf("session %s: apply skip: %w", s.SessionID, err)
				}
			}
			stats.Skipped += len(msgs)
			continue
		}

		// M-1: verify the session actually has NO setFocus events. If the
		// orchestrator mistakenly mapped a session that DID have focus, skip it
		// — that session belongs to RecoverFocus/RecoverWarmup, not synthesis.
		tl, tlErr := src(s.SessionID, opts.ProjectsDir)
		if tlErr != nil {
			r.log.Warn("synthesize: timeline check failed; skipping session",
				"session_id", s.SessionID, "error", tlErr)
			stats.Skipped++
			continue
		}
		if len(tl.Events) > 0 {
			r.log.Warn("synthesize: mapped session has setFocus events; skipping (belongs to recover-focus/warmup)",
				"session_id", s.SessionID, "focus_threads", len(tl.Events))
			stats.Skipped++
			continue
		}

		stats.Sessions++
		msgs, err := r.rec.ReclaimableMessages(ctx, s.SessionID, "", false, []string{"unallocated"})
		if err != nil {
			return stats, fmt.Errorf("session %s: list unallocated messages: %w", s.SessionID, err)
		}
		stats.Examined += len(msgs)

		if opts.DryRun {
			stats.Recovered += len(msgs)
			r.log.Info("synthesize (dry-run): would re-attribute",
				"session_id", s.SessionID, "count", len(msgs),
				"to_entity_type", mapping.EntityType, "to_entity_id", mapping.EntityID,
				"confidence", mapping.Confidence)
			continue
		}
		if len(msgs) == 0 {
			continue
		}

		msgIDs := make([]string, len(msgs))
		for i, m := range msgs {
			msgIDs[i] = m.MessageID
		}
		if err := r.rec.ApplyRecovery(ctx, store.RecoveryBatch{
			Strategy:   "synthesis",
			Method:     synthesisMethod,
			MessageIDs: msgIDs,
			Entity:     store.EntityRef{EntityType: mapping.EntityType, EntityID: mapping.EntityID},
			Evidence: map[string]any{
				"session_id":       s.SessionID,
				"confidence":       mapping.Confidence,
				"evidence_excerpt": mapping.EvidenceExcerpt,
				"mapping_source":   opts.MappingFile,
			},
		}); err != nil {
			return stats, fmt.Errorf("session %s: apply synthesis: %w", s.SessionID, err)
		}
		stats.Recovered += len(msgIDs)
	}

	if !opts.DryRun && stats.Recovered > 0 {
		if err := r.alloc.BuildCostRollup(ctx); err != nil {
			return stats, fmt.Errorf("synthesize: rebuild cost_rollup: %w", err)
		}
		if _, err := r.alloc.AssembleIntervalCost(ctx); err != nil {
			return stats, fmt.Errorf("synthesize: reassemble interval cost: %w", err)
		}
	}

	r.log.Info("synthesize-focus pass complete",
		"mappings_loaded", stats.MappingsLoaded, "sessions", stats.Sessions,
		"examined", stats.Examined, "recovered", stats.Recovered,
		"skipped", stats.Skipped, "dry_run", opts.DryRun)
	return stats, nil
}

// Unsynthesize reverses a synthesis pass: deletes every
// method='synthesized_outcome' attribution and its evidence, returning those
// messages to the unallocated bucket.
func (r *Runner) Unsynthesize(ctx context.Context) (int, error) {
	n, err := r.rec.UncoverRecovery(ctx, "synthesis")
	return int(n), err
}

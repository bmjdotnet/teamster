package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
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
// synthesis can attribute. It is host-scoped: only sessions local to opts are
// returned, so the caller only sees sessions whose transcripts it can read.
func (r *Runner) OrphanSessions(ctx context.Context, opts RecoverOptions) ([]OrphanSession, error) {
	src := opts.Source
	if src == nil {
		src = defaultTranscriptSource
	}

	sessions, err := r.unallocatedSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list unallocated sessions: %w", err)
	}

	var orphans []OrphanSession
	for _, s := range sessions {
		if opts.scoped() && (s.host != opts.Host || !localToUser(s.username, opts.User)) {
			continue
		}

		tl, err := src(s.sessionID, opts.ProjectsDir)
		if err != nil {
			r.log.Warn("orphan-check: timeline build failed; skipping",
				"session_id", s.sessionID, "error", err)
			continue
		}

		// A session is an orphan iff its timeline has NO setFocus on ANY thread.
		if len(tl.Events) > 0 {
			continue
		}

		// Compute cost for this session's unallocated messages.
		var costUSD float64
		err = r.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(t.cost_usd), 0)
			FROM usage_attribution ua
			JOIN token_ledger t ON t.message_id = ua.message_id
			WHERE ua.method = 'unallocated'
			  AND t.session_id = ? AND t.host = ? AND t.username = ?`,
			s.sessionID, s.host, s.username).Scan(&costUSD)
		if err != nil {
			return nil, fmt.Errorf("session %s: sum cost: %w", s.sessionID, err)
		}

		orphans = append(orphans, OrphanSession{
			SessionID: s.sessionID,
			Host:      s.host,
			Username:  s.username,
			MsgCount:  s.msgCount,
			CostUSD:   costUSD,
		})
	}
	return orphans, nil
}

// SynthesisMapping is one session→outcome mapping produced by the LLM orchestrator
// and consumed by the deterministic --synthesize-focus mode. The orchestrator
// creates the Outcome in WMS, tags it, and writes this mapping to a JSON file;
// the Go mode mechanically applies it to usage_attribution.
type SynthesisMapping struct {
	SessionID      string `json:"session_id"`
	EntityType     string `json:"entity_type"`
	EntityID       string `json:"entity_id"`
	Confidence     string `json:"confidence"`
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
// messages to the synthesized outcome with method='synthesized_outcome'.
//
// This is the deterministic write path — the LLM judgment lives entirely in the
// orchestrator that produced the mapping file; this mode is mechanical. It honors
// the same contracts as RecoverFocus:
//
// CONSERVATION: in-place UPDATE scoped to method='unallocated'; one row/message,
// weight 1.0; SUM(cost_facts) unchanged.
//
// REVERSIBLE: Unsynthesize deletes by method + removes evidence.
//
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
	now := time.Now().UTC()

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

	sessions, err := r.unallocatedSessions(ctx)
	if err != nil {
		return stats, fmt.Errorf("list unallocated sessions: %w", err)
	}

	for _, s := range sessions {
		mapping, ok := bySession[s.sessionID]
		if !ok {
			continue
		}

		// Handle skip entries: entity_type=="skip" means the LLM examined
		// this session and determined it can't be attributed. Mark it so
		// future sweeps don't re-examine it.
		if mapping.EntityType == "skip" {
			stats.Sessions++
			msgs, skipErr := r.unallocatedMessages(ctx, s)
			if skipErr != nil {
				return stats, fmt.Errorf("session %s: list unallocated messages: %w", s.sessionID, skipErr)
			}
			for _, m := range msgs {
				stats.Examined++
				if opts.DryRun {
					stats.Skipped++
					r.log.Info("synthesize (dry-run): would mark as skipped",
						"message_id", m.messageID, "session_id", s.sessionID,
						"reason", mapping.EvidenceExcerpt)
					continue
				}
				if err := r.applySkip(ctx, m, s.sessionID, mapping, opts.MappingFile, now); err != nil {
					return stats, fmt.Errorf("session %s message %s: apply skip: %w", s.sessionID, m.messageID, err)
				}
				stats.Skipped++
			}
			continue
		}

		// M-1: verify the session actually has NO setFocus events. If the
		// orchestrator mistakenly mapped a session that DID have focus, skip it —
		// that session belongs to RecoverFocus/RecoverWarmup, not synthesis.
		tl, tlErr := src(s.sessionID, opts.ProjectsDir)
		if tlErr != nil {
			r.log.Warn("synthesize: timeline check failed; skipping session",
				"session_id", s.sessionID, "error", tlErr)
			stats.Skipped++
			continue
		}
		if len(tl.Events) > 0 {
			r.log.Warn("synthesize: mapped session has setFocus events; skipping (belongs to recover-focus/warmup)",
				"session_id", s.sessionID, "focus_threads", len(tl.Events))
			stats.Skipped++
			continue
		}

		stats.Sessions++

		msgs, err := r.unallocatedMessages(ctx, s)
		if err != nil {
			return stats, fmt.Errorf("session %s: list unallocated messages: %w", s.sessionID, err)
		}

		for _, m := range msgs {
			stats.Examined++

			if opts.DryRun {
				stats.Recovered++
				r.log.Info("synthesize (dry-run): would re-attribute",
					"message_id", m.messageID, "session_id", s.sessionID,
					"to_entity_type", mapping.EntityType, "to_entity_id", mapping.EntityID,
					"confidence", mapping.Confidence)
				continue
			}

			if err := r.applySynthesis(ctx, m, s.sessionID, mapping, opts.MappingFile, now); err != nil {
				return stats, fmt.Errorf("session %s message %s: apply synthesis: %w", s.sessionID, m.messageID, err)
			}
			stats.Recovered++
		}
	}

	if !opts.DryRun && stats.Recovered > 0 {
		rows, err := r.BuildCostRollup(ctx)
		if err != nil {
			return stats, fmt.Errorf("synthesize: rebuild cost_rollup: %w", err)
		}
		intervals, err := r.AssembleIntervalCost(ctx)
		if err != nil {
			return stats, fmt.Errorf("synthesize: reassemble interval cost: %w", err)
		}
		r.log.Info("synthesize rebuilt aggregates", "rollup_rows", rows, "intervals_costed", intervals)
	}

	r.log.Info("synthesize-focus pass complete",
		"mappings_loaded", stats.MappingsLoaded, "sessions", stats.Sessions,
		"examined", stats.Examined, "recovered", stats.Recovered,
		"skipped", stats.Skipped, "dry_run", opts.DryRun)
	return stats, nil
}

// applySynthesis re-attributes one unallocated message and records provenance.
func (r *Runner) applySynthesis(ctx context.Context, m unallocatedMsg, sessionID string, mapping *SynthesisMapping, mappingFile string, now time.Time) error {
	var intervalID uint64
	if id, ok, err := r.intervalAt(ctx, mapping.EntityType, mapping.EntityID, m.ts); err != nil {
		return fmt.Errorf("intervalAt: %w", err)
	} else if ok {
		intervalID = id
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx, `
		UPDATE usage_attribution
		SET entity_type = ?, entity_id = ?, method = ?, interval_id = ?, computed_at = ?
		WHERE message_id = ? AND method = 'unallocated'`,
		mapping.EntityType, mapping.EntityID, synthesisMethod, intervalID, now, m.messageID)
	if err != nil {
		return fmt.Errorf("update attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO synthesis_evidence
			(message_id, entity_type, entity_id, session_id, confidence, evidence_excerpt, mapping_source, recovered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			entity_type      = VALUES(entity_type),
			entity_id        = VALUES(entity_id),
			session_id       = VALUES(session_id),
			confidence       = VALUES(confidence),
			evidence_excerpt = VALUES(evidence_excerpt),
			mapping_source   = VALUES(mapping_source),
			recovered_at     = VALUES(recovered_at)`,
		m.messageID, mapping.EntityType, mapping.EntityID, sessionID,
		mapping.Confidence, mapping.EvidenceExcerpt, mappingFile, now); err != nil {
		return fmt.Errorf("insert synthesis evidence: %w", err)
	}

	return tx.Commit()
}

// applySkip marks one unallocated message as permanently skipped and records the reason.
func (r *Runner) applySkip(ctx context.Context, m unallocatedMsg, sessionID string, mapping *SynthesisMapping, mappingFile string, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx, `
		UPDATE usage_attribution
		SET method = ?, computed_at = ?
		WHERE message_id = ? AND method = 'unallocated'`,
		skipMethod, now, m.messageID)
	if err != nil {
		return fmt.Errorf("update attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO synthesis_evidence
			(message_id, entity_type, entity_id, session_id, confidence, evidence_excerpt, mapping_source, recovered_at)
		VALUES (?, 'skip', 'SKIP', ?, 'skip', ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			entity_type      = VALUES(entity_type),
			entity_id        = VALUES(entity_id),
			confidence       = VALUES(confidence),
			evidence_excerpt = VALUES(evidence_excerpt),
			mapping_source   = VALUES(mapping_source),
			recovered_at     = VALUES(recovered_at)`,
		m.messageID, sessionID,
		mapping.EvidenceExcerpt, mappingFile, now); err != nil {
		return fmt.Errorf("insert skip evidence: %w", err)
	}

	return tx.Commit()
}

// Unsynthesize reverses a synthesis pass: deletes every method='synthesized_outcome'
// attribution and its evidence, returning those messages to the unallocated bucket.
func (r *Runner) Unsynthesize(ctx context.Context) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		DELETE se FROM synthesis_evidence se
		JOIN usage_attribution ua ON ua.message_id = se.message_id
		WHERE ua.method = ?`, synthesisMethod); err != nil {
		return 0, fmt.Errorf("delete synthesis evidence: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM usage_attribution WHERE method = ?`, synthesisMethod)
	if err != nil {
		return 0, fmt.Errorf("delete synthesized attribution: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

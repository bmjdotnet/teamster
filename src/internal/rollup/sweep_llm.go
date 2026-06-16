package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/llm"
	"github.com/bmjdotnet/teamster/internal/transcript"
)

const (
	maxLLMSessions  = 10
	llmPhaseTimeout = 5 * time.Minute
	sweepOutcomeID  = "out-sweep-nightly"
)

// SweepLLMStats summarizes one LLM sweep pass.
type SweepLLMStats struct {
	OrphansSynthesized int
	GapsSynthesized    int
	Skipped            int
	Errors             int
}

// SweepLLM runs the LLM-assisted attribution passes: orphan synthesis (no-focus
// sessions) and gap LLM fallback (gap threads that RecoverGaps couldn't resolve).
// It creates WMS outcomes for each synthesized session, writes a mapping file,
// and feeds it to SynthesizeFocus. Rate-limited to maxLLMSessions per run.
// Uses claude --print (Claude Code headless mode) — no ANTHROPIC_API_KEY needed.
func (r *Runner) SweepLLM(ctx context.Context, opts RecoverOptions) (SweepLLMStats, error) {
	llmCtx, cancel := context.WithTimeout(ctx, llmPhaseTimeout)
	defer cancel()

	tagVocab, err := r.loadTagVocab(ctx)
	if err != nil {
		r.log.Warn("sweep-llm: failed to load tag vocabulary; proceeding without", "error", err)
	}

	r.ensureSweepOutcome(ctx)

	var stats SweepLLMStats
	var mappings []SynthesisMapping

	orphanMappings, orphanStats := r.synthesizeOrphans(llmCtx, opts, tagVocab)
	mappings = append(mappings, orphanMappings...)
	stats.OrphansSynthesized = orphanStats.synthesized
	stats.Skipped += orphanStats.skipped
	stats.Errors += orphanStats.errors

	remaining := maxLLMSessions - len(orphanMappings)
	if remaining > 0 {
		gapMappings, gapStats := r.synthesizeGapFallback(llmCtx, opts, tagVocab, remaining)
		mappings = append(mappings, gapMappings...)
		stats.GapsSynthesized = gapStats.synthesized
		stats.Skipped += gapStats.skipped
		stats.Errors += gapStats.errors
	}

	if len(mappings) > 0 && !opts.DryRun {
		mapFile, err := writeTempMappings(mappings)
		if err != nil {
			return stats, fmt.Errorf("write mapping file: %w", err)
		}
		defer os.Remove(mapFile) //nolint:errcheck

		synthStats, err := r.SynthesizeFocus(ctx, SynthesizeOptions{
			MappingFile: mapFile,
			ProjectsDir: opts.ProjectsDir,
			Host:        opts.Host,
			User:        opts.User,
			Source:      opts.Source,
		})
		if err != nil {
			return stats, fmt.Errorf("synthesize-focus from LLM mappings: %w", err)
		}
		r.log.Info("sweep-llm: synthesize-focus applied",
			"recovered", synthStats.Recovered, "skipped", synthStats.Skipped)
	}

	r.log.Info("sweep-llm complete",
		"orphans_synthesized", stats.OrphansSynthesized,
		"gaps_synthesized", stats.GapsSynthesized,
		"skipped", stats.Skipped,
		"errors", stats.Errors,
		"dry_run", opts.DryRun)
	return stats, nil
}

type synthesisPassStats struct {
	synthesized int
	skipped     int
	errors      int
}

func (r *Runner) synthesizeOrphans(ctx context.Context, opts RecoverOptions, tagVocab string) ([]SynthesisMapping, synthesisPassStats) {
	var stats synthesisPassStats
	var mappings []SynthesisMapping

	orphans, err := r.OrphanSessions(ctx, opts)
	if err != nil {
		r.log.Error("sweep-llm: list orphan sessions failed", "error", err)
		stats.errors++
		return nil, stats
	}

	sort.Slice(orphans, func(i, j int) bool {
		return orphans[i].CostUSD > orphans[j].CostUSD
	})

	limit := maxLLMSessions
	if len(orphans) < limit {
		limit = len(orphans)
	}

	for _, orphan := range orphans[:limit] {
		if ctx.Err() != nil {
			r.log.Warn("sweep-llm: timeout reached; stopping orphan synthesis")
			break
		}

		mapping, err := r.synthesizeSession(ctx, orphan.SessionID, opts.ProjectsDir, tagVocab)
		if err != nil {
			r.log.Warn("sweep-llm: orphan synthesis failed; skipping",
				"session_id", orphan.SessionID, "cost_usd", orphan.CostUSD, "error", err)
			stats.errors++
			continue
		}

		if !opts.DryRun {
			if err := r.createSynthesizedOutcome(ctx, mapping); err != nil {
				r.log.Warn("sweep-llm: create outcome failed; skipping",
					"session_id", orphan.SessionID, "outcome_id", mapping.outcomeID, "error", err)
				stats.errors++
				continue
			}
		} else {
			r.log.Info("sweep-llm (dry-run): would synthesize orphan",
				"session_id", orphan.SessionID, "outcome_id", mapping.outcomeID,
				"title", mapping.title, "confidence", mapping.confidence,
				"cost_usd", orphan.CostUSD)
		}

		mappings = append(mappings, SynthesisMapping{
			SessionID:       orphan.SessionID,
			EntityType:      "outcome",
			EntityID:        mapping.outcomeID,
			Confidence:      mapping.confidence,
			EvidenceExcerpt: mapping.evidenceExcerpt,
		})
		stats.synthesized++
	}

	return mappings, stats
}

func (r *Runner) synthesizeGapFallback(ctx context.Context, opts RecoverOptions, tagVocab string, remaining int) ([]SynthesisMapping, synthesisPassStats) {
	var stats synthesisPassStats
	var mappings []SynthesisMapping

	threads, err := r.unresolvedGapThreads(ctx)
	if err != nil {
		r.log.Error("sweep-llm: list unresolved gap threads failed", "error", err)
		stats.errors++
		return nil, stats
	}

	sessionsSeen := map[string]bool{}
	count := 0

	for _, gt := range threads {
		if count >= remaining || ctx.Err() != nil {
			break
		}
		if sessionsSeen[gt.sessionID] {
			continue
		}
		sessionsSeen[gt.sessionID] = true

		mapping, err := r.synthesizeSession(ctx, gt.sessionID, opts.ProjectsDir, tagVocab)
		if err != nil {
			r.log.Warn("sweep-llm: gap synthesis failed; skipping",
				"session_id", gt.sessionID, "error", err)
			stats.errors++
			continue
		}

		if !opts.DryRun {
			if err := r.createSynthesizedOutcome(ctx, mapping); err != nil {
				r.log.Warn("sweep-llm: create gap outcome failed; skipping",
					"session_id", gt.sessionID, "outcome_id", mapping.outcomeID, "error", err)
				stats.errors++
				continue
			}
		} else {
			r.log.Info("sweep-llm (dry-run): would synthesize gap session",
				"session_id", gt.sessionID, "outcome_id", mapping.outcomeID,
				"title", mapping.title, "confidence", mapping.confidence)
		}

		mappings = append(mappings, SynthesisMapping{
			SessionID:       gt.sessionID,
			EntityType:      "outcome",
			EntityID:        mapping.outcomeID,
			Confidence:      mapping.confidence,
			EvidenceExcerpt: mapping.evidenceExcerpt,
		})
		stats.synthesized++
		count++
	}

	return mappings, stats
}

type synthesizedMapping struct {
	outcomeID       string
	title           string
	description     string
	product         string
	workType        string
	phase           string
	featureOrBug    string
	featureBugSlug  string
	component       string
	priority        string
	confidence      string
	evidenceExcerpt string
}

func (r *Runner) synthesizeSession(ctx context.Context, sessionID, projectsDir, tagVocab string) (*synthesizedMapping, error) {
	lines, err := transcript.ReadWindow(sessionID, projectsDir, time.Time{}, time.Now().Add(24*time.Hour), 200)
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty transcript for session %s", sessionID)
	}

	resp, err := llm.SynthesizeOutcome(ctx, llm.SynthesisRequest{
		SessionID:  sessionID,
		Transcript: lines,
		TagVocab:   tagVocab,
	})
	if err != nil {
		return nil, fmt.Errorf("LLM synthesis: %w", err)
	}

	return &synthesizedMapping{
		outcomeID:       resp.OutcomeID,
		title:           resp.Title,
		description:     resp.Description,
		product:         resp.Product,
		workType:        resp.WorkType,
		phase:           resp.Phase,
		featureOrBug:    resp.FeatureOrBug,
		featureBugSlug:  resp.FeatureBugSlug,
		component:       resp.Component,
		priority:        resp.Priority,
		confidence:      resp.Confidence,
		evidenceExcerpt: resp.EvidenceExcerpt,
	}, nil
}

func (r *Runner) createSynthesizedOutcome(ctx context.Context, m *synthesizedMapping) error {
	now := time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT IGNORE INTO outcomes (id, title, description, status, created_at, updated_at)
		VALUES (?, ?, ?, 'active', ?, ?)`,
		m.outcomeID, m.title, m.description, now, now)
	if err != nil {
		return fmt.Errorf("insert outcome: %w", err)
	}

	tags := []struct{ key, value string }{
		{"source", "synthesized"},
	}
	if m.product != "" {
		tags = append(tags, struct{ key, value string }{"product", m.product})
	}
	if m.workType != "" {
		tags = append(tags, struct{ key, value string }{"work-type", m.workType})
	}
	if m.featureOrBug == "feature" && m.featureBugSlug != "" {
		tags = append(tags, struct{ key, value string }{"feature", m.featureBugSlug})
	} else if m.featureOrBug == "bug" && m.featureBugSlug != "" {
		tags = append(tags, struct{ key, value string }{"bug", m.featureBugSlug})
	}
	if m.component != "" {
		tags = append(tags, struct{ key, value string }{"component", m.component})
	}
	if m.priority != "" {
		tags = append(tags, struct{ key, value string }{"priority", m.priority})
	}
	if m.phase != "" {
		tags = append(tags, struct{ key, value string }{"phase", m.phase})
	}

	for _, tag := range tags {
		if err := r.applyTag(ctx, "outcome", m.outcomeID, tag.key, tag.value, "sweep-llm"); err != nil {
			r.log.Warn("sweep-llm: tag failed (non-fatal)",
				"outcome_id", m.outcomeID, "key", tag.key, "value", tag.value, "error", err)
		}
	}

	return nil
}

// applyTag applies a single tag to an entity via direct SQL (the rollup binary
// doesn't have the store.Store interface — it operates on raw *sql.DB).
func (r *Runner) applyTag(ctx context.Context, entityType, entityID, tagKey, tagValue, source string) error {
	now := time.Now().UTC()

	var tagID int64
	err := r.db.QueryRowContext(ctx,
		`SELECT id FROM tags WHERE tag_key = ? AND tag_value = ?`,
		tagKey, tagValue).Scan(&tagID)
	if err == sql.ErrNoRows {
		res, err := r.db.ExecContext(ctx,
			`INSERT INTO tags (tag_key, tag_value, is_seed, category, cardinality) VALUES (?, ?, 0, 'context', 'single')`,
			tagKey, tagValue)
		if err != nil {
			return fmt.Errorf("create tag %s:%s: %w", tagKey, tagValue, err)
		}
		tagID, _ = res.LastInsertId()
	} else if err != nil {
		return fmt.Errorf("lookup tag %s:%s: %w", tagKey, tagValue, err)
	}

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE source = VALUES(source), applied_at = VALUES(applied_at)`,
		entityType, entityID, tagID, source, now)
	return err
}

func (r *Runner) loadTagVocab(ctx context.Context) (string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT tag_key, tag_value FROM tags WHERE category = 'context' AND tag_value != '' ORDER BY tag_key, tag_value`)
	if err != nil {
		return "", err
	}
	defer rows.Close() //nolint:errcheck

	byKey := map[string][]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return "", err
		}
		byKey[k] = append(byKey[k], v)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	var sb strings.Builder
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s: %s\n", k, strings.Join(byKey[k], ", "))
	}
	return sb.String(), nil
}

func (r *Runner) ensureSweepOutcome(ctx context.Context) {
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, `
		INSERT IGNORE INTO outcomes (id, title, description, status, created_at, updated_at)
		VALUES (?, 'Nightly Attribution Sweep', 'Standing outcome for the automated sweep process', 'active', ?, ?)`,
		sweepOutcomeID, now, now)
	if err != nil {
		r.log.Warn("sweep-llm: failed to ensure sweep outcome (non-fatal)", "error", err)
		return
	}
	_ = r.applyTag(ctx, "outcome", sweepOutcomeID, "product", "Teamster", "sweep-llm")
	_ = r.applyTag(ctx, "outcome", sweepOutcomeID, "feature", "data-cleanup", "sweep-llm")
}

// unresolvedGapThreads returns gap threads that RecoverGaps skipped (no entity
// could be resolved from existing attributions). These are the targets for LLM
// fallback.
func (r *Runner) unresolvedGapThreads(ctx context.Context) ([]gapThread, error) {
	threads, err := r.gapThreads(ctx)
	if err != nil {
		return nil, err
	}

	var unresolved []gapThread
	for _, gt := range threads {
		et, eid, _, _, err := r.resolveGapEntity(ctx, gt)
		if err != nil {
			continue
		}
		if et == "" || eid == "" {
			unresolved = append(unresolved, gt)
		}
	}
	return unresolved, nil
}

func writeTempMappings(mappings []SynthesisMapping) (string, error) {
	data, err := json.Marshal(mappings)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "sweep-llm-mappings-*.json")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close() //nolint:errcheck
		os.Remove(f.Name()) //nolint:errcheck
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name()) //nolint:errcheck
		return "", err
	}
	return f.Name(), nil
}

// FormatDryRunTable returns a human-readable summary of proposed LLM synthesis
// for operator review.
func FormatDryRunTable(orphans []OrphanSession, logger *slog.Logger) {
	if len(orphans) == 0 {
		logger.Info("sweep-llm dry-run: no orphan sessions to synthesize")
		return
	}

	var totalCost float64
	for _, o := range orphans {
		totalCost += o.CostUSD
		logger.Info("sweep-llm dry-run candidate",
			"session_id", o.SessionID,
			"host", o.Host,
			"messages", o.MsgCount,
			"cost_usd", fmt.Sprintf("%.2f", o.CostUSD))
	}
	logger.Info("sweep-llm dry-run summary",
		"total_sessions", len(orphans),
		"total_cost_usd", fmt.Sprintf("%.2f", totalCost))
}

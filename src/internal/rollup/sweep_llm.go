package rollup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/llm"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/transcript"
	"github.com/bmjdotnet/teamster/internal/wms"
)

const (
	maxLLMSessions  = 10
	llmPhaseTimeout = 5 * time.Minute
)

var defaultFacetKeys = map[string]bool{
	"feature": true, "bug": true, "refactor": true, "infra": true,
	"docs": true, "research": true, "test": true, "admin": true,
}

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
func (r *Runner) SweepLLM(ctx context.Context, opts RecoverOptions) (SweepLLMStats, error) {
	llmCtx, cancel := context.WithTimeout(ctx, llmPhaseTimeout)
	defer cancel()

	tagVocab, err := r.loadTagVocab(ctx)
	if err != nil {
		r.log.Warn("sweep-llm: failed to load tag vocabulary; proceeding without", "error", err)
	}

	facetKeys, err := r.loadFacetKeys(ctx, "work-type")
	if err != nil {
		r.log.Warn("sweep-llm: failed to load facet keys, using defaults", "error", err)
		facetKeys = defaultFacetKeys
	}
	if len(facetKeys) == 0 {
		facetKeys = defaultFacetKeys
	}

	if _, err := r.sweep.EnsureSweepOutcome(ctx); err != nil {
		r.log.Warn("sweep-llm: failed to ensure sweep outcome (non-fatal)", "error", err)
	}

	var stats SweepLLMStats
	var mappings []SynthesisMapping

	orphanMappings, orphanStats := r.synthesizeOrphans(llmCtx, opts, tagVocab, facetKeys)
	mappings = append(mappings, orphanMappings...)
	stats.OrphansSynthesized = orphanStats.synthesized
	stats.Skipped += orphanStats.skipped
	stats.Errors += orphanStats.errors

	remaining := maxLLMSessions - len(orphanMappings)
	if remaining > 0 {
		gapMappings, gapStats := r.synthesizeGapFallback(llmCtx, opts, tagVocab, remaining, facetKeys)
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

func (r *Runner) synthesizeOrphans(ctx context.Context, opts RecoverOptions, tagVocab string, facetKeys map[string]bool) ([]SynthesisMapping, synthesisPassStats) {
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
			if err := r.createSynthesizedOutcome(ctx, mapping, facetKeys); err != nil {
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

func (r *Runner) synthesizeGapFallback(ctx context.Context, opts RecoverOptions, tagVocab string, remaining int, facetKeys map[string]bool) ([]SynthesisMapping, synthesisPassStats) {
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
		if sessionsSeen[gt.SessionID] {
			continue
		}
		sessionsSeen[gt.SessionID] = true

		mapping, err := r.synthesizeSession(ctx, gt.SessionID, opts.ProjectsDir, tagVocab)
		if err != nil {
			r.log.Warn("sweep-llm: gap synthesis failed; skipping",
				"session_id", gt.SessionID, "error", err)
			stats.errors++
			continue
		}

		if !opts.DryRun {
			if err := r.createSynthesizedOutcome(ctx, mapping, facetKeys); err != nil {
				r.log.Warn("sweep-llm: create gap outcome failed; skipping",
					"session_id", gt.SessionID, "outcome_id", mapping.outcomeID, "error", err)
				stats.errors++
				continue
			}
		} else {
			r.log.Info("sweep-llm (dry-run): would synthesize gap session",
				"session_id", gt.SessionID, "outcome_id", mapping.outcomeID,
				"title", mapping.title, "confidence", mapping.confidence)
		}

		mappings = append(mappings, SynthesisMapping{
			SessionID:       gt.SessionID,
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
	slugKey         string
	slugValue       string
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
		slugKey:         resp.SlugKey,
		slugValue:       resp.SlugValue,
		component:       resp.Component,
		priority:        resp.Priority,
		confidence:      resp.Confidence,
		evidenceExcerpt: resp.EvidenceExcerpt,
	}, nil
}

// createSynthesizedOutcome creates the LLM-synthesized outcome via
// wms.Writer.CreateOutcome — check-then-create for idempotency, since
// CreateOutcome's current implementation is a plain INSERT, not yet the
// INSERT-IGNORE-equivalent the interface doc assumes (see store.go's doc
// comment on SweepStore.EnsureSweepOutcome for the same issue) — and tags it
// via wms.Writer.TagEntity, replacing the raw INSERT IGNORE + hand-rolled tag
// SQL (applyTag) the pre-port code used. Dropping the raw applyTag variant
// entirely, per 01-interfaces.md's note.
func (r *Runner) createSynthesizedOutcome(ctx context.Context, m *synthesizedMapping, facetKeys map[string]bool) error {
	if _, err := r.reader.GetOutcome(ctx, m.outcomeID); err == nil {
		// already exists — fall through to tagging (idempotent)
	} else if !store.IsNotFound(err) {
		return fmt.Errorf("check outcome: %w", err)
	} else if err := r.writer.CreateOutcome(ctx, &wms.Outcome{
		ID: m.outcomeID, Title: m.title, Description: m.description, Status: "active",
	}); err != nil {
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
	if m.slugKey != "" && m.slugValue != "" && facetKeys[m.slugKey] {
		tags = append(tags, struct{ key, value string }{m.slugKey, m.slugValue})
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
		if err := r.writer.TagEntity(ctx, "outcome", m.outcomeID, tag.key, tag.value, "sweep-llm", ""); err != nil {
			r.log.Warn("sweep-llm: tag failed (non-fatal)",
				"outcome_id", m.outcomeID, "key", tag.key, "value", tag.value, "error", err)
		}
	}

	return nil
}

func (r *Runner) loadTagVocab(ctx context.Context) (string, error) {
	rows, err := r.sweep.TagVocab(ctx)
	if err != nil {
		return "", err
	}

	byKey := map[string][]string{}
	facetOf := map[string]string{}
	for _, row := range rows {
		byKey[row.Key] = append(byKey[row.Key], row.Value)
		if row.FacetSource != "" {
			facetOf[row.Key] = row.FacetSource
		}
	}

	var sb strings.Builder
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		annotation := ""
		if facetOf[k] != "" {
			annotation = fmt.Sprintf(" (facet of %s)", facetOf[k])
		}
		fmt.Fprintf(&sb, "%s%s: %s\n", k, annotation, strings.Join(byKey[k], ", "))
	}
	return sb.String(), nil
}

func (r *Runner) loadFacetKeys(ctx context.Context, source string) (map[string]bool, error) {
	keys, err := r.sweep.FacetKeys(ctx, source)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out, nil
}

// unresolvedGapThreads returns gap threads that RecoverGaps skipped (no entity
// could be resolved from existing attributions). These are the targets for LLM
// fallback.
func (r *Runner) unresolvedGapThreads(ctx context.Context) ([]store.GapThread, error) {
	threads, err := r.rec.GapThreads(ctx)
	if err != nil {
		return nil, err
	}

	var unresolved []store.GapThread
	for _, gt := range threads {
		entity, _, _, err := r.resolveGapEntity(ctx, gt)
		if err != nil {
			continue
		}
		if entity.EntityType == "" || entity.EntityID == "" {
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
		f.Close()            //nolint:errcheck
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

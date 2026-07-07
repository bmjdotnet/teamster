// demogen populates a Teamster MySQL database with correlated synthetic demo
// data showing an idealized Teamster deployment. All rows use IDs prefixed with
// "demo-" so they can be removed cleanly with --clean.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/store"
	_ "github.com/bmjdotnet/teamster/internal/store/mysql" // registers the "mysql"/"mariadb" store.Open schemes
	"github.com/bmjdotnet/teamster/internal/wms"
)

// --------------------------------------------------------------------------
// Constants and pricing
// --------------------------------------------------------------------------

const (
	priceOpusInputPer1M  = 5.00
	priceOpusOutputPer1M = 25.00
	priceSonnetInput     = 3.00
	priceSonnetOutput    = 15.00
	priceHaikuInput      = 0.80
	priceHaikuOutput     = 4.00
	cacheReadFactor      = 0.10
	cacheWriteFactor     = 1.25
)

var modelPrices = map[string][2]float64{
	"claude-opus-4-8":   {priceOpusInputPer1M, priceOpusOutputPer1M},
	"claude-sonnet-4-6": {priceSonnetInput, priceSonnetOutput},
	"claude-haiku-4-5":  {priceHaikuInput, priceHaikuOutput},
}

func calcCost(model string, inputTok, outputTok, cacheRead, cacheWrite int64) float64 {
	p := modelPrices[model]
	in := p[0]
	out := p[1]
	return float64(inputTok)*in/1e6 +
		float64(outputTok)*out/1e6 +
		float64(cacheRead)*in*cacheReadFactor/1e6 +
		float64(cacheWrite)*in*cacheWriteFactor/1e6
}

// --------------------------------------------------------------------------
// Structures
// --------------------------------------------------------------------------

type workUnit struct {
	id        string
	outcomeID string
	title     string
	agent     string
	workType  string
	status    string // done / active
	startDay  int
	durDays   float64
	msgCount  int
	tags      []tagKV
	// progression defines state transitions for this workunit
	progression []stateStep
}

type stateStep struct {
	state    string
	fraction float64 // fraction of total duration this state ends at
}

type outcome struct {
	id       string
	parentID string // empty for strategic (root)
	title    string
	status   string
	units    []*workUnit
	tags     []tagKV
}

type tagKV struct{ key, val string }

// --------------------------------------------------------------------------
// Main
// --------------------------------------------------------------------------

func main() {
	dsn := flag.String("dsn", "", "MySQL DSN (default: TEAMSTER_STORE_DSN env var)")
	clean := flag.Bool("clean", false, "Delete existing demo data and exit")
	flag.Parse()

	if *dsn == "" {
		*dsn = os.Getenv("TEAMSTER_STORE_DSN")
	}
	if *dsn == "" {
		if cfg, err := config.Load(); err == nil && cfg.StoreDSN.Raw != "" {
			*dsn = cfg.StoreDSN.Raw
		}
	}
	if *dsn == "" {
		log.Fatal("no DSN: set TEAMSTER_STORE_DSN, pass --dsn=, or configure store.dsn in teamster.yaml")
	}

	ctx := context.Background()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close() //nolint:errcheck

	ds, ok := st.(store.DemoSeeder)
	if !ok {
		log.Fatalf("backend has no demo seeder")
	}
	rx, ok := st.(store.RawExecutor)
	if !ok {
		log.Fatalf("backend has no raw-SQL surface")
	}

	// Always clean before generating to prevent duplicates (B5 fix)
	fmt.Println("Cleaning existing demo data...")
	if err := ds.CleanDemo(ctx); err != nil {
		log.Fatalf("clean: %v", err)
	}

	if *clean {
		fmt.Println("Clean complete.")
		return
	}

	rng := rand.New(rand.NewSource(42))
	anchor := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -30)
	genTime := time.Now().UTC()

	outcomes := buildOutcomes()

	var totalOutcomes, totalUnits, totalMessages, totalIntervals, totalTags int
	var totalJournal, totalSessions, totalDeps, totalEdges int

	// Insert outcomes
	for _, out := range outcomes {
		if err := upsertOutcome(ctx, st, out, anchor); err != nil {
			log.Fatalf("upsert outcome %s: %v", out.id, err)
		}
		totalOutcomes++

		if err := bindTags(ctx, st, "outcome", out.id, out.tags); err != nil {
			log.Fatalf("bind outcome tags %s: %v", out.id, err)
		}
		totalTags += len(out.tags)
	}

	// Insert outcome_edges
	for _, out := range outcomes {
		if out.parentID != "" {
			if err := st.AddOutcomeEdge(ctx, out.parentID, out.id); err != nil {
				log.Fatalf("insert edge %s→%s: %v", out.parentID, out.id, err)
			}
			totalEdges++
		}
	}

	// Collect all workunits across outcomes
	var allUnits []*workUnit
	for _, out := range outcomes {
		for _, wu := range out.units {
			allUnits = append(allUnits, wu)
		}
	}

	// Insert workunits, messages, intervals, attribution, journal, sessions
	for _, wu := range allUnits {
		if err := upsertWorkUnit(ctx, st, wu, anchor); err != nil {
			log.Fatalf("upsert workunit %s: %v", wu.id, err)
		}
		totalUnits++

		if err := bindTags(ctx, st, "workunit", wu.id, wu.tags); err != nil {
			log.Fatalf("bind workunit tags %s: %v", wu.id, err)
		}
		totalTags += len(wu.tags)

		msgs, err := genMessages(ctx, ds, rng, wu, anchor)
		if err != nil {
			log.Fatalf("gen messages %s: %v", wu.id, err)
		}
		totalMessages += len(msgs)

		intervals, err := genIntervals(ctx, rx, wu, anchor, msgs, genTime)
		if err != nil {
			log.Fatalf("gen intervals %s: %v", wu.id, err)
		}
		totalIntervals += intervals

		if err := genAttribution(ctx, rx, wu, msgs); err != nil {
			log.Fatalf("gen attribution %s: %v", wu.id, err)
		}

		n, err := genJournal(ctx, rx, wu, anchor)
		if err != nil {
			log.Fatalf("gen journal %s: %v", wu.id, err)
		}
		totalJournal += n

		n, err = genSessions(ctx, st, wu, msgs)
		if err != nil {
			log.Fatalf("gen sessions %s: %v", wu.id, err)
		}
		totalSessions += n
	}

	// Entity dependencies
	n, err := genDependencies(ctx, st, allUnits, anchor)
	if err != nil {
		log.Fatalf("gen dependencies: %v", err)
	}
	totalDeps += n

	// Cost rollup
	//
	// TODO(phase-13/ADR-4 step 9): route through the real rollup service /
	// AllocationStore.BuildCostRollup once Phase 11 lands, per
	// phase-13-nongo-adrs-admin-plane.md's named partial dependency — deferred
	// rather than hand-rolling a separate rollup path for demo data.
	if err := buildCostRollup(ctx, rx, outcomes); err != nil {
		log.Fatalf("build cost_rollup: %v", err)
	}

	// Link usage_attribution to covering intervals
	if err := linkAttributionIntervals(ctx, rx); err != nil {
		log.Fatalf("link attribution intervals: %v", err)
	}

	// Assemble cost on closed state intervals
	if err := assembleIntervalCosts(ctx, rx, genTime); err != nil {
		log.Fatalf("assemble interval costs: %v", err)
	}

	fmt.Printf("Created %d outcomes, %d edges, %d workunits, %d messages, %d intervals, %d tag bindings, %d journal, %d sessions, %d deps\n",
		totalOutcomes, totalEdges, totalUnits, totalMessages, totalIntervals, totalTags, totalJournal, totalSessions, totalDeps)
}

// --------------------------------------------------------------------------
// Product / workunit definitions
// --------------------------------------------------------------------------

func buildOutcomes() []*outcome {
	// Standard done progression: pending → active → review → done (30%)
	progStandard := []stateStep{
		{"pending", 0.05},
		{"active", 0.75},
		{"review", 0.95},
		{"done", 1.00},
	}

	// Rework progression: pending → active → review → active → review → done (50%)
	progRework := []stateStep{
		{"pending", 0.05},
		{"active", 0.40},
		{"review", 0.50},
		{"active", 0.75},
		{"review", 0.95},
		{"done", 1.00},
	}

	// Complex: pending → active → blocked → active → review → done (20%)
	progComplex := []stateStep{
		{"pending", 0.05},
		{"active", 0.30},
		{"blocked", 0.40},
		{"active", 0.70},
		{"review", 0.95},
		{"done", 1.00},
	}

	// Active (ongoing) progression
	progActive := []stateStep{
		{"pending", 0.05},
		{"active", 1.00},
	}

	strategicID := "demo-out-acme-platform"

	return []*outcome{
		// Strategic (root) outcome
		{
			id:     strategicID,
			title:  "Acme Platform",
			status: "active",
			tags: []tagKV{
				{"product", "acme-platform"},
				{"priority", "p1"},
			},
		},

		// Tactical 1: API Overhaul
		{
			id:       "demo-out-api-overhaul",
			parentID: strategicID,
			title:    "API Overhaul",
			status:   "active",
			tags: []tagKV{
				{"product", "acme-platform"},
				{"feature", "api-overhaul"},
				{"product-version", "2.0.0"},
				{"github.owner", "acme"},
				{"github.repo", "acme-api"},
				{"priority", "p1"},
			},
			units: []*workUnit{
				{
					id:          "demo-wu-schema-migration",
					outcomeID:   "demo-out-api-overhaul",
					title:       "schema-migration",
					agent:       "@store",
					workType:    "feature",
					status:      "done",
					startDay:    0,
					durDays:     4,
					msgCount:    45,
					progression: progRework,
					tags: []tagKV{
						{"work-type", "feature"},
						{"feature", "schema-migration"},
						{"priority", "p1"},
						{"github.pr", "142"},
						{"github.issue", "89"},
					},
				},
				{
					id:          "demo-wu-endpoint-refactor",
					outcomeID:   "demo-out-api-overhaul",
					title:       "endpoint-refactor",
					agent:       "@api",
					workType:    "feature",
					status:      "done",
					startDay:    4,
					durDays:     5,
					msgCount:    50,
					progression: progComplex,
					tags: []tagKV{
						{"work-type", "feature"},
						{"feature", "endpoint-refactor"},
						{"priority", "p1"},
						{"github.pr", "156"},
						{"github.issue", "112"},
					},
				},
				{
					id:          "demo-wu-auth-integration",
					outcomeID:   "demo-out-api-overhaul",
					title:       "auth-integration",
					agent:       "@store",
					workType:    "feature",
					status:      "done",
					startDay:    9,
					durDays:     3,
					msgCount:    30,
					progression: progStandard,
					tags: []tagKV{
						{"work-type", "feature"},
						{"feature", "auth-integration"},
						{"priority", "p2"},
						{"github.pr", "161"},
						{"jira.id", "ACME-4521"},
						{"jira.project", "ACME"},
					},
				},
			},
		},

		// Tactical 2: Dashboard Rebuild
		{
			id:       "demo-out-dashboard-rebuild",
			parentID: strategicID,
			title:    "Dashboard Rebuild",
			status:   "active",
			tags: []tagKV{
				{"product", "acme-platform"},
				{"feature", "dashboard-rebuild"},
				{"product-version", "1.0.0"},
				{"github.owner", "acme"},
				{"github.repo", "dashboard-app"},
				{"priority", "p1"},
			},
			units: []*workUnit{
				{
					id:          "demo-wu-component-library",
					outcomeID:   "demo-out-dashboard-rebuild",
					title:       "component-library",
					agent:       "@display",
					workType:    "feature",
					status:      "done",
					startDay:    5,
					durDays:     4,
					msgCount:    40,
					progression: progRework,
					tags: []tagKV{
						{"work-type", "feature"},
						{"feature", "component-library"},
						{"priority", "p1"},
						{"github.pr", "23"},
						{"github.issue", "15"},
					},
				},
				{
					id:          "demo-wu-data-viz",
					outcomeID:   "demo-out-dashboard-rebuild",
					title:       "data-viz",
					agent:       "@engine",
					workType:    "feature",
					status:      "done",
					startDay:    9,
					durDays:     5,
					msgCount:    48,
					progression: progComplex,
					tags: []tagKV{
						{"work-type", "feature"},
						{"feature", "data-viz"},
						{"priority", "p1"},
						{"github.pr", "31"},
					},
				},
				{
					id:          "demo-wu-realtime-updates",
					outcomeID:   "demo-out-dashboard-rebuild",
					title:       "realtime-updates",
					agent:       "@infra",
					workType:    "feature",
					status:      "active",
					startDay:    14,
					durDays:     16,
					msgCount:    35,
					progression: progActive,
					tags: []tagKV{
						{"work-type", "feature"},
						{"feature", "realtime-updates"},
						{"priority", "p2"},
						{"github.pr", "45"},
					},
				},
			},
		},

		// Tactical 3: Ops Automation
		{
			id:       "demo-out-ops-automation",
			parentID: strategicID,
			title:    "Ops Automation",
			status:   "active",
			tags: []tagKV{
				{"product", "acme-platform"},
				{"feature", "ops-automation"},
				{"component", "monitoring"},
				{"priority", "p2"},
				{"git.repo", "/home/user/projects/monitoring"},
				{"git.branch", "main"},
			},
			units: []*workUnit{
				{
					id:          "demo-wu-ci-pipeline",
					outcomeID:   "demo-out-ops-automation",
					title:       "ci-pipeline",
					agent:       "@ops",
					workType:    "infra",
					status:      "done",
					startDay:    8,
					durDays:     3,
					msgCount:    25,
					progression: progStandard,
					tags: []tagKV{
						{"work-type", "infra"},
						{"feature", "ci-pipeline"},
						{"priority", "p2"},
						{"git.repo", "/home/user/projects/monitoring"},
						{"git.branch", "main"},
					},
				},
				{
					id:          "demo-wu-monitoring-setup",
					outcomeID:   "demo-out-ops-automation",
					title:       "monitoring-setup",
					agent:       "@test",
					workType:    "infra",
					status:      "done",
					startDay:    11,
					durDays:     4,
					msgCount:    30,
					progression: progRework,
					tags: []tagKV{
						{"work-type", "infra"},
						{"feature", "monitoring-setup"},
						{"priority", "p2"},
						{"git.repo", "/home/user/projects/monitoring"},
						{"git.branch", "main"},
					},
				},
				{
					id:          "demo-wu-deploy-automation",
					outcomeID:   "demo-out-ops-automation",
					title:       "deploy-automation",
					agent:       "@docs",
					workType:    "infra",
					status:      "active",
					startDay:    18,
					durDays:     12,
					msgCount:    20,
					progression: progActive,
					tags: []tagKV{
						{"work-type", "infra"},
						{"feature", "deploy-automation"},
						{"priority", "p3"},
					},
				},
			},
		},
	}
}

// --------------------------------------------------------------------------
// Upsert entities
// --------------------------------------------------------------------------

// upsertOutcome creates out via the domain API (wms.Writer.CreateOutcome).
//
// NOTE: unlike the raw INSERT it replaces, CreateOutcome always stamps
// created_at/updated_at to the real current time — it does not accept a
// caller-supplied timestamp. Outcomes/workunits therefore no longer show
// backdated creation dates spread across the 30-day anchor window (they all
// read as "created today"); the interval/message/session time series that
// drive the actual dashboards are unaffected, since those keep exact
// historical timestamps (see genIntervals/genMessages/genSessions). This is
// the deliberate, spec'd tradeoff of Phase 13/ADR-4 killing demogen's silent
// schema-rot problem via the compile-time-checked domain API.
func upsertOutcome(ctx context.Context, wr wms.Writer, out *outcome, _ time.Time) error {
	return wr.CreateOutcome(ctx, &wms.Outcome{
		ID:            out.id,
		Title:         out.title,
		Status:        out.status,
		OriginHost:    "demo-host",
		OriginSession: "demo-sess-lead",
	})
}

func upsertWorkUnit(ctx context.Context, wr wms.Writer, wu *workUnit, _ time.Time) error {
	return wr.CreateWorkUnit(ctx, &wms.WorkUnit{
		ID:            wu.id,
		OutcomeID:     wu.outcomeID,
		Title:         wu.title,
		Status:        wu.status,
		AgentID:       wu.agent,
		OriginHost:    "demo-host",
		OriginSession: "demo-sess-lead",
	})
}

// --------------------------------------------------------------------------
// Tags
// --------------------------------------------------------------------------

// bindTags applies each tag via wms.Writer.TagEntity, which creates the
// (key, value) pair as a non-seed context tag on first use — the domain-API
// equivalent of the former ensureTag+INSERT INTO entity_tags pair.
func bindTags(ctx context.Context, wr wms.Writer, entityType, entityID string, tags []tagKV) error {
	for _, t := range tags {
		if err := wr.TagEntity(ctx, entityType, entityID, t.key, t.val, "manual", ""); err != nil {
			return fmt.Errorf("bind tag %s:%s to %s/%s: %w", t.key, t.val, entityType, entityID, err)
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// Message generation
// --------------------------------------------------------------------------

type msgRecord struct {
	messageID        string
	sessionID        string
	agentName        string
	model            string
	inputTokens      int64
	outputTokens     int64
	cacheReadTokens  int64
	cacheWriteTokens int64
	costUSD          float64
	ts               time.Time
}

func pickModel(rng *rand.Rand) string {
	n := rng.Float64()
	switch {
	case n < 0.57:
		return "claude-opus-4-8"
	case n < 0.96:
		return "claude-sonnet-4-6"
	default:
		return "claude-haiku-4-5"
	}
}

type workSession struct {
	start    time.Time
	duration time.Duration
	nMsgs    int
}

func planSessions(rng *rand.Rand, wu *workUnit, anchor time.Time) []workSession {
	wuStart := anchor.Add(time.Duration(wu.startDay*24) * time.Hour)
	wuEnd := wuStart.Add(time.Duration(wu.durDays*24) * time.Hour)
	if wuEnd.After(time.Now().UTC()) {
		wuEnd = time.Now().UTC()
	}

	totalDur := wuEnd.Sub(wuStart)
	if totalDur < time.Hour {
		totalDur = time.Hour
	}

	remaining := wu.msgCount
	var sessions []workSession

	nSessions := 2 + rng.Intn(3)
	if remaining < nSessions {
		nSessions = remaining
	}

	cursor := wuStart
	for i := 0; i < nSessions && remaining > 0; i++ {
		sessDur := time.Duration(1+rng.Intn(3)) * time.Hour

		if i > 0 {
			gapHours := 2 + rng.Intn(12)
			cursor = cursor.Add(time.Duration(gapHours) * time.Hour)
		}

		if cursor.After(wuEnd) {
			cursor = wuEnd.Add(-time.Hour)
		}

		msgsInSess := remaining / (nSessions - i)
		if msgsInSess < 1 {
			msgsInSess = 1
		}
		remaining -= msgsInSess

		sessions = append(sessions, workSession{
			start:    cursor,
			duration: sessDur,
			nMsgs:    msgsInSess,
		})
		cursor = cursor.Add(sessDur)
	}
	return sessions
}

func pickStopReason(rng *rand.Rand) string {
	n := rng.Float64()
	switch {
	case n < 0.50:
		return "tool_use"
	case n < 0.94:
		return ""
	default:
		return "end_turn"
	}
}

func pickSpeed(rng *rand.Rand) string {
	if rng.Float64() < 0.56 {
		return "standard"
	}
	return ""
}

func pickServiceTier(rng *rand.Rand) string {
	if rng.Float64() < 0.83 {
		return "standard"
	}
	return ""
}

func genMessages(ctx context.Context, ds store.DemoSeeder, rng *rand.Rand, wu *workUnit, anchor time.Time) ([]msgRecord, error) {
	sessions := planSessions(rng, wu, anchor)
	var records []msgRecord
	var rows []store.TelemetryRow

	// For done workunits, compute the hard deadline so no message exceeds it
	var wuEnd time.Time
	if wu.status == "done" {
		wuStart := anchor.Add(time.Duration(wu.startDay*24) * time.Hour)
		wuEnd = wuStart.Add(time.Duration(wu.durDays*24) * time.Hour)
	}

	for si, sess := range sessions {
		sessID := fmt.Sprintf("demo-sess-%s-%02d", wu.id[len("demo-wu-"):], si)

		for mi := 0; mi < sess.nMsgs; mi++ {
			fraction := float64(mi) / float64(sess.nMsgs)
			msgTS := sess.start.Add(time.Duration(fraction * float64(sess.duration)))

			if wu.status == "done" && msgTS.After(wuEnd.Add(-time.Second)) {
				msgTS = wuEnd.Add(-time.Second)
			}

			model := pickModel(rng)

			// Token patterns: input near-zero (1-350), cache_read dominates (40k-200k)
			inputToks := int64(1 + rng.Intn(350))
			cacheRead := int64(40000 + rng.Intn(160000))
			// Cache read grows through session
			cacheRead += int64(float64(mi) * float64(cacheRead) * 0.3 / float64(max(sess.nMsgs, 1)))
			outputToks := int64(130 + rng.Intn(1270))
			cacheWrite := int64(3000 + rng.Intn(8000))

			totalInput := inputToks + cacheRead

			cost := calcCost(model, inputToks, outputToks, cacheRead, cacheWrite)
			msgID := fmt.Sprintf("demo-msg-%s-%02d-%03d", wu.id[len("demo-wu-"):], si, mi)

			stopReason := pickStopReason(rng)
			speed := pickSpeed(rng)
			serviceTier := pickServiceTier(rng)

			var nText, nToolUse int
			switch stopReason {
			case "tool_use":
				nToolUse = 1 + rng.Intn(3)
				nText = 0
			case "end_turn":
				nText = 1
				nToolUse = 0
			default:
				nText = 0
				nToolUse = 0
			}

			rec := msgRecord{
				messageID:        msgID,
				sessionID:        sessID,
				agentName:        wu.agent,
				model:            model,
				inputTokens:      inputToks,
				outputTokens:     outputToks,
				cacheReadTokens:  cacheRead,
				cacheWriteTokens: cacheWrite,
				costUSD:          cost,
				ts:               msgTS,
			}
			records = append(records, rec)

			rows = append(rows, store.TelemetryRow{
				SessionID:        sessID,
				MessageID:        msgID,
				AgentName:        wu.agent,
				Host:             "demo-host",
				Model:            model,
				InputTokens:      inputToks,
				OutputTokens:     outputToks,
				CacheReadTokens:  cacheRead,
				CacheWriteTokens: cacheWrite,
				NText:            int64(nText),
				NToolUse:         int64(nToolUse),
				TotalInput:       totalInput,
				StopReason:       stopReason,
				ServiceTier:      serviceTier,
				Speed:            speed,
				CostUSD:          cost,
				Timestamp:        msgTS,
			})
		}
	}
	if _, err := ds.SeedLedger(ctx, rows); err != nil {
		return nil, fmt.Errorf("seed ledger for %s: %w", wu.id, err)
	}
	return records, nil
}

// --------------------------------------------------------------------------
// Intervals
// --------------------------------------------------------------------------

func genIntervals(ctx context.Context, rx store.RawExecutor, wu *workUnit, anchor time.Time, msgs []msgRecord, genTime time.Time) (int, error) {
	wuStart := anchor.Add(time.Duration(wu.startDay*24) * time.Hour)
	wuEnd := wuStart.Add(time.Duration(wu.durDays*24) * time.Hour)
	if wu.status == "active" && wuEnd.After(time.Now().UTC()) {
		wuEnd = time.Now().UTC()
	}

	totalDur := wuEnd.Sub(wuStart)
	count := 0

	// State intervals from progression
	for i, step := range wu.progression {
		var iStart time.Time
		if i == 0 {
			iStart = wuStart
		} else {
			iStart = wuStart.Add(time.Duration(wu.progression[i-1].fraction * float64(totalDur)))
		}

		isLast := i == len(wu.progression)-1

		var iEnd *time.Time
		var durMs *int64
		var assembledAt *time.Time
		if wu.status == "active" && isLast {
			// Leave open
		} else {
			t := wuStart.Add(time.Duration(step.fraction * float64(totalDur)))
			iEnd = &t
			ms := t.Sub(iStart).Milliseconds()
			durMs = &ms
			assembledAt = &genTime
		}

		// Phase assignment: ~60% have phase, rest NULL
		var phase *string
		var phaseSource string
		midFrac := step.fraction
		if i > 0 {
			midFrac = (wu.progression[i-1].fraction + step.fraction) / 2
		}
		var phaseAssembledAt *time.Time
		if shouldAssignPhase(step.state, midFrac) {
			p := phaseForState(step.state, midFrac)
			phase = &p
			phaseSource = "classifier"
			// Mirror the classifier: a derived phase carries its own watermark
			// (phase_assembled_at), distinct from the rollup's cost assembled_at.
			phaseAssembledAt = assembledAt
		}

		// State intervals: identity_source='' for ~90%, 'carried' for ~10%
		identitySource := ""
		sessID := ""
		agentName := ""
		host := ""
		if i == 5 {
			identitySource = "carried"
			sessID = fmt.Sprintf("demo-sess-%s-00", wu.id[len("demo-wu-"):])
			agentName = strings.TrimPrefix(wu.agent, "@")
			host = "demo-host"
		}

		_, err := rx.ExecRaw(ctx, `
			INSERT IGNORE INTO wms_intervals
			  (kind, entity_type, entity_id, state, session_id, agent_name, host,
			   started_at, ended_at, duration_ms, phase, phase_source, assembled_at, phase_assembled_at, cost_usd, cost_tokens, identity_source)
			VALUES ('state', 'workunit', ?, ?, ?, ?, ?,
			        ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)`,
			wu.id, step.state, sessID, agentName, host,
			iStart, iEnd, durMs, phase, phaseSource, assembledAt, phaseAssembledAt, identitySource)
		if err != nil {
			return count, fmt.Errorf("insert state interval %s/%s: %w", wu.id, step.state, err)
		}
		count++
	}

	// Focus intervals: one per session
	sessions := groupBySession(msgs)
	for sessID, sessmsgs := range sessions {
		if len(sessmsgs) == 0 {
			continue
		}
		fStart := sessmsgs[0].ts
		fEnd := sessmsgs[len(sessmsgs)-1].ts.Add(5 * time.Minute)
		ms := fEnd.Sub(fStart).Milliseconds()

		_, err := rx.ExecRaw(ctx, `
			INSERT IGNORE INTO wms_intervals
			  (kind, entity_type, entity_id, state, session_id, agent_name, host,
			   started_at, ended_at, duration_ms, phase, phase_source, assembled_at, cost_usd, cost_tokens, identity_source)
			VALUES ('focus', 'workunit', ?, '', ?, ?, '',
			        ?, ?, ?, NULL, '', NULL, NULL, NULL, 'direct')`,
			wu.id, sessID, wu.agent,
			fStart, fEnd, ms)
		if err != nil {
			return count, fmt.Errorf("insert focus interval %s/%s: %w", wu.id, sessID, err)
		}
		count++
	}

	return count, nil
}

func shouldAssignPhase(state string, fraction float64) bool {
	if state == "blocked" || state == "pending" || state == "done" {
		return false
	}
	if state == "review" {
		return true
	}
	return fraction > 0.15 && fraction < 0.75
}

func phaseForState(state string, fraction float64) string {
	if state == "review" {
		return "review"
	}
	if state == "done" {
		return "review"
	}
	switch {
	case fraction < 0.15:
		return "design"
	case fraction < 0.70:
		return "build"
	case fraction < 0.85:
		return "test"
	default:
		return "review"
	}
}

func groupBySession(msgs []msgRecord) map[string][]msgRecord {
	m := make(map[string][]msgRecord)
	for _, msg := range msgs {
		m[msg.sessionID] = append(m[msg.sessionID], msg)
	}
	return m
}

// --------------------------------------------------------------------------
// Attribution — 100% attributed, no unallocated
// --------------------------------------------------------------------------

func genAttribution(ctx context.Context, rx store.RawExecutor, wu *workUnit, msgs []msgRecord) error {
	now := time.Now().UTC()
	for _, msg := range msgs {
		_, err := rx.ExecRaw(ctx, `
			INSERT IGNORE INTO usage_attribution
			  (message_id, entity_type, entity_id, weight, method, computed_at, interval_id)
			VALUES (?, 'workunit', ?, 1.00000, 'temporal_join', ?, 0)`,
			msg.messageID, wu.id, now)
		if err != nil {
			return fmt.Errorf("insert attribution %s: %w", msg.messageID, err)
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// Journal — one entry per status transition
// --------------------------------------------------------------------------

func genJournal(ctx context.Context, rx store.RawExecutor, wu *workUnit, anchor time.Time) (int, error) {
	wuStart := anchor.Add(time.Duration(wu.startDay*24) * time.Hour)
	wuEnd := wuStart.Add(time.Duration(wu.durDays*24) * time.Hour)
	totalDur := wuEnd.Sub(wuStart)

	count := 0
	for i, step := range wu.progression {
		var prevState string
		if i == 0 {
			prevState = ""
		} else {
			prevState = wu.progression[i-1].state
		}

		var transTS time.Time
		if i == 0 {
			transTS = wuStart
		} else {
			transTS = wuStart.Add(time.Duration(wu.progression[i-1].fraction * float64(totalDur)))
		}

		_, err := rx.ExecRaw(ctx, `
			INSERT INTO wms_journal
			  (entity_type, entity_id, field, old_value, new_value, agent_id, host, session_id, notes, created_at)
			VALUES ('workunit', ?, 'status', ?, ?, '', '', '', NULL, ?)`,
			wu.id, prevState, step.state, transTS)
		if err != nil {
			return count, fmt.Errorf("insert journal %s %s→%s: %w", wu.id, prevState, step.state, err)
		}
		count++
	}
	return count, nil
}

// --------------------------------------------------------------------------
// Sessions — one row per (session_id, agent_name)
// --------------------------------------------------------------------------

func genSessions(ctx context.Context, ss store.SessionStore, wu *workUnit, msgs []msgRecord) (int, error) {
	sessions := groupBySession(msgs)
	count := 0
	for sessID, sessmsgs := range sessions {
		if len(sessmsgs) == 0 {
			continue
		}
		firstSeen := sessmsgs[0].ts
		lastSeen := sessmsgs[len(sessmsgs)-1].ts

		// Lead session (agent_name='')
		if err := ss.CreateSession(ctx, store.Session{
			SessionID: sessID, Status: store.SessionStatusClosed,
			FirstSeen: firstSeen, LastSeen: lastSeen,
		}); err != nil {
			return count, fmt.Errorf("insert lead session %s: %w", sessID, err)
		}
		count++

		// Teammate session
		if err := ss.CreateSession(ctx, store.Session{
			SessionID: sessID, AgentName: wu.agent, Status: store.SessionStatusClosed,
			FirstSeen: firstSeen, LastSeen: lastSeen,
		}); err != nil {
			return count, fmt.Errorf("insert teammate session %s/%s: %w", sessID, wu.agent, err)
		}
		count++
	}
	return count, nil
}

// --------------------------------------------------------------------------
// Entity dependencies
// --------------------------------------------------------------------------

func genDependencies(ctx context.Context, wr wms.Writer, units []*workUnit, anchor time.Time) (int, error) {
	// Create 3 dependency chains
	deps := [][2]string{
		{"demo-wu-schema-migration", "demo-wu-endpoint-refactor"},
		{"demo-wu-endpoint-refactor", "demo-wu-auth-integration"},
		{"demo-wu-component-library", "demo-wu-data-viz"},
	}

	count := 0
	for _, d := range deps {
		err := wr.AddEntityDependency(ctx, &wms.Dependency{
			BlockerType: "workunit", BlockerID: d[0],
			BlockedType: "workunit", BlockedID: d[1],
		})
		if err != nil {
			return count, fmt.Errorf("insert dep %s→%s: %w", d[0], d[1], err)
		}
		count++
	}
	return count, nil
}

// --------------------------------------------------------------------------
// Link usage_attribution rows to covering state intervals
// --------------------------------------------------------------------------

func linkAttributionIntervals(ctx context.Context, rx store.RawExecutor) error {
	_, err := rx.ExecRaw(ctx, `
		UPDATE usage_attribution ua
		JOIN token_ledger tl ON tl.message_id = ua.message_id
		JOIN wms_intervals wi
		  ON wi.kind = 'state'
		 AND wi.entity_type = ua.entity_type
		 AND wi.entity_id = ua.entity_id
		 AND tl.timestamp >= wi.started_at
		 AND (wi.ended_at IS NULL OR tl.timestamp < wi.ended_at)
		SET ua.interval_id = wi.id
		WHERE ua.message_id LIKE 'demo-msg-%'
		  AND ua.interval_id = 0`)
	return err
}

// --------------------------------------------------------------------------
// Assemble cost on closed state intervals
// --------------------------------------------------------------------------

func assembleIntervalCosts(ctx context.Context, rx store.RawExecutor, genTime time.Time) error {
	_, err := rx.ExecRaw(ctx, `
		UPDATE wms_intervals wi
		JOIN (
			SELECT ua.interval_id,
			       SUM(tl.cost_usd * ua.weight) AS cost_usd,
			       SUM((tl.input_tokens + tl.cache_read_tokens) * ua.weight) AS cost_tokens
			FROM usage_attribution ua
			JOIN token_ledger tl ON tl.message_id = ua.message_id
			WHERE ua.message_id LIKE 'demo-msg-%'
			  AND ua.interval_id <> 0
			GROUP BY ua.interval_id
		) agg ON agg.interval_id = wi.id
		SET wi.cost_usd = agg.cost_usd,
		    wi.cost_tokens = agg.cost_tokens,
		    wi.assembled_at = ?
		WHERE wi.ended_at IS NOT NULL
		  AND wi.entity_id LIKE 'demo-%'`, genTime)
	return err
}

// --------------------------------------------------------------------------
// Cost rollup
// --------------------------------------------------------------------------

func buildCostRollup(ctx context.Context, rx store.RawExecutor, outcomes []*outcome) error {
	type rollupKey struct {
		day        string
		entityType string
		entityID   string
		agentName  string
		model      string
	}
	type rollupVal struct {
		tokens  int64
		costUSD float64
	}
	rollup := make(map[rollupKey]*rollupVal)

	for _, out := range outcomes {
		for _, wu := range out.units {
			rows, err := rx.QueryRaw(ctx, `
				SELECT tl.timestamp, tl.model, tl.agent_name,
				       tl.input_tokens + tl.cache_read_tokens AS tokens,
				       tl.cost_usd, ua.weight, ua.entity_type, ua.entity_id
				FROM token_ledger tl
				JOIN usage_attribution ua ON ua.message_id = tl.message_id
				WHERE tl.session_id LIKE ?`,
				"demo-sess-"+wu.id[len("demo-wu-"):]+"%")
			if err != nil {
				return fmt.Errorf("rollup query %s: %w", wu.id, err)
			}
			for rows.Next() {
				var ts time.Time
				var model, agentName, eType, eID string
				var tokens int64
				var costUSD, weight float64
				if err := rows.Scan(&ts, &model, &agentName, &tokens, &costUSD, &weight, &eType, &eID); err != nil {
					rows.Close()
					return err
				}
				k := rollupKey{
					day:        ts.Format("2006-01-02"),
					entityType: eType,
					entityID:   eID,
					agentName:  agentName,
					model:      model,
				}
				v := rollup[k]
				if v == nil {
					v = &rollupVal{}
					rollup[k] = v
				}
				v.tokens += int64(float64(tokens) * weight)
				v.costUSD += costUSD * weight
			}
			rows.Close()
		}
	}

	for k, v := range rollup {
		_, err := rx.ExecRaw(ctx, `
			INSERT INTO cost_rollup
			  (bucket_day, entity_type, entity_id, agent_name, model, tokens, cost_usd)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
			  tokens = tokens + VALUES(tokens),
			  cost_usd = cost_usd + VALUES(cost_usd)`,
			k.day, k.entityType, k.entityID, k.agentName, k.model, v.tokens, v.costUSD)
		if err != nil {
			return fmt.Errorf("upsert rollup %+v: %w", k, err)
		}
	}
	return nil
}

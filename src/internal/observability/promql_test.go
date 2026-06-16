package observability_test

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/promqltest"
)

// TestCanonicalJoinShape is tooth 3 from SPEC §4.4. It verifies the bridge
// gauge join produces 3.1666… for both lead (agent_name="") and teammate
// (agent_name="@scout") rows — the canonical expected values from SPEC §4.4.
//
// PromQL group_left/group_right semantics:
//   - group_left  → LEFT side is MANY (high-cardinality); RIGHT must be unique.
//   - group_right → RIGHT side is MANY; LEFT must be unique.
//
// The labels argument to group_left(…)/group_right(…) names labels to carry
// FROM the one (unique) side ONTO the many side's result rows.
//
// Our fixture: bridge gauge has TWO rows per session_id (lead + scout), so it
// is the MANY side. The pre-aggregated rate (sum by session_id) has ONE row per
// session_id, so it is the unique side.
//
// Fix B (operands swapped from SPEC §4.3 — group_left now valid):
//
//	teamster_session_active               ← many (LEFT)
//	  * on(session_id) group_left()
//	  sum by(session_id)(rate(…))         ← one (RIGHT)
//
// group_left() with empty list: no extra labels are carried from the RHS onto
// the result (RHS has only session_id after the inner sum). Result rows inherit
// LHS labels (agent_name, project_id, etc.). Sum-by then aggregates correctly.
//
// Rate at 2m: (200+100+60+20)/120 = 380/120 ≈ 3.1667. Both lead and scout rows
// get this value because the pre-aggregated rate is broadcast to each bridge row.
func TestCanonicalJoinShape(t *testing.T) {
	engine := promqltest.NewTestEngine(t, false, 0, 50000000)

	const fixture = `
load 1m
	claude_code_token_usage_tokens_total{session_id="X",model="opus",type="input"}      0 100 200
	claude_code_token_usage_tokens_total{session_id="X",model="opus",type="output"}     0  50 100
	claude_code_token_usage_tokens_total{session_id="X",model="sonnet",type="input"}    0  30  60
	claude_code_token_usage_tokens_total{session_id="X",model="sonnet",type="output"}   0  10  20
	teamster_session_active{session_id="X",host="h1",team_name="ops",agent_name="",outcome_id="O",workunit_id="U"} 1 1 1
	teamster_session_active{session_id="X",host="h1",team_name="ops",agent_name="@scout",outcome_id="O",workunit_id="U"} 1 1 1

eval instant at 2m sum by (outcome_id, agent_name) (teamster_session_active * on (session_id) group_left() sum by (session_id) (rate(claude_code_token_usage_tokens_total[2m])))
	{agent_name="@scout",outcome_id="O"} 3.1666666666666665
	{outcome_id="O"} 3.1666666666666665
`

	promqltest.RunTest(t, fixture, engine)
}

// TestJoinProducesDistinctAgentRows verifies the one-to-many join produces one
// result row per agent_name value (lead + teammate). Uses bridge gauge * 1
// instead of a rate() to avoid rate-window edge cases in the test fixture.
func TestJoinProducesDistinctAgentRows(t *testing.T) {
	engine := promqltest.NewTestEngine(t, false, 0, 50000000)

	// Multiply bridge gauge by a scalar 1 (via a single-row metric) to confirm
	// both (session_id, agent_name) pairs survive the group_left join.
	const fixture = `
load 5m
	teamster_session_active{session_id="S",host="h",team_name="",agent_name="",outcome_id="",workunit_id=""} 1 1 1
	teamster_session_active{session_id="S",host="h",team_name="",agent_name="@store",outcome_id="",workunit_id=""} 1 1 1
	probe_scalar{session_id="S"} 1 1 1

eval instant at 5m sum by (agent_name) (teamster_session_active * on (session_id) group_left() probe_scalar)
	{agent_name="@store"} 1
	{} 1
`

	promqltest.RunTest(t, fixture, engine)
}

// TestBridgeGaugeCarriesAllLabels verifies that the bridge gauge emits all
// required labels from SPEC §4.1 and that they can be selected by any label.
func TestBridgeGaugeCarriesAllLabels(t *testing.T) {
	engine := promqltest.NewTestEngine(t, false, 0, 50000000)

	const fixture = `
load 1m
	teamster_session_active{session_id="S1",host="h",team_name="ops",agent_name="",outcome_id="o1",workunit_id="u1"} 1
	teamster_session_active{session_id="S1",host="h",team_name="ops",agent_name="@scout",outcome_id="o1",workunit_id="u1"} 1

eval instant at 0 teamster_session_active{host="h",team_name="ops",outcome_id="o1"}
	teamster_session_active{session_id="S1",host="h",team_name="ops",agent_name="",outcome_id="o1",workunit_id="u1"} 1
	teamster_session_active{session_id="S1",host="h",team_name="ops",agent_name="@scout",outcome_id="o1",workunit_id="u1"} 1
`

	promqltest.RunTest(t, fixture, engine)
}

// TestLeadAgentNameIsEmpty verifies that filtering by agent_name="" returns
// only the lead row and not teammate rows.
func TestLeadAgentNameIsEmpty(t *testing.T) {
	engine := promqltest.NewTestEngine(t, false, 0, 50000000)

	const fixture = `
load 1m
	teamster_session_active{session_id="S",host="h",team_name="",agent_name="",outcome_id="",workunit_id=""} 1
	teamster_session_active{session_id="S",host="h",team_name="",agent_name="@scout",outcome_id="",workunit_id=""} 1
	teamster_session_active{session_id="S",host="h",team_name="",agent_name="@store",outcome_id="",workunit_id=""} 1

eval instant at 0 count(teamster_session_active{agent_name=""})
	{} 1

eval instant at 0 count(teamster_session_active{agent_name!=""})
	{} 2
`

	promqltest.RunTest(t, fixture, engine)
}

// TestPromqltestEngineStartsWithoutPanic is a smoke test verifying the
// engine initializes cleanly in test context.
func TestPromqltestEngineStartsWithoutPanic(t *testing.T) {
	engine := promqltest.NewTestEngine(t, false, 0*time.Second, 50000000)
	if engine == nil {
		t.Fatal("expected non-nil engine")
	}
}

// TestNewTestEngineOpts verifies we can construct with explicit EngineOpts.
func TestNewTestEngineOpts(t *testing.T) {
	engine := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples:    50000000,
		Timeout:       30 * time.Second,
		LookbackDelta: 5 * time.Minute,
	})
	if engine == nil {
		t.Fatal("expected non-nil engine from opts")
	}
}

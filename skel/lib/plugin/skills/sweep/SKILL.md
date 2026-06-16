---
name: sweep
description: "Nightly data-quality sweep: run deterministic recovery passes, then synthesize outcomes for orphan sessions via targeted transcript reads. Designed for `claude --print` (headless, autonomous, no operator interaction)."
disable-model-invocation: true
argument-hint: "[--dry-run]"
---

# Nightly Attribution Sweep

You are an autonomous background agent running the nightly data-quality sweep.
You have NO operator to interact with. Every decision must be self-contained.
When in doubt, skip and log — never block waiting for input.

This skill is invoked by a systemd timer via `claude --print`. It runs after
the deterministic `rollup --sweep` has already executed. Your job is the
**LLM-assisted phase**: synthesizing WMS Outcomes for orphan sessions that the
deterministic passes cannot attribute.

## Quick reference

| Item | Value |
|------|-------|
| Standing outcome | `out-sweep-nightly` (create if absent) |
| Tags on that outcome | `product:Teamster`, `feature:rollup`, `work-type:admin`, `component:wms` |
| Mapping file | `/tmp/sweep-llm-mapping-YYYY-MM-DD.json` |
| Max sessions per run | 10 |
| Method label | `synthesized_outcome` (reuses existing) |
| Rollup binary | `rollup` |
| SQL query path | `teamster sql` (reads `$TEAMSTER_STORE_DSN` in-process) |

---

## Step 0 — Self-report to WMS

Before any work, attribute your own cost:

```
wms_setFocus(entityType="outcome", entityID="out-sweep-nightly",
             focus="nightly LLM attribution sweep")
```

If `out-sweep-nightly` does not exist yet, create it first:

```
wms_createOutcome(id="out-sweep-nightly",
                  title="Nightly Attribution Sweep",
                  description="Standing outcome for automated data-quality sweeps")
wms_updateOutcomeStatus(id="out-sweep-nightly", status="active")
wms_tagEntity(entityType="outcome", entityID="out-sweep-nightly",
              tagKey="product", tagValue="Teamster", source="manual")
wms_tagEntity(entityType="outcome", entityID="out-sweep-nightly",
              tagKey="feature", tagValue="rollup", source="manual")
wms_tagEntity(entityType="outcome", entityID="out-sweep-nightly",
              tagKey="work-type", tagValue="admin", source="manual")
wms_tagEntity(entityType="outcome", entityID="out-sweep-nightly",
              tagKey="component", tagValue="wms", source="manual")
```

Then set focus on it. Keep this focus for the entire run.

---

## Step 1 — Run the deterministic sweep (if not already run)

Check whether the deterministic sweep ran today:

```bash
journalctl --user -u teamster-sweep.service --since today --no-pager | tail -5
```

If it ran, skip to Step 2. If not, run it:

```bash
rollup --sweep 2>&1 | tail -20
```

This chains: allocate, recover-focus, recover-warmup, recover-gaps. All
deterministic, no LLM. Wait for it to complete before proceeding.

---

## Step 2 — Identify orphan sessions

Query the DB for sessions with `method='unallocated'` rows, then check each
transcript for actual `wms_setFocus` tool_use blocks:

```bash
# Get unallocated sessions, cost-descending.
# `teamster sql` reads the DSN (including the password) from
# $TEAMSTER_STORE_DSN in-process — never put the password on the command line.
teamster sql -N -e "
  SELECT t.session_id, t.host, t.username,
         COUNT(*) AS msgs, ROUND(SUM(t.cost_usd), 2) AS cost
  FROM usage_attribution ua
  JOIN token_ledger t ON t.message_id = ua.message_id
  WHERE ua.method = 'unallocated'
  GROUP BY t.session_id, t.host, t.username
  ORDER BY cost DESC
  LIMIT 30;
"
```

For each session, check if it's a true orphan (no actual setFocus tool_use):

```bash
# Find the transcript file
found=$(find ~/.claude/projects/ -name "${session_id}.jsonl" 2>/dev/null | head -1)

# Check for REAL tool_use blocks (not text mentions)
python3 -c "
import json, sys
count = 0
with open('$found') as f:
    for line in f:
        try:
            obj = json.loads(line.strip())
            msg = obj.get('message', {})
            content = msg.get('content', [])
            if isinstance(content, list):
                for c in content:
                    if isinstance(c, dict) and c.get('type') == 'tool_use' \
                       and 'setFocus' in str(c.get('name', '')):
                        count += 1
        except: pass
print(count)
"
```

A session is an orphan iff: it has unallocated rows AND the tool_use count is 0
AND the transcript file exists on this host.

Collect up to **10 orphans** (cost-descending). If fewer than 10 qualify, that's
fine — process what exists.

---

## Step 3 — Read transcript heads and synthesize

For each orphan session, read the first ~200 lines and extract user/assistant
messages to understand the session objective:

```bash
head -200 "$transcript_file" | python3 -c "
import sys, json
for line in sys.stdin:
    try:
        obj = json.loads(line.strip())
        if obj.get('type') in ('user', 'assistant'):
            msg = obj.get('message', {})
            role = msg.get('role', '?')
            content = msg.get('content', '')
            if isinstance(content, list):
                texts = [c.get('text','') for c in content
                         if isinstance(c, dict) and c.get('type') == 'text']
                content = ' '.join(texts)
            if isinstance(content, str) and len(content.strip()) > 10:
                # Skip system-reminder injections
                if not content.strip().startswith('<system-reminder'):
                    print(f'{role}: {content[:300]}')
    except: pass
"
```

From the transcript excerpt, synthesize:

1. **outcome_id**: `out-synth-<short-kebab-slug>` (unique, descriptive)
2. **title**: 1-line description of the session's objective
3. **description**: 1-2 sentences on what the session accomplished
4. **Tags** (see Step 3a for guidance on each)

### Step 3a — Tag selection rules

**BEFORE synthesizing any tags**, call `wms_listTags` to see the full vocabulary
with descriptions. Each tag value has a `description` field that tells you
**when to apply it**. Read these descriptions and match — do not guess.

**`product`** (context, single — pick ONE):
- Reuse an existing value if the session's working directory or content matches
- Common values: `Teamster` (this repo), `homelab` (monitoring/infra/smart-home),
  `ScrollZ` (IRC client), `anchor` (IRC coordination harness), `job-search`
- Only propose a new value if genuinely new and reusable across future sessions

**`work-type`** (lifecycle, required — pick ONE):
- `feature` — adds a NEW capability that didn't exist before
- `bug` — fixes incorrect EXISTING behavior
- `refactor` — restructures code without changing external behavior
- `infra` — infrastructure, provisioning, CI, host setup
- `research` — investigation/audit whose output is knowledge, not code
- `docs` — documentation as the deliverable
- `test` — validation run producing a pass/fail verdict

**`feature` or `bug`** (context, single — pick ONE or neither):
- Mutually exclusive. Set `feature:<slug>` for feature/refactor/infra work
- Set `bug:<slug>` for bug work
- The slug should be short, kebab-case, reusable (e.g. `monitoring-stack`,
  `remote-installer-path`, NOT `fix-the-thing-john-asked-about`)
- Check existing values first — reuse if one fits

**`component`** (context, single — optional):
- The architectural component touched (e.g. `wms`, `dashboard`, `monitoring`,
  `cli`, `harness`, `feed`, `skills`)
- Only set if clearly identifiable from the transcript

**`priority`** (context, single — default `p2`):
- `p0` = emergency, `p1` = high, `p2` = normal, `p3` = low
- Default to `p2` unless the transcript signals urgency

### Step 3b — Confidence assessment

Rate each synthesis:
- **high** — user's opening message clearly states the objective
- **medium** — objective inferred from tool calls and assistant responses
- **low** — ambiguous, multiple possible interpretations

Skip (don't synthesize) if confidence is too low to be useful.

---

## Step 4 — Create WMS outcomes and apply tags

For each synthesized outcome, create it in WMS and apply all tags:

```
wms_createOutcome(id="out-synth-<slug>",
                  title="<title>",
                  description="<description>",
                  status="done")

wms_tagEntity(entityType="outcome", entityID="out-synth-<slug>",
              tagKey="source", tagValue="synthesized", source="classifier")
wms_tagEntity(entityType="outcome", entityID="out-synth-<slug>",
              tagKey="product", tagValue="<product>", source="manual")
wms_tagEntity(entityType="outcome", entityID="out-synth-<slug>",
              tagKey="work-type", tagValue="<work-type>", source="manual")
wms_tagEntity(entityType="outcome", entityID="out-synth-<slug>",
              tagKey="<feature|bug>", tagValue="<slug>", source="manual")
wms_tagEntity(entityType="outcome", entityID="out-synth-<slug>",
              tagKey="priority", tagValue="<priority>", source="manual")
wms_tagEntity(entityType="outcome", entityID="out-synth-<slug>",
              tagKey="resolution", tagValue="achieved", source="manual")
```

**Do NOT create duplicate outcomes.** Before creating, check if
`out-synth-<slug>` already exists:

```bash
teamster sql -N -e "SELECT id FROM outcomes WHERE id = 'out-synth-<slug>'"
```

If it exists, skip creation (a prior sweep already handled this session).

---

## Step 5 — Produce the mapping file

Write a JSON mapping file that `rollup --synthesize-focus` can consume:

```json
[
  {
    "session_id": "full-uuid-here",
    "entity_type": "outcome",
    "entity_id": "out-synth-<slug>",
    "confidence": "high",
    "evidence_excerpt": "User: <the key line from the transcript>"
  }
]
```

Write to `/tmp/sweep-llm-mapping-$(date +%F).json`.

---

## Step 6 — Apply attributions

Run the synthesize-focus pass:

```bash
rollup --synthesize-focus=/tmp/sweep-llm-mapping-$(date +%F).json 2>&1 | tail -10
```

This does the actual DB writes — in-place UPDATE of `usage_attribution` rows
from `method='unallocated'` to `method='synthesized_outcome'`, with provenance
in `synthesis_evidence`.

---

## Step 7 — Verify conservation

`teamster sql` runs one statement per call, so issue the two sums separately:

```bash
teamster sql -e "SELECT ROUND(SUM(cost_usd), 4) AS ledger FROM token_ledger;"
teamster sql -e "
  SELECT ROUND(SUM(t.cost_usd * COALESCE(ua.weight, 1)), 4) AS facts
  FROM token_ledger t
  LEFT JOIN usage_attribution ua ON ua.message_id = t.message_id;
"
```

The two numbers MUST match exactly. If they don't, something went wrong —
log the delta and do NOT run again. The operator investigates.

---

## Step 8 — Log results

Print a summary to stdout (this is captured by systemd journal):

```
sweep-llm complete: orphans_processed=N synthesized=N skipped=N
  conservation: $X == $X (delta $0.00)
  methods: synthesized_outcome=N gap_recovery=N unallocated=N
  remaining: N sessions / $X unallocated
```

---

## Rules (non-negotiable)

1. **No operator interaction.** You are headless. Never use AskUserQuestion or
   wait for input. If uncertain, skip the session and log why.
2. **Max 10 sessions per run.** Process cost-descending. The cap bounds cost
   and runtime. Tomorrow's sweep gets the next batch.
3. **Conservation is sacred.** If the conservation check fails, stop everything
   and log the error. Never retry — the operator investigates.
4. **Reuse tag values.** Always call `wms_listTags` before inventing new values.
   Read each value's `description` to decide if it fits. A near-duplicate is
   worse than reusing an imperfect match.
5. **source:synthesized on every created outcome.** This is how dashboards
   distinguish LLM-inferred from human-declared attribution. Never omit it.
6. **Don't re-process.** If a session already has `method='synthesized_outcome'`
   rows, the prior sweep handled it. Skip.
7. **Host-local only.** Only process sessions whose transcript file exists on
   this host. Don't try to read remote transcripts.
8. **Log, don't crash.** If a transcript is unreadable, a DB query fails, or
   an outcome can't be created — log it, skip it, continue to the next session.

---

## Examples

### Example 1: Clear objective (high confidence)

Transcript head:
```
user: Read the anchor-initial-spec.rtf
assistant: I'll read the spec file...
[tool calls to read the file, extensive analysis follows]
```

Synthesis:
- outcome_id: `out-synth-anchor-spec-review`
- title: "Anchor IRC Harness — Initial Spec Review"
- product: `anchor`
- work-type: `research`
- feature: `anchor-design`
- priority: `p2`
- confidence: `high`
- evidence: "User: Read the anchor-initial-spec.rtf"

### Example 2: Monitoring setup (high confidence)

Transcript head:
```
user: You are going to be fixing the monitoring stack on this system, along
      with setting up some nifty dashboards for monitoring
```

Synthesis:
- outcome_id: `out-synth-monitoring-setup`
- title: "Homelab Monitoring Stack Setup"
- product: `homelab`
- work-type: `infra`
- feature: `monitoring-stack`
- priority: `p2`
- confidence: `high`

### Example 3: Ambiguous (skip)

Transcript head:
```
user: /effort max
user: /compact
[no substantive user message in first 200 lines]
```

Action: skip this session (no objective to synthesize).

---

## Invocation

The systemd timer runs this after the deterministic sweep:

```bash
cd /path/to/teamster/repo && \
  claude --print -p "/teamster:sweep" \
  --allowedTools "Bash,Read,Write,mcp__wms__*,mcp__activity__*"
```

Or equivalently with the prompt inlined:

```bash
claude --print -p "Read $TEAMSTER_BASEDIR/lib/plugin/skills/sweep/SKILL.md and follow every step."
```

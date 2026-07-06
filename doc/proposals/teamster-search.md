# Proposal: `teamster search sessions` + `wms.Search` — WMS-backed discovery

Status: **Draft / for review (rev 3 — session-centric CLI, primitive/composition split)**
Author: Claude (opus) via /teamster:solo on `hub01`
Related: WMS outcome `teamster-searchable-session-titles` (feature:searchable-session-titles)

## 1. Problem

Claude Code's `--resume` picker cannot find a session by what it was *about*. It
titles/searches on a per-session display **name** (default `nameSource: "derived"`,
from the cwd) or the first user message — which for a `/teamster:start` session is
identical `<local-command-caveat>` / SKILL.md boilerplate. So a search for
`gastown` (which appeared 670× in the transcript body) returns **nothing**.

This is not fixable inside the skill: Claude Code exposes **no agent-invocable way
to set a session title after the interview**. `/rename` is human-UI-only, has a
history of persistence bugs (anthropics/claude-code #25090, #36077), `--resume`
doesn't search by it (#43963), and an MCP `rename_session` tool is an *open,
unshipped* request (#67680). The only reliable native setter is `claude -n <name>`
at **startup** — before the focus is known.

Teamster already holds the ground truth for what every session worked on — outcomes,
work units, tags, and focus intervals, keyed by session/host/user in the central
store. We **externalize discovery there** instead of fighting Claude Code's picker.

## 2. Goal / non-goals

**Goal.** Find "the **session(s)** that worked on X" across every host and user
reporting to the store, then resume by session id — the operator's primary need.
**Multi-operator is a hard requirement**: the store aggregates many operators
across many hosts, so `user` and `host` are first-class result *and* filter
dimensions, and search is **cross-operator by default** (any operator sees the
team's sessions; `--user`/`--host` narrow). Built on a reusable WMS search
primitive so agents get the same power via MCP.

**Non-goals.**
- Not changing Claude Code's native `--resume` picker (out of our control).
- **Metadata return only** — WMS metadata, no transcript-body full-text search.
- No filesystem `cwd` / ready `claude --resume` line in v1 — cwd isn't in WMS
  (only `host` + `session_id`). See §9.

## 3. Architecture: primitive → composition → fronts

Per review, the generic search and the session-search use case are **separate
layers**. The CLI's default perspective is *session-centric* (one row per session);
that projection is a named composition over a generic primitive, not baked in.

```
  ┌──────────────────────────────────────────────────────────────────────┐
  │ L1  PRIMITIVE  internal/wms.Search(ctx, SearchQuery) → []Hit          │
  │      generic; granular hits (per matching entity / focus interval,    │
  │      each carrying user·host·session·agent·entity·when·why)           │
  └───────────────┬───────────────────────────────────┬──────────────────┘
                  │                                     │
  ┌───────────────▼─────────────────────┐   ┌──────────▼──────────────────┐
  │ L2  COMPOSITION                      │   │  wms_search  (MCP tool)     │
  │  internal/wms.SearchSessions(ctx,q)  │   │  exposes the L1 primitive    │
  │   → []SessionMatch                   │   │  raw to agents / the model   │
  │   groups Hits by session_id:         │   └──────────────────────────────┘
  │   ONE ROW PER SESSION                 │
  └───────────────┬──────────────────────┘
                  │
  ┌───────────────▼──────────────────────┐
  │ L3  FRONT   teamster search sessions  │  renders []SessionMatch as
  │             <query> [--json]          │  table / JSON. thin.
  └───────────────────────────────────────┘
```

- **L1 `Search` (primitive)** — generic engine. Returns granular `Hit`s (one per
  matching entity or focus interval, with full attribution). Reusable for *any*
  search projection. This is what the `wms_search` MCP tool exposes.
- **L2 `SearchSessions` (composition)** — the session-search use case: calls
  `Search`, then **groups Hits by `session_id`** to emit one `SessionMatch` per
  session. Lives in `internal/wms` (not the CLI) so it's reusable and testable.
- **L3 fronts** — `teamster search sessions` renders `SearchSessions`; `wms_search`
  exposes `Search`. `search` is a **command group**; `sessions` is the first
  subcommand (future: `teamster search outcomes`, `search workunits`, each its own
  projection over the same L1 primitive).

## 4. Data model (what we join)

All columns exist today (`internal/store/mysql/migrations.go`) — **no migration.**

| Table | Search-relevant columns | Role |
|---|---|---|
| `outcomes` | `id`, `title`, `description`, `status`, `focus`, `origin_host`, `origin_session`, `origin_agent`, `updated_at` | entity + creating session/host/agent |
| `workunits` | `id`, `outcome_id`, `title`, `description`, `status`, `agent_id`, `focus`, `origin_host`, `origin_session`, `origin_agent`, `updated_at` | entity + creating session/host/agent |
| `entity_tags` + `tags` | `entity_type`, `entity_id`, `tag_key`, `tag_value` | tag matches (`product`, `feature`, `research`, **`user`**, …) |
| `sessions` | `session_id`, `agent_name`, `host`, `team_name`, `focus`, `first_seen`, `last_seen`, `status` | session → host / recency (the L2 grouping key) |
| `wms_intervals` | `kind='focus'`, `session_id`, `agent_name`, `entity_type`, `entity_id`, `host`, `phase`, `started_at`, `ended_at` | **the focus-interval surface**: which session/host/agent worked on which entity |

`user` is the (engine-managed) `entity_tags(tag_key='user')` dimension; `host` is a
first-class column. `wms_intervals` is the primary driver for session search: a
session matches when one of its **focus intervals** ties it to a matching entity.

## 5. Matching semantics

**L1 `Search` — find granular hits** (case-insensitive `LIKE %q%`), gated by
`--type=outcomes,workunits,focus,all` (default `all`):
- `outcomes` → `outcomes.{id,title,description,focus}` (+ tag values via `entity_tags`)
- `workunits` → `workunits.{id,title,description,focus}` (+ tag values)
- `focus` → `wms_intervals` + `sessions.focus` — the sessions/agents that *worked
  on* entities, and their focus strings
- `all` → union

Each `Hit` carries: `user, host, session_id, agent_name, entity_type, entity_id,
title, status, when, match[]`.

**L2 `SearchSessions` — group by session.** Collapse Hits to **one row per
`session_id`**: a session qualifies if any of its focus intervals (or its
created/touched entities under the active `--type`) matched. The `SessionMatch`
aggregates the matched entities for that session:

```
SessionMatch{ User, Host, SessionID, Team, LastSeen, Status,
              Matched []EntityRef,   // the entity id(s) that hit, with why
              FocusSummary string }  // sessions.focus / most-recent focus
```

## 6. CLI surface — `teamster search sessions`

```
teamster search sessions <query> [flags]

flags:
  --type <list>    outcomes,workunits,focus,all  (default all; focus is primary)
  --user <u>       filter to a user
  --host <h>       filter to a host
  --status <s>     filter session status (active, idle, closed)
  --tag key=value  exact tag filter, repeatable (AND)
  --since <dur>    sessions active within window (e.g. 72h)
  --limit <n>      cap rows (default 50)
  --json           machine-readable []SessionMatch (agents / scripts / dashboard)
```

**One row per session. SESSION is never truncated** — it is the primary extraction,
so the full session id always prints in full (table and JSON). The matched entity
id(s) ellipsize with a `+N` overflow; session ids never.

```
$ teamster search sessions gastown
USER  HOST    SESSION                               MATCHED                              WHEN
bj    hub01   45cc474f-f08d-4988-b60c-d3f33e9d3bab   outcome:gastown-integration (+2)     12h ago
bj    studio  892e1187-6361-4a2e-9f0c-1b2c3d4e5f60   workunit:gs-events-costagg           3d ago
2 sessions · 2 hosts
```

(No `team`/`phase` columns — per review.) `--json` emits the `[]SessionMatch`
array, each row including the full `matched` list of `{entity_type, entity_id,
why}`.

## 7. `wms_search` MCP tool (exposes the L1 primitive)

Agents get the **granular** primitive, not the session projection — they can group
however they need (and the model running `/teamster:start` can ask "what past work
matches X?" against the whole store).

```json
{"name":"wms_search","input_schema":{"type":"object","required":["query"],
 "properties":{
   "query":{"type":"string"},
   "type":{"type":"string","description":"comma list: outcomes,workunits,focus,all"},
   "user":{"type":"string"}, "host":{"type":"string"}, "status":{"type":"string"},
   "session":{"type":"string"}, "tag":{"type":"array","items":{"type":"string"}},
   "since":{"type":"string"}, "limit":{"type":"integer"}}}}
```

Returns `[]Hit`. Registered in `internal/mcp/wms/names.go` (`ToolSearch =
"wms_search"`) + a case in `internal/mcp/wms/wms.go`; served by existing
`cmd/wms-mcp`. (A session-grouped `wms_searchSessions` over `SearchSessions` is a
trivial add later if agents want the rollup too — deferred.)

## 8. Implementation sketch

- `internal/wms`: `SearchQuery`, `Hit`, `SessionMatch`, `EntityRef` types;
  `Search()` (one match CTE per enabled `--type`, unioned, + attribution join) and
  `SearchSessions()` (group `Search` output by `session_id`). No schema change.
- `cmd/teamster/search.go`: `runSearch` (group) dispatches `sessions` →
  `runSearchSessions` → `wms.SearchSessions` → tabwriter / `--json`.
- `main.go`: `case "search": return runSearch(rest)` + help line.
- `internal/mcp/wms/{names.go,wms.go}`: `wms_search` → `wms.Search`.
- Tests: table-driven against the MySQL test harness — seed
  outcomes/workunits/tags/intervals across two hosts+users; assert (a) `Search`
  granular hits + `--type` gating, (b) `SearchSessions` collapses to one row per
  session with the right `matched` set.

**Estimated size:** `Search` (~120 LOC) + `SearchSessions` (~50) + `search.go`
(~120) + MCP wiring (~40) + `main.go`/help (~10) + tests. No migration.

## 9. Future work (out of v1 scope)

- **cwd / ready resume command.** Add a `cwd` column to `sessions` from the hook
  payload (or local `session_id → ~/.claude/projects/<encoded-cwd>/` enrichment) to
  turn a row into `cd <cwd> && claude --resume <id>`.
- **Other projections** — `teamster search outcomes|workunits` over the same L1.
- **`wms_searchSessions` MCP tool** if agents want the session rollup.
- **Relevance ranking**; **dashboard + `tm search`** surfaces.

## 10. Decisions from review

| # | Decision |
|---|---|
| a | **No `team`/`phase` columns** in the default table. ✔ |
| b | **Session-centric default**: `teamster search **sessions**` returns **one row per session** where a focus interval (or matched entity, per `--type`) ties to the query. `search` is a command group for future projections. ✔ |
| c | **Primitive/composition split**: generic `wms.Search` (L1, granular, = `wms_search` MCP) vs. `wms.SearchSessions` (L2, session rollup, = CLI). ✔ |
| d | SESSION never truncated; metadata-only v1; `--json` on both fronts. ✔ (rev 2) |
| e | **Multi-operator is required.** `user`/`host` are first-class result+filter dimensions; **cross-operator visibility by default**; per-session identity resolved from the engine-managed `user` tag + interval `identity_source`. Per-operator access *restriction* (if ever needed) is a store-auth concern, out of scope for v1. ✔ |

| f | For `--type=focus` **focus-string** matches with no specific entity, **show the focus text itself** in `MATCHED` (don't drop the session — a focus-string hit is a legitimate "this session was about X" signal). ✔ |

**All questions resolved — proceeding to implementation** (agent team: builders + reviewer).

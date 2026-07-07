package sqlite

import "github.com/bmjdotnet/teamster/internal/store"

// migrations is the SQLite-dialect migration list. It mirrors
// internal/store/mysql/migrations.go's (Version, Name) pairs exactly — same
// 50-slot numbering (with the same gaps at v2/v22/v24 mysql's own list
// carries), so highestKnownVersion() here also returns 50 and a "seed at an
// older version, then run forward" test can use the same v44 cut point the
// mysql conformance_dim5 test uses.
//
// Strategy (deliberately simpler than replaying 50 versions of ALTERs
// verbatim — see 03-architecture/04-migrations.md's Invariant 1, which binds
// a backend with DEPLOYED history to byte-for-byte replay; this backend has
// never been deployed, so that invariant does not apply here):
//
//  1. At the version where mysql FIRST creates a table this backend needs,
//     emit ONE "CREATE TABLE IF NOT EXISTS" carrying the table's COMPLETE
//     HEAD (v50) column set — not just the columns mysql had at that first
//     version — plus every index that table will ever have, expressed as
//     separate CREATE INDEX/CREATE UNIQUE INDEX statements (SQLite has no
//     inline INDEX(...) clause).
//  2. Every LATER version where mysql did "ALTER TABLE ADD COLUMN" (or
//     widened a column, or added an index) on a table already front-loaded
//     here becomes an EMPTY step {Version, Name} — a legitimate no-op since
//     the column/index already exists.
//  3. DATA-mutating steps (INSERT/UPDATE/DELETE seed rows, tag-vocabulary
//     backfills, etc.) are NOT no-ops — they are replayed at their ORIGINAL
//     version number, translated to SQLite dialect, because front-loading a
//     table's CREATE only gives it an empty shape; the seed rows a live
//     install accumulates over 50 versions must still be inserted for this
//     backend's tags/transition_rules tables to reach the same catalog a
//     fresh mysql v50 install has.
//  4. The 3 Func steps in mysql (backfillV1ToV3 @16, backfillWmsIntervals
//     @23, mergeProjectToProduct @27) migrate LEGACY DATA from an
//     old-shaped table into a new one. On mysql itself these are no-ops on a
//     genuinely fresh install (the v1 tables / wms_event_records /
//     agent_focus_intervals are empty until an operator or a running system
//     populates them) — EXCEPT mergeProjectToProduct, whose input (a
//     tag_key='project' row) is itself produced by this same migration
//     history's OWN v14 seed step, not by "legacy data". Rather than
//     replay-then-immediately-merge-away a transient tag_key, this file
//     simply never seeds 'project' at v14 (see that step's comment) — so
//     mergeProjectToProduct has nothing to do here either, and all 3 Func
//     steps are legitimately empty. This is the one judgment call in this
//     file that changes an intermediate value from what mysql's DML
//     literally does; the converged HEAD STATE (only 'product' survives) is
//     identical either way.
//  5. Steps that are pure MySQL admin/ops with no schema meaning here
//     (CREATE EVENT retention jobs) are no-ops.
//  6. Two reporting-layer VIEWs mysql defines (cost_facts @29,
//     entity_tags_resolved @29/41) are NOT created here — see the v29/v41
//     step comments and this package's migrations_test.go doc comment for
//     why, and what a future SQLite ReportingStore/AllocationStore
//     implementer should know.
//
// Dialect notes (applied throughout): VARCHAR(n)/TEXT -> TEXT (no length
// limit in SQLite); BIGINT ... AUTO_INCREMENT PRIMARY KEY -> INTEGER PRIMARY
// KEY (SQLite's ROWID alias — only the bare spelling "INTEGER PRIMARY KEY"
// triggers the alias, so id columns always use exactly that, never BIGINT);
// ENUM(...) -> TEXT with an optional CHECK; DATETIME(6)/DATETIME -> DATETIME
// (decltype sniffing on this driver auto-converts DATETIME/DATE-declared
// columns to time.Time on Scan — see store.go's New doc comment — so DATE
// stays DATE, not TEXT); ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 -> dropped;
// TINYINT(1) -> INTEGER (Go bool binds/scans transparently against INTEGER
// on both backends); DECIMAL(p,s) -> REAL (SQLite has no fixed-point decimal
// type; every Go-side consumer of these columns already uses float64 —
// store.TelemetryRow.CostUSD, store.Interval.CostUSD, etc. — so there is no
// Go-side precision loss, but SQL-side arithmetic becomes floating point
// rather than exact decimal; flagged in this package's outstanding-risks
// notes for whoever writes the SQLite AllocationStore/ReportingStore
// aggregation queries). Inline "INDEX name (cols)" / "UNIQUE KEY name
// (cols)" -> separate named "CREATE [UNIQUE] INDEX IF NOT EXISTS" statements
// (checked for cross-table name collisions against the full mysql index
// namespace — none exist, so every index keeps its mysql name for parity).
var migrations = []store.Migration{
	{
		// wms-core: mysql's v1 creates the v1 entity tables (projects, goals,
		// tasks, work_items, work_dependencies). Confirmed genuinely dead: a
		// repo-wide grep for FROM/INTO of any of these five table names
		// outside internal/store/mysql/migrations.go and its own tests turns
		// up nothing — no production Go code reads or writes them (v17
		// archives them under archived_v1_* on mysql; v16's backfill fans
		// them out into Outcomes/WorkUnits; neither has any legacy state to
		// migrate on a backend with no deployed history). Not created here.
		Version: 1,
		Name:    "wms-core",
	},
	{
		// sessions-activity: both tables are alive (store.SessionStore /
		// store.ActivityStore). Front-loaded with their full head shape:
		// sessions gains `username` at mysql v34, folded in here. runtime/cwd/
		// model/originator/cli_version added at mysql v51 (codex-support) are
		// also folded in here.
		Version: 3,
		Name:    "sessions-activity",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS sessions (
				session_id  TEXT NOT NULL,
				agent_name  TEXT NOT NULL DEFAULT '',
				host        TEXT NOT NULL DEFAULT '',
				team_name   TEXT NOT NULL DEFAULT '',
				project_id  TEXT NOT NULL DEFAULT '',
				goal_id     TEXT NOT NULL DEFAULT '',
				task_id     TEXT NOT NULL DEFAULT '',
				workitem_id TEXT NOT NULL DEFAULT '',
				focus       TEXT NOT NULL DEFAULT '',
				username    TEXT NOT NULL DEFAULT '',
				first_seen  DATETIME NOT NULL,
				last_seen   DATETIME NOT NULL,
				status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','idle','closed')),
				runtime     TEXT NOT NULL DEFAULT 'claude',
				cwd         TEXT NOT NULL DEFAULT '',
				model       TEXT NOT NULL DEFAULT '',
				originator  TEXT NOT NULL DEFAULT '',
				cli_version TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (session_id, agent_name)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_host ON sessions(host)`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_team ON sessions(team_name)`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_last_seen ON sessions(last_seen)`,
			`CREATE TABLE IF NOT EXISTS activity_events (
				id          INTEGER PRIMARY KEY,
				session_id  TEXT NOT NULL,
				agent_name  TEXT NOT NULL DEFAULT '',
				host        TEXT NOT NULL DEFAULT '',
				tag         TEXT NOT NULL,
				display     TEXT NOT NULL,
				focus       TEXT NOT NULL DEFAULT '',
				timestamp   DATETIME NOT NULL,
				CONSTRAINT fk_activity_session FOREIGN KEY (session_id, agent_name)
					REFERENCES sessions(session_id, agent_name) ON DELETE CASCADE
			)`,
			`CREATE INDEX IF NOT EXISTS idx_activity_pair_time ON activity_events(session_id, agent_name, timestamp)`,
			`CREATE INDEX IF NOT EXISTS idx_activity_time ON activity_events(timestamp)`,
		},
	},
	{
		// transition-rules: alive (Store.RoleAllowed); columns never change
		// across mysql's history.
		Version: 4,
		Name:    "transition-rules",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS transition_rules (
				entity_type   TEXT NOT NULL,
				old_status    TEXT NOT NULL,
				new_status    TEXT NOT NULL,
				required_role TEXT NOT NULL DEFAULT '*',
				PRIMARY KEY (entity_type, old_status, new_status, required_role)
			)`,
		},
	},
	{
		// wms-journal: alive (Store.GetJournalEntries/WriteJournalEntry,
		// wms.JournalObserver). NOTE the one deliberate exception to "no DDL
		// DEFAULT clauses on timestamps": mysql's WriteJournalEntry does NOT
		// supply created_at in its INSERT — it relies on the column's
		// DEFAULT CURRENT_TIMESTAMP (confirmed in the golden schema fixture:
		// wms_journal.created_at has extra=DEFAULT_GENERATED). SQLite
		// supports the same DEFAULT CURRENT_TIMESTAMP keyword, producing a
		// "YYYY-MM-DD HH:MM:SS" UTC string this driver's DATETIME decltype
		// sniffing parses back into time.Time on Scan — so the SQLite
		// backend's WriteJournalEntry can mirror mysql's insert (omit
		// created_at) with the same effect. Flagged for the CRUD
		// implementer in this package's report.
		Version: 5,
		Name:    "wms-journal",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS wms_journal (
				id           INTEGER PRIMARY KEY,
				entity_type  TEXT NOT NULL,
				entity_id    TEXT NOT NULL,
				field        TEXT NOT NULL,
				old_value    TEXT,
				new_value    TEXT,
				agent_id     TEXT,
				host         TEXT,
				session_id   TEXT,
				notes        TEXT,
				created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`,
			`CREATE INDEX IF NOT EXISTS idx_entity ON wms_journal(entity_type, entity_id)`,
		},
	},
	{
		// cost-columns: ALTERs work_items/tasks — both v1 tables, dead here (never created).
		Version: 6,
		Name:    "cost-columns",
	},
	{
		// plane-sync: creates plane_sync. Confirmed dead — Plane integration
		// was dropped 2026-06-02 (mysql v17 archives it; no live code
		// references it, not even in comments beyond migrations.go/tests).
		// Never created here.
		Version: 7,
		Name:    "plane-sync",
	},
	{
		// plane-sync-project-id: ALTERs the dead plane_sync table.
		Version: 8,
		Name:    "plane-sync-project-id",
	},
	{
		// event-records: creates wms_event_records (kind='state' predecessor
		// of wms_intervals, created at v21 below) and backfills it from the
		// dead v1 entity tables. Entirely superseded by wms_intervals; mysql
		// itself archives this table at v25 and no live code reads it after
		// that. Never created here — a fresh SQLite install goes straight to
		// wms_intervals.
		Version: 9,
		Name:    "event-records",
	},
	{
		// token-ledger: alive (store.TelemetryStore). Front-loaded with the
		// full head shape — the telemetry columns mysql adds at v11
		// (message_id, cache_write_1h/5m, n_text/n_tool_use/n_thinking,
		// total_input, stop_reason, service_tier, speed) and username added
		// at v34 are folded in here.
		// runtime/reasoning_output_tokens added at mysql v51 (codex-support) are also folded in here.
		Version: 10,
		Name:    "token-ledger",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS token_ledger (
				id                      INTEGER PRIMARY KEY,
				session_id              TEXT NOT NULL,
				message_id              TEXT NOT NULL DEFAULT '',
				agent_name              TEXT NOT NULL DEFAULT '',
				host                    TEXT NOT NULL DEFAULT '',
				username                TEXT NOT NULL DEFAULT '',
				model                   TEXT NOT NULL DEFAULT '',
				input_tokens            INTEGER NOT NULL DEFAULT 0,
				output_tokens           INTEGER NOT NULL DEFAULT 0,
				cache_read_tokens       INTEGER NOT NULL DEFAULT 0,
				cache_write_tokens      INTEGER NOT NULL DEFAULT 0,
				cache_write_1h          INTEGER NOT NULL DEFAULT 0,
				cache_write_5m          INTEGER NOT NULL DEFAULT 0,
				n_text                  INTEGER NOT NULL DEFAULT 0,
				n_tool_use              INTEGER NOT NULL DEFAULT 0,
				n_thinking              INTEGER NOT NULL DEFAULT 0,
				total_input             INTEGER NOT NULL DEFAULT 0,
				stop_reason             TEXT NOT NULL DEFAULT '',
				service_tier            TEXT NOT NULL DEFAULT '',
				speed                   TEXT NOT NULL DEFAULT '',
				cost_usd                REAL NOT NULL DEFAULT 0,
				timestamp               DATETIME NOT NULL,
				runtime                 TEXT NOT NULL DEFAULT 'claude',
				reasoning_output_tokens INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_token_session ON token_ledger(session_id)`,
			`CREATE INDEX IF NOT EXISTS idx_token_agent ON token_ledger(agent_name)`,
			`CREATE INDEX IF NOT EXISTS idx_token_model ON token_ledger(model)`,
			`CREATE INDEX IF NOT EXISTS idx_token_time ON token_ledger(timestamp)`,
			`CREATE INDEX IF NOT EXISTS idx_token_agent_time ON token_ledger(agent_name, timestamp)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS uq_message ON token_ledger(message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_token_total_input ON token_ledger(session_id, total_input)`,
		},
	},
	{
		// token-ledger-telemetry: columns/indexes front-loaded at v10 above.
		// mysql's data backfill (`message_id = 'legacy-'||id WHERE message_id
		// = ''`) has no rows to touch on a fresh install. The CREATE EVENT
		// scheduled retention job has no SQLite equivalent (no server-side
		// scheduler) — not replicated; retention is an operator/ops concern
		// for this validation backend, not a migration concern.
		Version: 11,
		Name:    "token-ledger-telemetry",
	},
	{
		// attribution-spine: agent_focus_intervals is dead (superseded by
		// wms_intervals @21, never read outside mysql's own upgrade-path
		// tests) — not created. usage_attribution, tags, entity_tags,
		// cost_rollup, session_reconciliation are all alive and front-loaded
		// with their full head shapes (usage_attribution.method @48 chars
		// per v33; usage_attribution.interval_id per v20; tags gains
		// category/cardinality/retired/required/scope/exclusion_group/
		// auto_extract/interview/facet_source across v14-v49, all folded in;
		// cost_rollup's PK uses bucket_hour per v43's hourly-grain change).
		//
		// The seed tag rows below are DATA, not schema — replayed verbatim
		// (translated INSERT IGNORE -> INSERT OR IGNORE) so this backend's
		// tags catalog converges to the same rows a fresh mysql v50 install
		// has. The 'progress' key seeded here is deleted again at v14 below,
		// exactly mirroring mysql's own seed-then-prune history.
		Version: 12,
		Name:    "attribution-spine",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS usage_attribution (
				message_id   TEXT NOT NULL,
				entity_type  TEXT NOT NULL DEFAULT '',
				entity_id    TEXT NOT NULL DEFAULT '',
				weight       REAL NOT NULL,
				method       TEXT NOT NULL,
				computed_at  DATETIME NOT NULL,
				interval_id  INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (message_id, entity_type, entity_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_ua_entity ON usage_attribution(entity_type, entity_id)`,
			`CREATE INDEX IF NOT EXISTS idx_ua_method ON usage_attribution(method)`,
			`CREATE INDEX IF NOT EXISTS idx_ua_interval ON usage_attribution(interval_id)`,
			`CREATE TABLE IF NOT EXISTS tags (
				id              INTEGER PRIMARY KEY,
				tag_key         TEXT NOT NULL,
				tag_value       TEXT NOT NULL,
				is_seed         INTEGER NOT NULL DEFAULT 0,
				description     TEXT NOT NULL DEFAULT '',
				category        TEXT NOT NULL DEFAULT 'context',
				cardinality     TEXT NOT NULL DEFAULT 'multi',
				retired         INTEGER NOT NULL DEFAULT 0,
				required        INTEGER NOT NULL DEFAULT 0,
				scope           TEXT NOT NULL DEFAULT '',
				exclusion_group TEXT NOT NULL DEFAULT '',
				auto_extract    TEXT NOT NULL DEFAULT '',
				interview       TEXT NOT NULL DEFAULT 'propose',
				facet_source    TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS uq_tag ON tags(tag_key, tag_value)`,
			`CREATE INDEX IF NOT EXISTS idx_tag_key ON tags(tag_key)`,
			`CREATE TABLE IF NOT EXISTS entity_tags (
				entity_type TEXT NOT NULL,
				entity_id   TEXT NOT NULL,
				tag_id      INTEGER NOT NULL,
				source      TEXT NOT NULL DEFAULT 'manual',
				applied_at  DATETIME NOT NULL,
				PRIMARY KEY (entity_type, entity_id, tag_id),
				CONSTRAINT fk_et_tag FOREIGN KEY (tag_id) REFERENCES tags(id) ON DELETE CASCADE
			)`,
			`CREATE INDEX IF NOT EXISTS idx_et_tag ON entity_tags(tag_id)`,
			`CREATE INDEX IF NOT EXISTS idx_et_entity ON entity_tags(entity_type, entity_id)`,
			`CREATE TABLE IF NOT EXISTS cost_rollup (
				bucket_day  DATE NOT NULL,
				bucket_hour DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
				entity_type TEXT NOT NULL DEFAULT '',
				entity_id   TEXT NOT NULL DEFAULT '',
				agent_name  TEXT NOT NULL DEFAULT '',
				model       TEXT NOT NULL DEFAULT '',
				tokens      INTEGER NOT NULL DEFAULT 0,
				cost_usd    REAL NOT NULL DEFAULT 0,
				PRIMARY KEY (bucket_hour, entity_type, entity_id, agent_name, model)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_cr_entity ON cost_rollup(entity_type, entity_id)`,
			`CREATE INDEX IF NOT EXISTS idx_cr_agent ON cost_rollup(agent_name)`,
			`CREATE TABLE IF NOT EXISTS session_reconciliation (
				session_id      TEXT NOT NULL PRIMARY KEY,
				otel_cost_usd   REAL NOT NULL DEFAULT 0,
				ledger_cost_usd REAL NOT NULL DEFAULT 0,
				divergence_usd  REAL NOT NULL DEFAULT 0,
				computed_at     DATETIME NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_sr_divergence ON session_reconciliation(divergence_usd)`,
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, description) VALUES
				('phase','design',1,'Design or planning before implementation: shaping the approach, writing the spec/plan. Apply while the task is figuring out WHAT and HOW, before code is written.'),
				('phase','build',1,'Implementation: writing the code/content that satisfies the task. Apply once the task moves from planning into producing the change.'),
				('phase','test',1,'Verification: running tests, exercising behavior, confirming the change does what the task says. Apply during the VALIDATE phase of the execution loop.'),
				('phase','review',1,'Adversarial review of a completed change by someone other than the author. Apply during the REVIEW phase, before commit.'),
				('phase','rework',1,'Work redone after a review or validation sent it back. Apply when an entity re-enters an earlier phase to fix problems found downstream — distinguishes first-pass cost from correction cost.'),
				('work-type','feature',1,'Adds a new capability or user-visible behavior that did not exist before.'),
				('work-type','bug',1,'Fixes incorrect existing behavior. Apply when the task corrects a defect, not when it adds something new.'),
				('work-type','refactor',1,'Restructures existing code without changing its external behavior (cleanup, extraction, renaming).'),
				('work-type','infra',1,'Infrastructure, build, deploy, CI, or tooling work rather than product code.'),
				('work-type','research',1,'Investigation, spike, or exploration to reduce uncertainty; output is knowledge/a decision, not shipped code.'),
				('release','v0.1',1,'Scoped into the first release. Apply to work targeted at the v0.1 milestone.'),
				('release','unreleased',1,'Backlog not yet assigned to a release. The default until work is scheduled into a release.'),
				('priority','p0',1,'Critical / drop-everything: blocks others or breaks production. Apply sparingly.'),
				('priority','p1',1,'High priority: should be done this cycle, ahead of normal work.'),
				('priority','p2',1,'Normal priority: the default for routine work.'),
				('priority','p3',1,'Low priority / nice-to-have: do when nothing higher is pending.'),
				('progress','not-started',1,'Analytic mirror of progress (NOT the WMS state machine): no work has begun.'),
				('progress','in-progress',1,'Analytic mirror of progress (NOT the WMS state machine): actively being worked.'),
				('progress','in-review',1,'Analytic mirror of progress (NOT the WMS state machine): work done, awaiting review.'),
				('progress','done',1,'Analytic mirror of progress (NOT the WMS state machine): work complete. Faceting analytic only — the WMS status is authoritative for workflow.')`,
		},
	},
	{
		// wms-v2-entities: outcomes/outcome_edges/workunits/entity_dependencies
		// are alive and front-loaded with their full (unchanging) head shapes.
		Version: 13,
		Name:    "wms-v2-entities",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS outcomes (
				id             TEXT NOT NULL PRIMARY KEY,
				title          TEXT NOT NULL,
				description    TEXT NOT NULL,
				status         TEXT NOT NULL DEFAULT 'pending',
				prior_status   TEXT NOT NULL DEFAULT '',
				focus          TEXT NOT NULL DEFAULT '',
				origin_host    TEXT NOT NULL DEFAULT '',
				origin_session TEXT NOT NULL DEFAULT '',
				origin_agent   TEXT NOT NULL DEFAULT '',
				created_at     DATETIME NOT NULL,
				updated_at     DATETIME NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_outcomes_status ON outcomes(status)`,
			`CREATE TABLE IF NOT EXISTS outcome_edges (
				parent_id TEXT NOT NULL,
				child_id  TEXT NOT NULL,
				PRIMARY KEY (parent_id, child_id),
				CONSTRAINT fk_oe_parent FOREIGN KEY (parent_id) REFERENCES outcomes(id),
				CONSTRAINT fk_oe_child  FOREIGN KEY (child_id)  REFERENCES outcomes(id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_oe_child ON outcome_edges(child_id)`,
			`CREATE TABLE IF NOT EXISTS workunits (
				id             TEXT NOT NULL PRIMARY KEY,
				outcome_id     TEXT NOT NULL,
				title          TEXT NOT NULL,
				description    TEXT NOT NULL,
				status         TEXT NOT NULL DEFAULT 'pending',
				prior_status   TEXT NOT NULL DEFAULT '',
				agent_id       TEXT NOT NULL DEFAULT '',
				focus          TEXT NOT NULL DEFAULT '',
				origin_host    TEXT NOT NULL DEFAULT '',
				origin_session TEXT NOT NULL DEFAULT '',
				origin_agent   TEXT NOT NULL DEFAULT '',
				created_at     DATETIME NOT NULL,
				updated_at     DATETIME NOT NULL,
				CONSTRAINT fk_wu_outcome FOREIGN KEY (outcome_id) REFERENCES outcomes(id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_wu_outcome ON workunits(outcome_id)`,
			`CREATE INDEX IF NOT EXISTS idx_wu_agent ON workunits(agent_id)`,
			`CREATE INDEX IF NOT EXISTS idx_wu_status ON workunits(status)`,
			`CREATE TABLE IF NOT EXISTS entity_dependencies (
				blocker_type TEXT NOT NULL,
				blocker_id   TEXT NOT NULL,
				blocked_type TEXT NOT NULL,
				blocked_id   TEXT NOT NULL,
				created_at   DATETIME NOT NULL,
				PRIMARY KEY (blocker_type, blocker_id, blocked_type, blocked_id)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_ed_blocked ON entity_dependencies(blocked_type, blocked_id)`,
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, description) VALUES
				('scope', 'strategic', 1, 'High-level initiative or program of work. Apply to outcomes that group other outcomes — the equivalent of a v1 Project.'),
				('scope', 'tactical', 1, 'Concrete measurable result within a strategic outcome. Apply to outcomes that directly parent work units — the equivalent of a v1 Goal.'),
				('resolution', 'achieved', 1, 'Outcome completed successfully. Apply when an outcome reaches done and the objective was met.'),
				('resolution', 'abandoned', 1, 'Outcome deliberately dropped. Apply when an outcome reaches done but the objective was not pursued to completion.'),
				('lifecycle', 'archived', 1, 'Entity is no longer active but retained for historical reference. Apply instead of deleting.')`,
			`DELETE FROM transition_rules WHERE entity_type IN ('project','goal','task','workitem')`,
			`INSERT INTO transition_rules (entity_type, old_status, new_status, required_role) VALUES
				('outcome', 'pending', 'active', '*'),
				('outcome', 'pending', 'blocked', '*'),
				('outcome', 'active', 'review', '*'),
				('outcome', 'active', 'blocked', '*'),
				('outcome', 'active', 'done', '*'),
				('outcome', 'review', 'active', '*'),
				('outcome', 'review', 'done', '*'),
				('outcome', 'review', 'blocked', '*'),
				('outcome', 'blocked', 'pending', '*'),
				('outcome', 'blocked', 'active', '*'),
				('outcome', 'blocked', 'review', '*'),
				('outcome', 'blocked', 'done', '*'),
				('workunit', 'pending', 'active', '*'),
				('workunit', 'pending', 'blocked', '*'),
				('workunit', 'active', 'review', '*'),
				('workunit', 'active', 'blocked', '*'),
				('workunit', 'active', 'done', '*'),
				('workunit', 'review', 'active', '*'),
				('workunit', 'review', 'done', '*'),
				('workunit', 'review', 'blocked', '*'),
				('workunit', 'blocked', 'pending', '*'),
				('workunit', 'blocked', 'active', '*'),
				('workunit', 'blocked', 'review', '*'),
				('workunit', 'blocked', 'done', '*')`,
		},
	},
	{
		// tag-categories: `category` column front-loaded on tags (v12) above.
		// The 'progress' seed (v12) is pruned here, exactly mirroring
		// mysql's own seed-then-prune. The 'project' seed mysql inserts here
		// is deliberately OMITTED — see this file's top comment on
		// mergeProjectToProduct (v27): mysql seeds 'project' at v14 then
		// merges it into 'product' at v27 on every install, fresh or not;
		// skipping the seed here reaches the same converged state (only
		// 'product' survives) without ever creating the transient key.
		Version: 14,
		Name:    "tag-categories",
		SQL: []string{
			`UPDATE tags SET category = 'lifecycle'
				WHERE tag_key IN ('phase', 'work-type', 'resolution', 'lifecycle')`,
			`DELETE FROM entity_tags WHERE tag_id IN (SELECT id FROM tags WHERE tag_key = 'progress')`,
			`DELETE FROM tags WHERE tag_key = 'progress'`,
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, description) VALUES
				('team', '', 1, 'context', 'Which team is working on this. Auto-set from TeamCreate.')`,
		},
	},
	{
		Version: 15,
		Name:    "worktype-lifecycle-seeds",
		SQL: []string{
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, description) VALUES
				('work-type', 'test', 1, 'lifecycle', 'Verification work: writing or running tests, exercising behavior to confirm correctness. Output is confidence, not new product behavior.'),
				('work-type', 'docs', 1, 'lifecycle', 'Documentation work: writing/updating docs, specs, comments. Output is prose, not code.')`,
			`UPDATE tags SET category = 'lifecycle'
				WHERE tag_key = 'work-type' AND tag_value IN ('test', 'docs')`,
		},
	},
	{
		// v1-to-v3-backfill (mysql Func backfillV1ToV3): reads FROM the v1
		// entity tables, which are empty on any fresh install (this backend
		// never creates them at all) — a genuine no-op, not just "no legacy
		// data available".
		Version: 16,
		Name:    "v1-to-v3-backfill",
	},
	{
		// v1-rename: RENAMEs the v1 tables + plane_sync, none of which exist
		// in this backend (dead on arrival — see v1/v7 above).
		Version: 17,
		Name:    "v1-rename",
	},
	{
		// tag-vocab-prune: `cardinality` column front-loaded on tags (v12).
		Version: 18,
		Name:    "tag-vocab-prune",
		SQL: []string{
			`UPDATE tags SET cardinality = 'single' WHERE tag_key IN ('project', 'priority')`,
			`UPDATE tags SET is_seed = 0 WHERE tag_key IN ('scope', 'team', 'release')`,
		},
	},
	{
		// interval-phase-column: ALTERs the dead wms_event_records table.
		Version: 19,
		Name:    "interval-phase-column",
	},
	{
		// interval-cost-columns: ALTERs the dead wms_event_records table;
		// usage_attribution.interval_id + its index are front-loaded at v12.
		Version: 20,
		Name:    "interval-cost-columns",
	},
	{
		// wms-intervals-create: the unified interval table — the SOLE
		// interval store on this backend (there is no predecessor table to
		// unify away from, since wms_event_records/agent_focus_intervals
		// were never created). Front-loaded with the full head shape,
		// including phase_assembled_at (mysql v50) so v50 below is a no-op.
		//
		// uq_open (entity_type, entity_id, kind, ended_at): SQLite, like
		// MySQL/MariaDB, treats NULL as DISTINCT within a UNIQUE index, so
		// this is a closed-interval uniqueness guard, not a single-open
		// enforcer — identical semantics to the mysql column, carried over
		// unchanged.
		Version: 21,
		Name:    "wms-intervals-create",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS wms_intervals (
				id                 INTEGER PRIMARY KEY,
				kind               TEXT NOT NULL,
				entity_type        TEXT NOT NULL,
				entity_id          TEXT NOT NULL,
				state              TEXT NOT NULL DEFAULT '',
				session_id         TEXT NOT NULL DEFAULT '',
				agent_name         TEXT NOT NULL DEFAULT '',
				host               TEXT NOT NULL DEFAULT '',
				started_at         DATETIME NOT NULL,
				ended_at           DATETIME,
				duration_ms        INTEGER,
				phase              TEXT,
				phase_source       TEXT NOT NULL DEFAULT '',
				assembled_at       DATETIME,
				phase_assembled_at DATETIME,
				cost_usd           REAL,
				cost_tokens        INTEGER,
				identity_source    TEXT NOT NULL DEFAULT ''
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS uq_open ON wms_intervals(entity_type, entity_id, kind, ended_at)`,
			`CREATE INDEX IF NOT EXISTS idx_entity_time ON wms_intervals(entity_type, entity_id, started_at)`,
			`CREATE INDEX IF NOT EXISTS idx_started_ended ON wms_intervals(started_at, ended_at)`,
			`CREATE INDEX IF NOT EXISTS idx_ended_started ON wms_intervals(ended_at, started_at)`,
			`CREATE INDEX IF NOT EXISTS idx_duration ON wms_intervals(duration_ms)`,
			`CREATE INDEX IF NOT EXISTS idx_phase ON wms_intervals(entity_type, phase)`,
			`CREATE INDEX IF NOT EXISTS idx_assemble ON wms_intervals(ended_at, assembled_at)`,
			`CREATE INDEX IF NOT EXISTS idx_focus_lookup ON wms_intervals(session_id, agent_name, started_at)`,
			`CREATE INDEX IF NOT EXISTS idx_focus_open ON wms_intervals(session_id, agent_name, ended_at)`,
			`CREATE INDEX IF NOT EXISTS idx_kind_entity ON wms_intervals(kind, entity_type, entity_id, started_at)`,
		},
	},
	// v22 and v24 are intentionally absent: on mysql they are code-only dual-
	// write/read-cutover waves with no schema version of their own (OD-4) —
	// mirrored here by the same gap, not a step this file forgot.
	{
		// wms-intervals-backfill (mysql Func backfillWmsIntervals): copies
		// wms_event_records/agent_focus_intervals into wms_intervals.
		// Neither source table exists in this backend — no-op.
		Version: 23,
		Name:    "wms-intervals-backfill",
	},
	{
		// wms-intervals-archive: RENAMEs wms_event_records/
		// agent_focus_intervals, neither of which exist here.
		Version: 25,
		Name:    "wms-intervals-archive",
	},
	{
		Version: 26,
		Name:    "context-tag-seeds",
		SQL: []string{
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES
				('product', '', 1, 'context', 'single', 'The ongoing product or area of work (e.g. teamster, homelab). Durable — rarely changes.'),
				('feature', '', 1, 'context', 'single', 'The specific feature being built within a product. A short slug (e.g. dashboard-rework, add-tagging). Mutually exclusive with bug.'),
				('bug', '', 1, 'context', 'single', 'The specific bug being fixed within a product. A short slug (e.g. migration-race, pie-chart-broken). Mutually exclusive with feature.')`,
		},
	},
	{
		// project-to-product (mysql Func mergeProjectToProduct): this file
		// never seeds a 'project' tag_key (see v14's comment), so there is
		// nothing for a merge to do — a genuine no-op here, unlike mysql
		// where the Func performs a real (if deterministic and idempotent)
		// merge on every install. The Stmts-only seeds mysql runs alongside
		// the Func (component/product-version stubs, the lifecycle-key
		// cardinality fix) are real data and are replayed below.
		Version: 27,
		Name:    "project-to-product",
		SQL: []string{
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES
				('component', '', 1, 'context', 'single', 'Subsystem within a product (e.g. networking, harness, ui). Secondary grouping below product.'),
				('product-version', '', 1, 'context', 'single', 'Version or milestone being targeted (semver or milestone slug). Create-on-apply.')`,
			`UPDATE tags SET cardinality = 'single'
				WHERE tag_key IN ('phase', 'resolution', 'lifecycle')
				  AND cardinality != 'single'`,
		},
	},
	{
		// tag-value-retired: `retired` column front-loaded on tags (v12).
		Version: 28,
		Name:    "tag-value-retired",
	},
	{
		// cost-views: mysql defines two VIEWs here (cost_facts,
		// entity_tags_resolved) consumed only by the mysql package's own
		// AllocationStore/ReportingStore SQL and by internal/rollup's test
		// suite (which opens stores exclusively via storetest.Open, a
		// MySQL-only harness — see internal/store/storetest/storetest.go).
		// Neither view is part of store.Store's Go-level contract, so
		// nothing in a SQLite backend NEEDS them to exist. Deliberately not
		// created here; see this package's migrations_test.go doc comment
		// for the full rationale and what a future SQLite ReportingStore/
		// AllocationStore implementer should know if they want equivalent
		// views of their own (a new v51+ migration, translating
		// CAST(...AS UNSIGNED) -> CAST(...AS INTEGER); everything else in
		// both view bodies — COALESCE, LEFT JOIN, window functions — is
		// already portable SQL).
		Version: 29,
		Name:    "cost-views",
	},
	{
		// tag-required: `required` column front-loaded on tags (v12).
		Version: 30,
		Name:    "tag-required",
		SQL: []string{
			`UPDATE tags SET required = 1 WHERE tag_key = 'work-type'`,
		},
	},
	{
		// tag-description-widen: tags.description is already TEXT (no
		// length limit) from its v12 front-load.
		Version: 31,
		Name:    "tag-description-widen",
	},
	{
		Version: 32,
		Name:    "worktype-rubric-refine",
		SQL: []string{
			`UPDATE tags SET description = 'Investigation, audit, or synthesis whose output is knowledge (a finding or recommendation), not code or docs. Title starts Investigate/Recon/Audit/Explore/Evaluate/Inspect/Synthesize/Diagnose. Synthesis is research even under a docs/build outcome.' WHERE tag_key = 'work-type' AND tag_value = 'research'`,
			`UPDATE tags SET description = 'Authoring or rewriting documentation as the deliverable: README, architecture doc, spec, guide, comments. Output is the prose itself; title names a doc file or says write/rewrite/document. NOT investigation that feeds a doc (that is research).' WHERE tag_key = 'work-type' AND tag_value = 'docs'`,
			`UPDATE tags SET description = 'Infrastructure, build, deploy, CI, provisioning, host setup, or schema/migration plumbing: tooling/substrate, not user-facing behavior. Title: host setup, install/CI/systemd, DB schema scaffolding, exporter wiring. NOT a product capability users invoke.' WHERE tag_key = 'work-type' AND tag_value = 'infra'`,
			`UPDATE tags SET description = 'Adds a new capability that did not exist before: a new endpoint, panel, column, command, or integration. Title starts Add/Implement/Build/Create/Support and the result is new. NOT fixing broken behavior (bug), NOT restructuring code (refactor).' WHERE tag_key = 'work-type' AND tag_value = 'feature'`,
			`UPDATE tags SET description = 'Fixes incorrect existing behavior, a defect in something that already exists. Title starts Fix/Repair/Correct/Resolve, or restores a broken panel/metric/label. NOT adding something new (feature), NOT tooling/infra changes (infra).' WHERE tag_key = 'work-type' AND tag_value = 'bug'`,
			`UPDATE tags SET description = 'Validation run: exercising a deployed system end-to-end to confirm it behaves correctly. Apply when the primary output is a pass/fail verdict on deployed behavior, not new code.' WHERE tag_key = 'work-type' AND tag_value = 'test'`,
		},
	},
	{
		// widen-attribution-method: usage_attribution.method is already TEXT
		// (no length limit) from its v12 front-load.
		Version: 33,
		Name:    "widen-attribution-method",
	},
	{
		// host-user-capture: token_ledger.username / sessions.username are
		// both front-loaded already (v10 / v3).
		Version: 34,
		Name:    "host-user-capture",
	},
	{
		Version: 35,
		Name:    "recovery-evidence",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS recovery_evidence (
				message_id   TEXT NOT NULL PRIMARY KEY,
				entity_type  TEXT NOT NULL DEFAULT '',
				entity_id    TEXT NOT NULL DEFAULT '',
				setfocus_at  DATETIME NOT NULL,
				recovered_at DATETIME NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_re_entity ON recovery_evidence(entity_type, entity_id)`,
		},
	},
	{
		Version: 36,
		Name:    "seed-user-tag-key",
		SQL: []string{
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES
				('user', '', 1, 'context', 'single', 'OS user that created the WMS entity; auto-applied at creation from the session/process user (TEAMSTER_USER else os user). Faceting key for multi-user fabrics.')`,
		},
	},
	{
		Version: 37,
		Name:    "warmup-recovery",
		SQL: []string{
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES
				('phase', 'admin', 1, 'lifecycle', 'single', 'Orientation/warmup/coordination cost before the session declared a work focus.')`,
			`CREATE TABLE IF NOT EXISTS warmup_evidence (
				message_id       TEXT NOT NULL PRIMARY KEY,
				entity_type      TEXT NOT NULL DEFAULT '',
				entity_id        TEXT NOT NULL DEFAULT '',
				warmup_start     DATETIME NOT NULL,
				first_focus_at   DATETIME NOT NULL,
				recovered_at     DATETIME NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_we_entity ON warmup_evidence(entity_type, entity_id)`,
		},
	},
	{
		// synthesis-evidence: front-loaded with confidence already TEXT (no
		// length limit), so v48's later widen is a no-op.
		Version: 38,
		Name:    "synthesis-evidence",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS synthesis_evidence (
				message_id       TEXT NOT NULL PRIMARY KEY,
				entity_type      TEXT NOT NULL DEFAULT '',
				entity_id        TEXT NOT NULL DEFAULT '',
				session_id       TEXT NOT NULL DEFAULT '',
				confidence       TEXT NOT NULL DEFAULT '',
				evidence_excerpt TEXT NOT NULL,
				mapping_source   TEXT NOT NULL DEFAULT '',
				recovered_at     DATETIME NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_se_entity ON synthesis_evidence(entity_type, entity_id)`,
			`CREATE INDEX IF NOT EXISTS idx_se_session ON synthesis_evidence(session_id)`,
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES
				('source', '', 1, 'context', 'single', 'Provenance marker for WMS entities created by automated processes (e.g. source:synthesized for LLM-synthesized outcomes).')`,
		},
	},
	{
		Version: 39,
		Name:    "gap-recovery",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS gap_evidence (
				message_id           TEXT NOT NULL PRIMARY KEY,
				entity_type          TEXT NOT NULL DEFAULT '',
				entity_id            TEXT NOT NULL DEFAULT '',
				session_id           TEXT NOT NULL DEFAULT '',
				agent_name           TEXT NOT NULL DEFAULT '',
				resolution_method    TEXT NOT NULL DEFAULT '',
				resolved_from_entity TEXT NOT NULL DEFAULT '',
				recovered_at         DATETIME NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_ge_entity ON gap_evidence(entity_type, entity_id)`,
			`CREATE INDEX IF NOT EXISTS idx_ge_session ON gap_evidence(session_id)`,
		},
	},
	{
		Version: 40,
		Name:    "required-phase-product",
		SQL: []string{
			`UPDATE tags SET required = 1 WHERE tag_key = 'phase'`,
			`UPDATE tags SET required = 1 WHERE tag_key = 'product'`,
		},
	},
	{
		// entity-tags-resolved-promote-lifecycle: extends the entity_tags_resolved
		// VIEW this file does not create (see v29's comment) — no-op.
		Version: 41,
		Name:    "entity-tags-resolved-promote-lifecycle",
	},
	{
		// outcome-cost-rollup: front-loaded with the bucket_hour PK v43 adds
		// on mysql, so v43 below is a no-op for this table too.
		Version: 42,
		Name:    "outcome-cost-rollup",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS outcome_cost_rollup (
				bucket_day  DATE NOT NULL,
				bucket_hour DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
				outcome_id  TEXT NOT NULL,
				source_type TEXT NOT NULL,
				source_id   TEXT NOT NULL DEFAULT '',
				model       TEXT NOT NULL DEFAULT '',
				agent_name  TEXT NOT NULL DEFAULT '',
				tokens      INTEGER NOT NULL DEFAULT 0,
				cost_usd    REAL NOT NULL DEFAULT 0,
				PRIMARY KEY (bucket_hour, outcome_id, source_type, source_id, model, agent_name)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_ocr_outcome ON outcome_cost_rollup(outcome_id)`,
			`CREATE INDEX IF NOT EXISTS idx_ocr_day ON outcome_cost_rollup(bucket_day)`,
		},
	},
	{
		// cost-rollup-hourly-grain: both cost_rollup (v12) and
		// outcome_cost_rollup (v42) are already front-loaded with the
		// bucket_hour-inclusive primary key this step adds on mysql.
		Version: 43,
		Name:    "cost-rollup-hourly-grain",
	},
	{
		// tag-conventions: scope/exclusion_group/auto_extract/interview are
		// all front-loaded on tags (v12).
		Version: 44,
		Name:    "tag-conventions",
		SQL: []string{
			`UPDATE tags SET scope = 'outcome' WHERE tag_key IN ('product','priority','product-version','feature','bug')`,
			`UPDATE tags SET scope = 'workunit' WHERE tag_key IN ('component','phase','work-type','resolution')`,
			`UPDATE tags SET exclusion_group = 'work-scope' WHERE tag_key IN ('feature','bug')`,
			`UPDATE tags SET auto_extract = 'git' WHERE tag_key LIKE 'github.%' OR tag_key LIKE 'gitlab.%' OR tag_key LIKE 'git.%' OR tag_key LIKE 'jira.%' OR tag_key LIKE 'linear.%'`,
			`UPDATE tags SET interview = 'auto' WHERE tag_key LIKE 'github.%' OR tag_key LIKE 'gitlab.%' OR tag_key LIKE 'git.%'`,
			`UPDATE tags SET interview = 'skip' WHERE tag_key IN ('phase','work-type','resolution','lifecycle','component','user','source')`,
		},
	},
	{
		Version: 45,
		Name:    "brief-directive-recovery",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS directive_evidence (
				message_id     TEXT NOT NULL PRIMARY KEY,
				entity_type    TEXT NOT NULL DEFAULT '',
				entity_id      TEXT NOT NULL DEFAULT '',
				session_id     TEXT NOT NULL DEFAULT '',
				agent_name     TEXT NOT NULL DEFAULT '',
				directive_type TEXT NOT NULL DEFAULT '',
				directive_id   TEXT NOT NULL DEFAULT '',
				recovered_at   DATETIME NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_de_entity ON directive_evidence(entity_type, entity_id)`,
			`CREATE INDEX IF NOT EXISTS idx_de_session ON directive_evidence(session_id)`,
		},
	},
	{
		Version: 46,
		Name:    "focus-interval-repair",
		SQL: []string{
			`CREATE TABLE IF NOT EXISTS focus_interval_repair (
				interval_id     INTEGER NOT NULL PRIMARY KEY,
				prior_ended_at  DATETIME,
				new_ended_at    DATETIME,
				repaired_at     DATETIME NOT NULL
			)`,
		},
	},
	{
		// lifecycle-category-backfill: a drift repair for rows a runtime bug
		// could have miscategorized on a live mysql hub. Harmless to replay
		// verbatim here — it matches nothing (v14/v15 already set these
		// categories correctly) but costs nothing to keep for parity.
		Version: 47,
		Name:    "lifecycle-category-backfill",
		SQL: []string{
			`UPDATE tags SET category = 'lifecycle'
				WHERE tag_key IN ('phase', 'work-type', 'resolution', 'lifecycle')
				  AND category != 'lifecycle'`,
		},
	},
	{
		// widen-synthesis-confidence: synthesis_evidence.confidence is
		// already TEXT (no length limit) from its v38 front-load.
		Version: 48,
		Name:    "widen-synthesis-confidence",
	},
	{
		// add-facet-source: `facet_source` column front-loaded on tags (v12).
		Version: 49,
		Name:    "add-facet-source",
		SQL: []string{
			`UPDATE tags SET facet_source = 'work-type' WHERE tag_key IN ('feature', 'bug') AND facet_source = ''`,
			`INSERT OR IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description, scope, exclusion_group, interview, facet_source)
			 VALUES
			 ('refactor', '', 1, 'context', 'single', 'Refactor slug — identifies the specific refactoring purpose', 'outcome', 'work-scope', 'propose', 'work-type'),
			 ('infra', '', 1, 'context', 'single', 'Infrastructure slug — identifies the specific infra work', 'outcome', 'work-scope', 'propose', 'work-type'),
			 ('docs', '', 1, 'context', 'single', 'Documentation slug — identifies the specific doc effort', 'outcome', 'work-scope', 'propose', 'work-type'),
			 ('research', '', 1, 'context', 'single', 'Research slug — identifies the specific investigation', 'outcome', 'work-scope', 'propose', 'work-type'),
			 ('test', '', 1, 'context', 'single', 'Test slug — identifies the specific validation target', 'outcome', 'work-scope', 'propose', 'work-type'),
			 ('admin', '', 1, 'context', 'single', 'Admin slug — identifies the specific admin task', 'outcome', 'work-scope', 'propose', 'work-type')`,
			`UPDATE tags SET facet_source = 'work-type', scope = 'outcome', exclusion_group = 'work-scope', interview = 'propose', cardinality = 'single'
			 WHERE tag_key IN ('refactor', 'infra', 'docs', 'research', 'test', 'admin') AND facet_source = ''`,
			`UPDATE tags SET exclusion_group = 'work-scope' WHERE tag_key IN ('feature', 'bug') AND exclusion_group = ''`,
		},
	},
	{
		// phase-assembled-at-decouple: `phase_assembled_at` column
		// front-loaded on wms_intervals (v21). The backfill UPDATE is
		// replayed verbatim for fidelity, though wms_intervals is always
		// empty at migration time (no rows can have a non-null phase yet),
		// so it is a genuine no-op in practice.
		Version: 50,
		Name:    "phase-assembled-at-decouple",
		SQL: []string{
			`UPDATE wms_intervals SET phase_assembled_at = assembled_at WHERE phase IS NOT NULL AND phase != ''`,
		},
	},
	{
		// codex-support: sessions/token_ledger column additions front-loaded
		// into v3/v10 above (see their comments). No SQL to replay here --
		// this entry exists only to keep version numbers aligned with mysql.
		Version: 51,
		Name:    "codex-support",
	},
}

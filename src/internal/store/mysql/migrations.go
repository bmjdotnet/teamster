package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// migrations is the ordered list of DDL statements that bring a database
// schema to current. Each entry has a stable integer version that is
// recorded in the schema_version table after successful application.
//
// The DDL mirrors the SQLite schema in github.com/bmjdotnet/teamster/internal/
// wms/sqlite + the V3 additions for sessions and activity_events. Where
// SQLite uses TEXT for IDs and CHECK constraints for enums, MySQL uses
// VARCHAR(64) and ENUM. Timestamps are DATETIME(6) UTC; the Go code always
// writes time.Now.UTC values (no DDL DEFAULT clauses on timestamps) so both
// backends round-trip identically (SPEC §6.3).
var migrations = []migrationStep{
	{
		Version: 1,
		Name:    "wms-core",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS projects (
				id             VARCHAR(64)  NOT NULL PRIMARY KEY,
				name           VARCHAR(255) NOT NULL,
				team_id        VARCHAR(64)  NOT NULL,
				description    TEXT         NOT NULL,
				status         ENUM('planning','active','blocked','complete','archived') NOT NULL DEFAULT 'planning',
				focus          VARCHAR(255) NOT NULL DEFAULT '',
				origin_host    VARCHAR(128) NOT NULL DEFAULT '',
				origin_session VARCHAR(64)  NOT NULL DEFAULT '',
				origin_agent   VARCHAR(64)  NOT NULL DEFAULT '',
				created_at     DATETIME(6)  NOT NULL,
				updated_at     DATETIME(6)  NOT NULL,
				INDEX idx_projects_team (team_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS goals (
				id             VARCHAR(64)  NOT NULL PRIMARY KEY,
				title          VARCHAR(255) NOT NULL,
				project_id     VARCHAR(64)  NOT NULL,
				description    TEXT         NOT NULL,
				status         ENUM('open','active','blocked','achieved','abandoned') NOT NULL DEFAULT 'open',
				focus          VARCHAR(255) NOT NULL DEFAULT '',
				origin_host    VARCHAR(128) NOT NULL DEFAULT '',
				origin_session VARCHAR(64)  NOT NULL DEFAULT '',
				origin_agent   VARCHAR(64)  NOT NULL DEFAULT '',
				created_at     DATETIME(6)  NOT NULL,
				updated_at     DATETIME(6)  NOT NULL,
				INDEX idx_goals_project (project_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS tasks (
				id             VARCHAR(64)  NOT NULL PRIMARY KEY,
				title          VARCHAR(255) NOT NULL,
				goal_id        VARCHAR(64)  NOT NULL DEFAULT '',
				squad_id       VARCHAR(64)  NOT NULL DEFAULT '',
				description    TEXT         NOT NULL,
				status         ENUM('pending','active','blocked','review','complete') NOT NULL DEFAULT 'pending',
				prior_status   VARCHAR(32)  NOT NULL DEFAULT '',
				focus          VARCHAR(255) NOT NULL DEFAULT '',
				origin_host    VARCHAR(128) NOT NULL DEFAULT '',
				origin_session VARCHAR(64)  NOT NULL DEFAULT '',
				origin_agent   VARCHAR(64)  NOT NULL DEFAULT '',
				created_at     DATETIME(6)  NOT NULL,
				updated_at     DATETIME(6)  NOT NULL,
				INDEX idx_tasks_goal (goal_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS work_items (
				id             VARCHAR(64)  NOT NULL PRIMARY KEY,
				title          VARCHAR(255) NOT NULL,
				task_id        VARCHAR(64)  NOT NULL,
				agent_id       VARCHAR(64)  NOT NULL DEFAULT '',
				description    TEXT         NOT NULL,
				status         ENUM('pending','assigned','active','review','complete','blocked') NOT NULL DEFAULT 'pending',
				prior_status   VARCHAR(32)  NOT NULL DEFAULT '',
				output         TEXT         NOT NULL,
				origin_host    VARCHAR(128) NOT NULL DEFAULT '',
				origin_session VARCHAR(64)  NOT NULL DEFAULT '',
				origin_agent   VARCHAR(64)  NOT NULL DEFAULT '',
				created_at     DATETIME(6)  NOT NULL,
				updated_at     DATETIME(6)  NOT NULL,
				INDEX idx_work_items_task (task_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS work_dependencies (
				blocker_id   VARCHAR(64) NOT NULL,
				blocked_id   VARCHAR(64) NOT NULL,
				blocker_type VARCHAR(32) NOT NULL,
				blocked_type VARCHAR(32) NOT NULL,
				created_at   DATETIME(6) NOT NULL,
				PRIMARY KEY (blocker_id, blocked_id),
				INDEX idx_deps_blocked (blocked_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		Version: 3,
		Name:    "sessions-activity",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS sessions (
				session_id  VARCHAR(64)  NOT NULL,
				agent_name  VARCHAR(64)  NOT NULL DEFAULT '',
				host        VARCHAR(128) NOT NULL DEFAULT '',
				team_name   VARCHAR(64)  NOT NULL DEFAULT '',
				project_id  VARCHAR(64)  NOT NULL DEFAULT '',
				goal_id     VARCHAR(64)  NOT NULL DEFAULT '',
				task_id     VARCHAR(64)  NOT NULL DEFAULT '',
				workitem_id VARCHAR(64)  NOT NULL DEFAULT '',
				focus       VARCHAR(255) NOT NULL DEFAULT '',
				first_seen  DATETIME(6)  NOT NULL,
				last_seen   DATETIME(6)  NOT NULL,
				status      ENUM('active','idle','closed') NOT NULL DEFAULT 'active',
				PRIMARY KEY (session_id, agent_name),
				INDEX idx_sessions_host (host),
				INDEX idx_sessions_team (team_name),
				INDEX idx_sessions_last_seen (last_seen)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS activity_events (
				id          BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
				session_id  VARCHAR(64)  NOT NULL,
				agent_name  VARCHAR(64)  NOT NULL DEFAULT '',
				host        VARCHAR(128) NOT NULL DEFAULT '',
				tag         VARCHAR(16)  NOT NULL,
				display     TEXT         NOT NULL,
				focus       VARCHAR(255) NOT NULL DEFAULT '',
				timestamp   DATETIME(6)  NOT NULL,
				INDEX idx_activity_pair_time (session_id, agent_name, timestamp),
				INDEX idx_activity_time (timestamp),
				CONSTRAINT fk_activity_session FOREIGN KEY (session_id, agent_name)
					REFERENCES sessions(session_id, agent_name) ON DELETE CASCADE
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		Version: 4,
		Name:    "transition-rules",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS transition_rules (
				entity_type   VARCHAR(32) NOT NULL,
				old_status    VARCHAR(32) NOT NULL,
				new_status    VARCHAR(32) NOT NULL,
				required_role VARCHAR(64) NOT NULL DEFAULT '*',
				PRIMARY KEY (entity_type, old_status, new_status, required_role)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		Version: 5,
		Name:    "wms-journal",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS wms_journal (
				id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				entity_type  VARCHAR(32)  NOT NULL,
				entity_id    VARCHAR(128) NOT NULL,
				field        VARCHAR(64)  NOT NULL,
				old_value    TEXT,
				new_value    TEXT,
				agent_id     VARCHAR(128),
				host         VARCHAR(128),
				session_id   VARCHAR(128),
				notes        TEXT,
				created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (id),
				INDEX idx_entity (entity_type, entity_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		Version: 6,
		Name:    "cost-columns",
		Stmts: []string{
			`ALTER TABLE work_items ADD COLUMN cost_details TEXT NOT NULL`,
			`UPDATE work_items SET cost_details = '{}' WHERE cost_details = ''`,
			`ALTER TABLE work_items ADD COLUMN usage_details TEXT NOT NULL`,
			`UPDATE work_items SET usage_details = '{}' WHERE usage_details = ''`,
			`ALTER TABLE tasks ADD COLUMN cost_details TEXT NOT NULL`,
			`UPDATE tasks SET cost_details = '{}' WHERE cost_details = ''`,
			`ALTER TABLE tasks ADD COLUMN usage_details TEXT NOT NULL`,
			`UPDATE tasks SET usage_details = '{}' WHERE usage_details = ''`,
		},
	},
	{
		Version: 7,
		Name:    "plane-sync",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS plane_sync (
				plane_id        VARCHAR(255) NOT NULL,
				plane_type      VARCHAR(64)  NOT NULL,
				wms_entity_type VARCHAR(32)  NOT NULL,
				wms_entity_id   VARCHAR(64)  NOT NULL,
				last_sync       DATETIME(6),
				sync_source     VARCHAR(16)  NOT NULL DEFAULT 'plane',
				PRIMARY KEY (plane_id, plane_type),
				INDEX idx_plane_sync_wms (wms_entity_type, wms_entity_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		Version: 8,
		Name:    "plane-sync-project-id",
		Stmts: []string{
			`ALTER TABLE plane_sync ADD COLUMN plane_project_id VARCHAR(255) NOT NULL DEFAULT ''`,
		},
	},
	{
		Version: 9,
		Name:    "event-records",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS wms_event_records (
				id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				entity_type VARCHAR(32)  NOT NULL,
				entity_id   VARCHAR(128) NOT NULL,
				state       VARCHAR(32)  NOT NULL,
				started_at  DATETIME(6)  NOT NULL,
				ended_at    DATETIME(6)  NULL,
				duration_ms BIGINT       NULL,
				session_id  VARCHAR(64)  NOT NULL DEFAULT '',
				agent_name  VARCHAR(64)  NOT NULL DEFAULT '',
				host        VARCHAR(128) NOT NULL DEFAULT '',
				PRIMARY KEY (id),
				UNIQUE INDEX uq_open (entity_type, entity_id, ended_at),
				INDEX idx_entity_time (entity_type, entity_id, started_at),
				INDEX idx_started_ended (started_at, ended_at),
				INDEX idx_ended_started (ended_at, started_at),
				INDEX idx_duration (duration_ms)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`INSERT INTO wms_event_records (entity_type, entity_id, state, started_at)
			 SELECT 'project', id, status, UTC_TIMESTAMP(6) FROM projects`,
			`INSERT INTO wms_event_records (entity_type, entity_id, state, started_at)
			 SELECT 'goal', id, status, UTC_TIMESTAMP(6) FROM goals`,
			`INSERT INTO wms_event_records (entity_type, entity_id, state, started_at)
			 SELECT 'task', id, status, UTC_TIMESTAMP(6) FROM tasks`,
			`INSERT INTO wms_event_records (entity_type, entity_id, state, started_at)
			 SELECT 'workitem', id, status, UTC_TIMESTAMP(6) FROM work_items`,
		},
	},
	{
		Version: 10,
		Name:    "token-ledger",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS token_ledger (
				id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				session_id         VARCHAR(64)     NOT NULL,
				agent_name         VARCHAR(64)     NOT NULL DEFAULT '',
				host               VARCHAR(128)    NOT NULL DEFAULT '',
				model              VARCHAR(128)    NOT NULL DEFAULT '',
				input_tokens       BIGINT UNSIGNED NOT NULL DEFAULT 0,
				output_tokens      BIGINT UNSIGNED NOT NULL DEFAULT 0,
				cache_read_tokens  BIGINT UNSIGNED NOT NULL DEFAULT 0,
				cache_write_tokens BIGINT UNSIGNED NOT NULL DEFAULT 0,
				cost_usd           DECIMAL(12,6)   NOT NULL DEFAULT 0,
				timestamp          DATETIME(6)     NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_token_session (session_id),
				INDEX idx_token_agent (agent_name),
				INDEX idx_token_model (model),
				INDEX idx_token_time (timestamp),
				INDEX idx_token_agent_time (agent_name, timestamp)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		Version: 11,
		Name:    "token-ledger-telemetry",
		Stmts: []string{
			`ALTER TABLE token_ledger
				ADD COLUMN message_id     VARCHAR(128)    NOT NULL DEFAULT '' AFTER session_id,
				ADD COLUMN cache_write_1h BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER cache_write_tokens,
				ADD COLUMN cache_write_5m BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER cache_write_1h,
				ADD COLUMN n_text         SMALLINT UNSIGNED NOT NULL DEFAULT 0 AFTER cache_write_5m,
				ADD COLUMN n_tool_use     SMALLINT UNSIGNED NOT NULL DEFAULT 0 AFTER n_text,
				ADD COLUMN n_thinking     SMALLINT UNSIGNED NOT NULL DEFAULT 0 AFTER n_tool_use,
				ADD COLUMN total_input    BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER n_thinking,
				ADD COLUMN stop_reason    VARCHAR(32)     NOT NULL DEFAULT '' AFTER total_input,
				ADD COLUMN service_tier   VARCHAR(32)     NOT NULL DEFAULT '' AFTER stop_reason,
				ADD COLUMN speed          VARCHAR(32)     NOT NULL DEFAULT '' AFTER service_tier`,
			`UPDATE token_ledger SET message_id = CONCAT('legacy-', id) WHERE message_id = ''`,
			`ALTER TABLE token_ledger
				ADD UNIQUE INDEX uq_message (message_id),
				ADD INDEX idx_token_total_input (session_id, total_input)`,
			`CREATE EVENT IF NOT EXISTS prune_token_ledger
				ON SCHEDULE EVERY 1 DAY
				STARTS CURRENT_TIMESTAMP
				DO DELETE FROM token_ledger WHERE timestamp < DATE_SUB(UTC_TIMESTAMP(), INTERVAL 6 MONTH)`,
		},
	},
	{
		// attribution-spine: the two-source-join attribution model. token_ledger
		// (Record) stays the immutable per-message fact table; these tables are
		// the Associate + Aggregate layers plus the tag faceting dimension.
		// Per-agent identity now arrives directly on token_ledger rows (the
		// scraper stamps agent_name from subagent files), so there is no separate
		// message_attribution bridge — the ledger row IS the message→agent fact.
		Version: 12,
		Name:    "attribution-spine",
		Stmts: []string{
			// agent_focus_intervals: append-only history of what each agent was
			// focused on over time. Replaces the last-write-wins single task_id
			// column on sessions; lets the allocator ask "what was @spine working
			// on at time T?". One open interval (ended_at NULL) per focus.
			`CREATE TABLE IF NOT EXISTS agent_focus_intervals (
				id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				session_id  VARCHAR(64)  NOT NULL,
				agent_name  VARCHAR(64)  NOT NULL DEFAULT '',
				entity_type VARCHAR(32)  NOT NULL,
				entity_id   VARCHAR(128) NOT NULL,
				started_at  DATETIME(6)  NOT NULL,
				ended_at    DATETIME(6)  NULL,
				PRIMARY KEY (id),
				INDEX idx_afi_lookup (session_id, agent_name, started_at),
				INDEX idx_afi_open (session_id, agent_name, ended_at)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			// usage_attribution: the Associate layer. Maps each costed message
			// (token_ledger.message_id) to one or more entities with weights that
			// sum to exactly 1.0 per message_id (an explicit unallocated row makes
			// up any remainder). This single SUM(weight)=1 invariant is what makes
			// the old cost_details 163% overcount structurally impossible.
			`CREATE TABLE IF NOT EXISTS usage_attribution (
				message_id   VARCHAR(128) NOT NULL,
				entity_type  VARCHAR(32)  NOT NULL DEFAULT '',
				entity_id    VARCHAR(128) NOT NULL DEFAULT '',
				weight       DECIMAL(6,5) NOT NULL,
				method       VARCHAR(32)  NOT NULL,
				computed_at  DATETIME(6)  NOT NULL,
				PRIMARY KEY (message_id, entity_type, entity_id),
				INDEX idx_ua_entity (entity_type, entity_id),
				INDEX idx_ua_method (method)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			// tags: key:value faceting dimension. Progress/rework/phase/work-type
			// are tags here, NOT the rigid WMS state machine. is_seed marks the
			// shipped starter classifiers so a reset can restore them without
			// nuking operator-defined tags.
			`CREATE TABLE IF NOT EXISTS tags (
				id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				tag_key     VARCHAR(64)  NOT NULL,
				tag_value   VARCHAR(128) NOT NULL,
				is_seed     TINYINT(1)   NOT NULL DEFAULT 0,
				description VARCHAR(255) NOT NULL DEFAULT '',
				PRIMARY KEY (id),
				UNIQUE KEY uq_tag (tag_key, tag_value),
				INDEX idx_tag_key (tag_key)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			// entity_tags: M:N between WMS entities and tags. DAG inheritance
			// (a goal tag flowing to its tasks) is resolved at READ time in the
			// rollup/view, never written here — so changing a parent tag does not
			// require re-tagging children.
			`CREATE TABLE IF NOT EXISTS entity_tags (
				entity_type VARCHAR(32)  NOT NULL,
				entity_id   VARCHAR(128) NOT NULL,
				tag_id      BIGINT UNSIGNED NOT NULL,
				source      VARCHAR(16)  NOT NULL DEFAULT 'manual',
				applied_at  DATETIME(6)  NOT NULL,
				PRIMARY KEY (entity_type, entity_id, tag_id),
				INDEX idx_et_tag (tag_id),
				INDEX idx_et_entity (entity_type, entity_id),
				CONSTRAINT fk_et_tag FOREIGN KEY (tag_id) REFERENCES tags(id) ON DELETE CASCADE
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			// cost_rollup: the Aggregate layer. Denormalized per-day/entity/agent/
			// model cost = SUM(token_ledger.cost_usd * usage_attribution.weight),
			// fully recomputable from the source tables. The unallocated bucket
			// appears as entity_type='' / entity_id=''. Tag facets join at query
			// time, NOT baked here.
			`CREATE TABLE IF NOT EXISTS cost_rollup (
				bucket_day  DATE         NOT NULL,
				entity_type VARCHAR(32)  NOT NULL DEFAULT '',
				entity_id   VARCHAR(128) NOT NULL DEFAULT '',
				agent_name  VARCHAR(64)  NOT NULL DEFAULT '',
				model       VARCHAR(128) NOT NULL DEFAULT '',
				tokens      BIGINT UNSIGNED NOT NULL DEFAULT 0,
				cost_usd    DECIMAL(14,6) NOT NULL DEFAULT 0,
				PRIMARY KEY (bucket_day, entity_type, entity_id, agent_name, model),
				INDEX idx_cr_entity (entity_type, entity_id),
				INDEX idx_cr_agent (agent_name)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			// session_reconciliation: the OTel↔ledger join result, one row per
			// session. divergence_usd = otel - ledger; a materially positive value
			// is the data-quality alarm (pricing gap, scraper failure, un-ingested
			// subagents). Written by the rollup job; read by dashboards.
			`CREATE TABLE IF NOT EXISTS session_reconciliation (
				session_id      VARCHAR(64)   NOT NULL,
				otel_cost_usd   DECIMAL(14,6) NOT NULL DEFAULT 0,
				ledger_cost_usd DECIMAL(14,6) NOT NULL DEFAULT 0,
				divergence_usd  DECIMAL(14,6) NOT NULL DEFAULT 0,
				computed_at     DATETIME(6)   NOT NULL,
				PRIMARY KEY (session_id),
				INDEX idx_sr_divergence (divergence_usd)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			// Seed classifier rows (is_seed=1). The starter set; operators add
			// their own keys and values freely.
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, description) VALUES
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
		Version: 13,
		Name:    "wms-v2-entities",
		Stmts: []string{
			// 1. New tables
			`CREATE TABLE IF NOT EXISTS outcomes (
				id             VARCHAR(64)  NOT NULL PRIMARY KEY,
				title          VARCHAR(255) NOT NULL,
				description    TEXT         NOT NULL,
				status         VARCHAR(32)  NOT NULL DEFAULT 'pending',
				prior_status   VARCHAR(32)  NOT NULL DEFAULT '',
				focus          VARCHAR(255) NOT NULL DEFAULT '',
				origin_host    VARCHAR(128) NOT NULL DEFAULT '',
				origin_session VARCHAR(64)  NOT NULL DEFAULT '',
				origin_agent   VARCHAR(64)  NOT NULL DEFAULT '',
				created_at     DATETIME(6)  NOT NULL,
				updated_at     DATETIME(6)  NOT NULL,
				INDEX idx_outcomes_status (status)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

			`CREATE TABLE IF NOT EXISTS outcome_edges (
				parent_id VARCHAR(64) NOT NULL,
				child_id  VARCHAR(64) NOT NULL,
				PRIMARY KEY (parent_id, child_id),
				INDEX idx_oe_child (child_id),
				CONSTRAINT fk_oe_parent FOREIGN KEY (parent_id) REFERENCES outcomes(id),
				CONSTRAINT fk_oe_child  FOREIGN KEY (child_id)  REFERENCES outcomes(id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

			`CREATE TABLE IF NOT EXISTS workunits (
				id             VARCHAR(64)  NOT NULL PRIMARY KEY,
				outcome_id     VARCHAR(64)  NOT NULL,
				title          VARCHAR(255) NOT NULL,
				description    TEXT         NOT NULL,
				status         VARCHAR(32)  NOT NULL DEFAULT 'pending',
				prior_status   VARCHAR(32)  NOT NULL DEFAULT '',
				agent_id       VARCHAR(64)  NOT NULL DEFAULT '',
				focus          VARCHAR(255) NOT NULL DEFAULT '',
				origin_host    VARCHAR(128) NOT NULL DEFAULT '',
				origin_session VARCHAR(64)  NOT NULL DEFAULT '',
				origin_agent   VARCHAR(64)  NOT NULL DEFAULT '',
				created_at     DATETIME(6)  NOT NULL,
				updated_at     DATETIME(6)  NOT NULL,
				INDEX idx_wu_outcome (outcome_id),
				INDEX idx_wu_agent (agent_id),
				INDEX idx_wu_status (status),
				CONSTRAINT fk_wu_outcome FOREIGN KEY (outcome_id) REFERENCES outcomes(id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

			`CREATE TABLE IF NOT EXISTS entity_dependencies (
				blocker_type VARCHAR(32) NOT NULL,
				blocker_id   VARCHAR(64) NOT NULL,
				blocked_type VARCHAR(32) NOT NULL,
				blocked_id   VARCHAR(64) NOT NULL,
				created_at   DATETIME(6) NOT NULL,
				PRIMARY KEY (blocker_type, blocker_id, blocked_type, blocked_id),
				INDEX idx_ed_blocked (blocked_type, blocked_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

			// 2. Seed tags for v2 classification
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, description) VALUES
				('scope', 'strategic', 1, 'High-level initiative or program of work. Apply to outcomes that group other outcomes — the equivalent of a v1 Project.'),
				('scope', 'tactical', 1, 'Concrete measurable result within a strategic outcome. Apply to outcomes that directly parent work units — the equivalent of a v1 Goal.'),
				('resolution', 'achieved', 1, 'Outcome completed successfully. Apply when an outcome reaches done and the objective was met.'),
				('resolution', 'abandoned', 1, 'Outcome deliberately dropped. Apply when an outcome reaches done but the objective was not pursued to completion.'),
				('lifecycle', 'archived', 1, 'Entity is no longer active but retained for historical reference. Apply instead of deleting.')`,

			// 3. Transition rules: replace v1 with v2
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
		// Func: nil for now — backfill is Phase 4
	},
	{
		// tag-categories: split the flat tag vocabulary into two behavior
		// classes. 'context' (durable metadata, inherited down the DAG at read
		// time) is the DEFAULT — every operator-defined key is a context tag.
		// 'lifecycle' (execution tracking, per-entity, engine-managed) is a
		// closed seeded set. VARCHAR not ENUM so a third class needs no DDL.
		Version: 14,
		Name:    "tag-categories",
		Stmts: []string{
			`ALTER TABLE tags ADD COLUMN category VARCHAR(32) NOT NULL DEFAULT 'context'`,
			`UPDATE tags SET category = 'lifecycle'
				WHERE tag_key IN ('phase', 'work-type', 'resolution', 'lifecycle')`,
			// Drop the stale 'progress' analytic mirror. Delete bindings first,
			// then the vocabulary; fk_et_tag is ON DELETE CASCADE so the second
			// delete would clear bindings too, but being explicit is order-safe.
			`DELETE FROM entity_tags WHERE tag_id IN (SELECT id FROM tags WHERE tag_key = 'progress')`,
			`DELETE FROM tags WHERE tag_key = 'progress'`,
			// Add the 'project' and 'team' context seeds. Empty tag_value is the
			// vocabulary stub; create-on-apply fills real values on first use.
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, description) VALUES
				('project', '', 1, 'context', 'Which project this work belongs to. Create values on first use.'),
				('team', '', 1, 'context', 'Which team is working on this. Auto-set from TeamCreate.')`,
		},
	},
	{
		// worktype-lifecycle-seeds: the classifier applies work-type=test and
		// work-type=docs (classifier.go rules 3 and 4), but neither was in the
		// v12 work-type seed set, so create-on-apply inserts them with the v14
		// DEFAULT category 'context' — a single tag_key spanning two categories,
		// while every other work-type value is 'lifecycle'. Seed both as
		// 'lifecycle' for fresh installs AND correct any row a prior classifier
		// run already created as 'context' (B2 is live; deployed DBs may hold them).
		Version: 15,
		Name:    "worktype-lifecycle-seeds",
		Stmts: []string{
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, description) VALUES
				('work-type', 'test', 1, 'lifecycle', 'Verification work: writing or running tests, exercising behavior to confirm correctness. Output is confidence, not new product behavior.'),
				('work-type', 'docs', 1, 'lifecycle', 'Documentation work: writing/updating docs, specs, comments. Output is prose, not code.')`,
			`UPDATE tags SET category = 'lifecycle'
				WHERE tag_key = 'work-type' AND tag_value IN ('test', 'docs')`,
		},
	},
	{
		// v1→v3 backfill: preserve existing v1 entities as v3 Outcomes/WorkUnits.
		// Project→Outcome(scope:strategic, project:<name>), Goal→Outcome(scope:
		// tactical, parent=project's outcome via outcome_edges), Task→WorkUnit
		// (outcome_id=goal's outcome). WorkItems are absorbed into their task's
		// WorkUnit (their tags/events/journal re-pointed to wu-<task.id>). Carries
		// status (mapped) and re-points entity_tags / wms_event_records /
		// wms_journal / work_dependencies(→entity_dependencies). Pure Go transform
		// in one transaction; idempotent (INSERT IGNORE + a no-op guard). The v1
		// tables are NOT touched here — the rename/cleanup is a later migration.
		Version: 16,
		Name:    "v1-to-v3-backfill",
		Func:    backfillV1ToV3,
	},
	{
		// v1-rename: archive the v1 base tables under archived_v1_* prefix.
		// RENAME TABLE is reversible at the DB level:
		//   RENAME TABLE archived_v1_projects TO projects (etc.)
		// restores the original layout. Operator sign-off: Plane integration
		// dropped 2026-06-02; v1 CRUD removed atomically in this same batch —
		// no live code reader points at these table names after this migration.
		// plane_sync is archived here too: the Plane webhook path is removed.
		Version: 17,
		Name:    "v1-rename",
		Stmts: []string{
			`RENAME TABLE projects          TO archived_v1_projects`,
			`RENAME TABLE goals             TO archived_v1_goals`,
			`RENAME TABLE tasks             TO archived_v1_tasks`,
			`RENAME TABLE work_items        TO archived_v1_work_items`,
			`RENAME TABLE work_dependencies TO archived_v1_work_dependencies`,
			`RENAME TABLE plane_sync        TO archived_v1_plane_sync`,
		},
	},
	{
		// tag-vocab-prune: shrink the work-item DECLARED vocabulary and add
		// per-key cardinality. Two changes, both NON-DESTRUCTIVE:
		//
		//   1. Add the `cardinality` column ('single' | 'multi'). It records
		//      whether a key holds one value per entity (project, priority) or
		//      many (work-type: feature + experiment). Default 'multi' so storage
		//      keeps its existing multi-value behavior; the single-value rule is a
		//      write-guard in TagEntity, never a schema constraint.
		//   2. DEMOTE the pruned keys (scope, team, release) by clearing is_seed,
		//      NOT deleting them. Per the v17 archived_v1_* / RENAME-not-drop
		//      precedent, this is reversible: re-listing a key in the yaml `tags:`
		//      vocabulary (or DefineTag) re-promotes it. Existing entity_tags
		//      bindings are left completely untouched — demoting a seed must never
		//      orphan an operator's tagged work. The backfill's scope:strategic/
		//      tactical writes (v1->v3 provenance) are intentionally NOT touched.
		Version: 18,
		Name:    "tag-vocab-prune",
		Stmts: []string{
			`ALTER TABLE tags ADD COLUMN cardinality VARCHAR(8) NOT NULL DEFAULT 'multi'`,
			`UPDATE tags SET cardinality = 'single' WHERE tag_key IN ('project', 'priority')`,
			`UPDATE tags SET is_seed = 0 WHERE tag_key IN ('scope', 'team', 'release')`,
		},
	},
	{
		// interval-phase-column: put a single-valued phase ON the interval row so
		// cost-by-phase (v20) has a conserved phase to group on. Additive and
		// reversible (drop the columns). NO data backfill — no historical phase is
		// invented; phase is NULL until declared (or, in B4, classifier-derived).
		//   phase        NULL = "not yet classified".
		//   phase_source {'declared','classifier',''} — declared wins (the
		//                UpdateEventRecordPhase WHERE guard enforces precedence).
		//   assembled_at the async watermark (B4 classifier uses it; present now so
		//                the column set is stable and v20 doesn't re-ALTER).
		Version: 19,
		Name:    "interval-phase-column",
		Stmts: []string{
			`ALTER TABLE wms_event_records
				ADD COLUMN phase        VARCHAR(32)  NULL,
				ADD COLUMN phase_source VARCHAR(16)  NOT NULL DEFAULT '',
				ADD COLUMN assembled_at DATETIME(6)  NULL`,
			`ALTER TABLE wms_event_records ADD INDEX idx_phase (entity_type, phase)`,
		},
	},
	{
		// interval-cost-columns: columns to hold conserved per-interval cost and to
		// attribute each usage row to its covering interval. Columns ONLY — the
		// assembly (intervalAt resolution + the SUM-back UPDATE) lives in the async
		// rollup (cmd/rollup), which shares the usage_attribution pass it already
		// builds. Additive and reversible (drop the columns).
		//   wms_event_records.cost_usd/cost_tokens  NULL = not yet assembled.
		//   usage_attribution.interval_id  a NON-key column (DEFAULT 0 = "no covering
		//     interval / not yet interval-attributed"), so the PK
		//     (message_id, entity_type, entity_id) and the SUM(weight)=1 invariant
		//     are untouched.
		Version: 20,
		Name:    "interval-cost-columns",
		Stmts: []string{
			`ALTER TABLE wms_event_records
				ADD COLUMN cost_usd    DECIMAL(14,6)   NULL,
				ADD COLUMN cost_tokens BIGINT UNSIGNED NULL`,
			`ALTER TABLE usage_attribution
				ADD COLUMN interval_id BIGINT UNSIGNED NOT NULL DEFAULT 0`,
			`ALTER TABLE usage_attribution ADD INDEX idx_ua_interval (interval_id)`,
		},
	},
	{
		// wms-intervals-create (B3 Wave 1): the unified interval table that will
		// subsume wms_event_records (kind='state') and agent_focus_intervals
		// (kind='focus') — B3-design §1.2. ADDITIVE ONLY: a new table beside the
		// old two. No dual-write, no backfill, no reader change, no rename — those
		// are Waves 2/3/4 (v23 backfill Func, v25 rename). Nothing reads or writes
		// this table yet, so it is reversible by DROP.
		//
		// The intrinsic columns mirror wms_event_records (v9 + v19 phase + v20 cost)
		// plus the focus-side columns (session_id/agent_name/started_at/ended_at)
		// and the new identity_source provenance column (§2). `kind` is the LOCKED
		// provenance discriminator; adding it to uq_open lets one entity hold a
		// concurrent open 'state' and open 'focus' row without colliding.
		//
		// uq_open is a CLOSED-interval uniqueness guard, NOT a single-open enforcer:
		// NULL ended_at is DISTINCT in a MySQL/MariaDB UNIQUE index, so it does not
		// cap open rows (same property wms_event_records.uq_open has today). The
		// single-open invariant stays procedural in the FOR-UPDATE writers (B3 §1.2
		// / red-team MF-4) — those carry over unchanged in later waves.
		//
		// Cross-engine: every type/clause is portable across MySQL 8.0 and
		// MariaDB 11.8 — DATETIME(6), DECIMAL(14,6),
		// BIGINT UNSIGNED, VARCHAR, ENGINE=InnoDB, named indexes; no MariaDB-only
		// syntax. CREATE TABLE IF NOT EXISTS runs verbatim through execMigrationStmt
		// (not an ADD-COLUMN ALTER), so a re-run is a no-op.
		Version: 21,
		Name:    "wms-intervals-create",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS wms_intervals (
				id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
				kind          VARCHAR(16)  NOT NULL,
				entity_type   VARCHAR(32)  NOT NULL,
				entity_id     VARCHAR(128) NOT NULL,
				state         VARCHAR(32)  NOT NULL DEFAULT '',
				session_id    VARCHAR(64)  NOT NULL DEFAULT '',
				agent_name    VARCHAR(64)  NOT NULL DEFAULT '',
				host          VARCHAR(128) NOT NULL DEFAULT '',
				started_at    DATETIME(6)  NOT NULL,
				ended_at      DATETIME(6)  NULL,
				duration_ms   BIGINT       NULL,
				phase         VARCHAR(32)  NULL,
				phase_source  VARCHAR(16)  NOT NULL DEFAULT '',
				assembled_at  DATETIME(6)  NULL,
				cost_usd      DECIMAL(14,6) NULL,
				cost_tokens   BIGINT UNSIGNED NULL,
				identity_source VARCHAR(16) NOT NULL DEFAULT '',
				PRIMARY KEY (id),
				UNIQUE INDEX uq_open (entity_type, entity_id, kind, ended_at),
				INDEX idx_entity_time   (entity_type, entity_id, started_at),
				INDEX idx_started_ended (started_at, ended_at),
				INDEX idx_ended_started (ended_at, started_at),
				INDEX idx_duration      (duration_ms),
				INDEX idx_phase         (entity_type, phase),
				INDEX idx_assemble      (ended_at, assembled_at),
				INDEX idx_focus_lookup  (session_id, agent_name, started_at),
				INDEX idx_focus_open    (session_id, agent_name, ended_at),
				INDEX idx_kind_entity   (kind, entity_type, entity_id, started_at)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		// wms-intervals-backfill (B3 Wave 2): copy the existing wms_event_records
		// and agent_focus_intervals rows into wms_intervals, then carry identity
		// onto the state rows from their contemporaneous focus intervals. Go Func
		// (peer of backfillV1ToV3) — runs under the migrate advisory lock.
		//
		// OD-2 R2 (operator-ruled FINAL, atomic, no post-deploy step): BOTH kinds
		// take FRESH auto-increment ids — NOT id-preservation. After the copy, one
		// in-migration UPDATE remaps usage_attribution.interval_id (v20) from the old
		// wms_event_records.id to the new wms_intervals.id, while wms_event_records
		// still exists (before the v25 rename). R1 (preserve the state id) was
		// REJECTED: the W2 dual-write claims wms_intervals ids 1..N at runtime BEFORE
		// this backfill runs, so an explicit-id INSERT would 1062-collide on the live
		// upgrade path — cleanroom-invisible. See §3.1 of the B3 design.
		//
		// Idempotent: NOT EXISTS guards on both INSERTs and the carry's
		// session_id=''/identity_source<>'direct' guard make a re-run insert/update
		// nothing. The W2 dual-write keeps wms_intervals current for NEW writes;
		// this Func is the one-shot history copy.
		Version: 23,
		Name:    "wms-intervals-backfill",
		Func:    backfillWmsIntervals,
	},
	{
		// wms-intervals-archive (B3 Wave 4): the unify is complete — every reader
		// and writer now targets wms_intervals (W3 cutover, dual-write dropped), and
		// the post-cutover phase/cost assembly is verified. Archive the two now-dead
		// source tables under archived_v2_* by RENAME — NEVER DROP — exactly as v17
		// archived the v1 base tables (the precedent above). RENAME is reversible at
		// the DB level (RENAME TABLE archived_v2_event_records TO wms_event_records,
		// etc.) so a regression can fully restore the pre-B3 layout.
		//
		// Safe to run because usage_attribution.interval_id was already remapped in
		// v23 (OD-2 R2) from wms_event_records.id to wms_intervals.id, so NO live
		// cost pointer survives into the archived table — nothing reads these names
		// after W3.
		//
		// Version 25 (not 24): v22/v24 are the dual-write and read-cutover CODE waves
		// — they ship in the binary, not as schema versions, so the migrations slice
		// gaps over them (OD-4). RENAME TABLE is portable across MySQL 8.0 and
		// MariaDB 11.8; it is not an ADD-COLUMN ALTER, so it runs verbatim
		// through execMigrationStmt. The runMigrations schema_version gate makes it
		// run exactly once — a re-run skips the applied version (RENAME is not itself
		// idempotent; the version gate is what makes the step safe to re-encounter,
		// same as v17).
		Version: 25,
		Name:    "wms-intervals-archive",
		Stmts: []string{
			`RENAME TABLE wms_event_records    TO archived_v2_event_records`,
			`RENAME TABLE agent_focus_intervals TO archived_v2_focus_intervals`,
		},
	},
	{
		Version: 26,
		Name:    "context-tag-seeds",
		Stmts: []string{
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES
            ('product', '', 1, 'context', 'single', 'The ongoing product or area of work (e.g. teamster, homelab). Durable — rarely changes.'),
            ('feature', '', 1, 'context', 'single', 'The specific feature being built within a product. A short slug (e.g. dashboard-rework, add-tagging). Mutually exclusive with bug.'),
            ('bug', '', 1, 'context', 'single', 'The specific bug being fixed within a product. A short slug (e.g. migration-race, pie-chart-broken). Mutually exclusive with feature.')`,
		},
	},
	{
		// project-to-product: rename the legacy `project` tag_key to `product`,
		// preserving all entity_tags bindings (they reference tags.id, not key names).
		//
		// The `uq_tag (tag_key, tag_value)` unique index prevents a plain UPDATE when
		// a `product` stub already exists for the same tag_value. The Func handles
		// this by looping project rows: if no product row with the same value exists,
		// UPDATE the tag_key in place; if a product stub exists (the empty-value stub
		// from v26), entity_tags already points at the product row so we just DELETE
		// the project duplicate.
		//
		// Stmts run first: add component/product-version stubs and fix lifecycle
		// cardinality. Func runs after: perform the project→product merge.
		Version: 27,
		Name:    "project-to-product",
		Stmts: []string{
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES
            ('component', '', 1, 'context', 'single', 'Subsystem within a product (e.g. networking, harness, ui). Secondary grouping below product.'),
            ('product-version', '', 1, 'context', 'single', 'Version or milestone being targeted (semver or milestone slug). Create-on-apply.')`,
			`UPDATE tags SET cardinality = 'single'
            WHERE tag_key IN ('phase', 'resolution', 'lifecycle')
              AND cardinality != 'single'`,
		},
		Func: mergeProjectToProduct,
	},
	{
		Version: 28,
		Name:    "tag-value-retired",
		Stmts: []string{
			`ALTER TABLE tags ADD COLUMN retired TINYINT(1) NOT NULL DEFAULT 0`,
		},
	},
	{
		Version: 29,
		Name:    "cost-views",
		Stmts: []string{
			// cost_facts: message-grained weighted cost. Conservation-exact vs
			// token_ledger: the LEFT JOIN means unattributed messages appear as
			// unallocated rows with entity '' and weight 1, so SUM(cost_usd) over
			// cost_facts equals SUM(cost_usd) over token_ledger. CREATE OR REPLACE
			// VIEW is idempotent — safe to re-run over a production DB that already
			// has this view (as created and validated live on the hub).
			`CREATE OR REPLACE VIEW cost_facts AS
SELECT
  tl.message_id, tl.session_id, tl.agent_name, tl.host, tl.model, tl.timestamp,
  COALESCE(ua.entity_type, '') AS entity_type,
  COALESCE(ua.entity_id, '')   AS entity_id,
  COALESCE(ua.weight, 1)       AS weight,
  tl.cost_usd * COALESCE(ua.weight, 1) AS cost_usd,
  CAST((tl.input_tokens + tl.output_tokens + tl.cache_read_tokens + tl.cache_write_tokens) * COALESCE(ua.weight, 1) AS UNSIGNED) AS tokens
FROM token_ledger tl
LEFT JOIN usage_attribution ua ON ua.message_id = tl.message_id`,
			// entity_tags_resolved: direct tags union workunit inheriting parent
			// outcome's tags. Per-key override: if a workunit has its own value for
			// a tag_key, the inherited outcome row for that key is suppressed — only
			// the workunit's own binding is visible. This preserves specificity while
			// ensuring a workunit without an explicit value falls back to the outcome.
			`CREATE OR REPLACE VIEW entity_tags_resolved AS
SELECT et.entity_type, et.entity_id, et.tag_id
FROM entity_tags et
UNION ALL
SELECT 'workunit', w.id, et.tag_id
FROM workunits w
JOIN entity_tags et ON et.entity_type = 'outcome' AND et.entity_id = w.outcome_id
JOIN tags ti ON ti.id = et.tag_id
WHERE NOT EXISTS (
  SELECT 1 FROM entity_tags e2 JOIN tags t2 ON t2.id = e2.tag_id
  WHERE e2.entity_type = 'workunit' AND e2.entity_id = w.id AND t2.tag_key = ti.tag_key
)`,
		},
	},
	{
		// required flag: a per-key property (like cardinality). When ANY row for a
		// tag_key has required=1, that key must be present on every workunit
		// (outcomes are exempt — they carry inherited context tags, not lifecycle
		// tags). work-type ships required by default — the primary work
		// classification. The ADD COLUMN is made idempotent by execMigrationStmt's
		// information_schema guard (cross-engine: MySQL 8.0 and MariaDB 11.8).
		Version: 30,
		Name:    "tag-required",
		Stmts: []string{
			`ALTER TABLE tags ADD COLUMN required TINYINT(1) NOT NULL DEFAULT 0 AFTER retired`,
			`UPDATE tags SET required = 1 WHERE tag_key = 'work-type'`,
		},
	},
	{
		// Widen tags.description 255→1024. The tag steward's purpose is rich,
		// rule-bearing classification descriptions ("the description IS the rule"),
		// but varchar(255) silently rejected the first live wms_describeTag calls
		// with Error 1406 (em-dashes are multibyte, eating the budget invisibly).
		// MODIFY COLUMN is supported on MySQL 8.0 and MariaDB 11.8 and is idempotent
		// (MODIFY to the same type is a no-op re-run on both engines). NOTE: this is
		// NOT an ADD COLUMN, so parseAddColumnAlter's information_schema guard does
		// not apply — re-run safety rests on MODIFY-to-same-type being a no-op.
		// Must precede v32's refined descriptions so the longer text fits.
		Version: 31,
		Name:    "tag-description-widen",
		Stmts: []string{
			`ALTER TABLE tags MODIFY description VARCHAR(1024) NOT NULL DEFAULT ''`,
		},
	},
	{
		// Capture the work-type rubric the live tag steward sharpened on the hub
		// into the shipped vocabulary, so fresh installs
		// get the refined, rule-bearing descriptions instead of the generic seeds.
		// Six of seven values are refined here (research/docs/infra/feature/bug from
		// this backfill, plus test from an earlier live refinement that had diverged
		// from the seed); refactor is left unchanged (already matches its seed).
		// Each UPDATE is idempotent (re-running writes the same text). Ordered after
		// v31 so the >255-char text fits. The original seeds (v12/v15) run first on a
		// fresh install, then this overwrites the six with the refined rubric.
		Version: 32,
		Name:    "worktype-rubric-refine",
		Stmts: []string{
			`UPDATE tags SET description = 'Investigation, audit, or synthesis whose output is knowledge (a finding or recommendation), not code or docs. Title starts Investigate/Recon/Audit/Explore/Evaluate/Inspect/Synthesize/Diagnose. Synthesis is research even under a docs/build outcome.' WHERE tag_key = 'work-type' AND tag_value = 'research'`,
			`UPDATE tags SET description = 'Authoring or rewriting documentation as the deliverable: README, architecture doc, spec, guide, comments. Output is the prose itself; title names a doc file or says write/rewrite/document. NOT investigation that feeds a doc (that is research).' WHERE tag_key = 'work-type' AND tag_value = 'docs'`,
			`UPDATE tags SET description = 'Infrastructure, build, deploy, CI, provisioning, host setup, or schema/migration plumbing: tooling/substrate, not user-facing behavior. Title: host setup, install/CI/systemd, DB schema scaffolding, exporter wiring. NOT a product capability users invoke.' WHERE tag_key = 'work-type' AND tag_value = 'infra'`,
			`UPDATE tags SET description = 'Adds a new capability that did not exist before: a new endpoint, panel, column, command, or integration. Title starts Add/Implement/Build/Create/Support and the result is new. NOT fixing broken behavior (bug), NOT restructuring code (refactor).' WHERE tag_key = 'work-type' AND tag_value = 'feature'`,
			`UPDATE tags SET description = 'Fixes incorrect existing behavior, a defect in something that already exists. Title starts Fix/Repair/Correct/Resolve, or restores a broken panel/metric/label. NOT adding something new (feature), NOT tooling/infra changes (infra).' WHERE tag_key = 'work-type' AND tag_value = 'bug'`,
			`UPDATE tags SET description = 'Validation run: exercising a deployed system end-to-end to confirm it behaves correctly. Apply when the primary output is a pass/fail verdict on deployed behavior, not new code.' WHERE tag_key = 'work-type' AND tag_value = 'test'`,
		},
	},
	{
		// Widen usage_attribution.method 32→48. The focus-attribution recovery
		// work adds two new method labels that exceed VARCHAR(32):
		// 'temporal_join_lead_session_fallback' (35 chars, the P1a lead-session
		// fallback) overflows with Error 1406, and 'transcript_focus_recovery'
		// (25) fits today but leaves no headroom. 48 comfortably holds both plus
		// future labels. MODIFY COLUMN to a wider VARCHAR is supported and
		// idempotent on MySQL 8.0 and MariaDB 11.8 (MODIFY to the same width is a
		// no-op re-run); it is NOT an ADD COLUMN, so parseAddColumnAlter's
		// information_schema guard does not apply — re-run safety rests on
		// MODIFY-to-same-type being a no-op, exactly as v31 did for tags.description.
		Version: 33,
		Name:    "widen-attribution-method",
		Stmts: []string{
			`ALTER TABLE usage_attribution MODIFY method VARCHAR(48) NOT NULL`,
		},
	},
	{
		// Add a `username` column to token_ledger and sessions so the
		// focus-attribution recovery pass can route a session to the correct
		// host-local ~/.claude transcript home: recovery is host+user-local, and
		// `host` alone does not disambiguate two users on one host. Both tables
		// already carry `host VARCHAR(128)` (the hostname, from TEAMSTER_HOST /
		// os.Hostname); only the user is missing — hence username only, no second
		// hostname column. The scraper/server stamping populates it; '' default
		// keeps every existing row and any unstamped writer valid.
		//
		// Each ADD COLUMN is its own single-clause ALTER so execMigrationStmt's
		// parseAddColumnAlter recognises it and skips the clause when the column
		// already exists (information_schema guard, identical on MySQL 8.0 and
		// MariaDB 11.8). That makes a re-run a no-op and prevents the 1060
		// duplicate-column race when several migrate() callers run on a fresh DB,
		// under the advisory lock the migrate loop already holds. No index:
		// recovery filters by host first, then locates the user home.
		Version: 34,
		Name:    "host-user-capture",
		Stmts: []string{
			`ALTER TABLE token_ledger ADD COLUMN username VARCHAR(64) NOT NULL DEFAULT '' AFTER host`,
			`ALTER TABLE sessions ADD COLUMN username VARCHAR(64) NOT NULL DEFAULT '' AFTER host`,
		},
	},
	{
		// recovery_evidence: the audit side table for the focus-attribution
		// recovery pass (--recover-focus). One row per recovered message records
		// the matched setFocus that justified the re-attribution — the entity and
		// the setFocus instant read from the transcript — so the operator can ask
		// "why is this $0.16 on outcome X" and get "wms_setFocus(X) at <ts>, before
		// this message" (spec §5.4). usage_attribution alone holds the resolved
		// entity but not the matched setFocus timestamp (and interval_id can't carry
		// a transcript instant), hence a side table. Keyed by message_id (1:1 with
		// the recovered usage_attribution row). Unrecover deletes these alongside
		// the recovery rows. CREATE TABLE IF NOT EXISTS is idempotent, so a re-run
		// under the migrate advisory lock is a clean no-op.
		Version: 35,
		Name:    "recovery-evidence",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS recovery_evidence (
				message_id   VARCHAR(128) NOT NULL,
				entity_type  VARCHAR(32)  NOT NULL DEFAULT '',
				entity_id    VARCHAR(128) NOT NULL DEFAULT '',
				setfocus_at  DATETIME(6)  NOT NULL,
				recovered_at DATETIME(6)  NOT NULL,
				PRIMARY KEY (message_id),
				INDEX idx_re_entity (entity_type, entity_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		// Seed the `user` tag key as context/single so the wms-mcp auto-applies
		// user:<creator> on every entity at creation (wu-user-identity Part B).
		// The key must exist with category='context', cardinality='single' on a
		// FRESH DB — without this seed it would be create-on-apply at the multi
		// default, the wrong cardinality. INSERT IGNORE on the uq_tag(tag_key,
		// tag_value) unique key makes the empty-value stub idempotent, exactly as
		// v26/v28 seed product/feature/bug/component. Matches the operator's live
		// runtime wms_defineTag so fresh installs and the :13306 test DB converge.
		Version: 36,
		Name:    "seed-user-tag-key",
		Stmts: []string{
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES
				('user', '', 1, 'context', 'single', 'OS user that created the WMS entity; auto-applied at creation from the session/process user (TEAMSTER_USER else os user). Faceting key for multi-user fabrics.')`,
		},
	},
	{
		Version: 37,
		Name:    "warmup-recovery",
		Stmts: []string{
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES
				('phase', 'admin', 1, 'lifecycle', 'single', 'Orientation/warmup/coordination cost before the session declared a work focus.')`,
			`CREATE TABLE IF NOT EXISTS warmup_evidence (
				message_id       VARCHAR(128) NOT NULL,
				entity_type      VARCHAR(32)  NOT NULL DEFAULT '',
				entity_id        VARCHAR(128) NOT NULL DEFAULT '',
				warmup_start     DATETIME(6)  NOT NULL,
				first_focus_at   DATETIME(6)  NOT NULL,
				recovered_at     DATETIME(6)  NOT NULL,
				PRIMARY KEY (message_id),
				INDEX idx_we_entity (entity_type, entity_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		Version: 38,
		Name:    "synthesis-evidence",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS synthesis_evidence (
				message_id       VARCHAR(128) NOT NULL,
				entity_type      VARCHAR(32)  NOT NULL DEFAULT '',
				entity_id        VARCHAR(128) NOT NULL DEFAULT '',
				session_id       VARCHAR(64)  NOT NULL DEFAULT '',
				confidence       VARCHAR(16)  NOT NULL DEFAULT '',
				evidence_excerpt TEXT         NOT NULL,
				mapping_source   VARCHAR(255) NOT NULL DEFAULT '',
				recovered_at     DATETIME(6)  NOT NULL,
				PRIMARY KEY (message_id),
				INDEX idx_se_entity (entity_type, entity_id),
				INDEX idx_se_session (session_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description) VALUES
				('source', '', 1, 'context', 'single', 'Provenance marker for WMS entities created by automated processes (e.g. source:synthesized for LLM-synthesized outcomes).')`,
		},
	},
	{
		Version: 39,
		Name:    "gap-recovery",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS gap_evidence (
				message_id        VARCHAR(128) NOT NULL,
				entity_type       VARCHAR(32)  NOT NULL DEFAULT '',
				entity_id         VARCHAR(128) NOT NULL DEFAULT '',
				session_id        VARCHAR(64)  NOT NULL DEFAULT '',
				agent_name        VARCHAR(64)  NOT NULL DEFAULT '',
				resolution_method VARCHAR(48)  NOT NULL DEFAULT '',
				resolved_from_entity VARCHAR(192) NOT NULL DEFAULT '',
				recovered_at      DATETIME(6)  NOT NULL,
				PRIMARY KEY (message_id),
				INDEX idx_ge_entity (entity_type, entity_id),
				INDEX idx_ge_session (session_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		// Mark phase and product as required (system-expected tags). The
		// sweep, classifier, and cost-by-product/phase dashboards depend on these.
		// work-type was already required since v30. Users can retire these via
		// wms_retireTag if they accept the dashboard/recovery consequences.
		Version: 40,
		Name:    "required-phase-product",
		Stmts: []string{
			`UPDATE tags SET required = 1 WHERE tag_key = 'phase'`,
			`UPDATE tags SET required = 1 WHERE tag_key = 'product'`,
		},
	},
	{
		// Extend entity_tags_resolved with lifecycle-tag promotion: when cost is
		// attributed to an outcome that has no lifecycle tag (work-type, phase),
		// bubble the dominant value from its workunits up to the outcome row.
		// This closes the "(untagged)" gap in cost explorer facets — lifecycle
		// tags live on workunits, but cost_facts can point at outcomes directly
		// (synthesized_outcome, gap_recovery, admin_warmup methods).
		//
		// The promotion picks the MODE (most-common value) per (outcome, tag_key)
		// via a correlated subquery with LIMIT 1. If no workunits carry the key,
		// no row is produced (LEFT JOIN semantics in the dashboard query handle
		// that as "(untagged)" — irreducible).
		//
		// Only lifecycle-category keys are promoted; context keys already inherit
		// downward (leg 2). The NOT EXISTS guard prevents promotion when the
		// outcome already carries an explicit binding for that key.
		Version: 41,
		Name:    "entity-tags-resolved-promote-lifecycle",
		Stmts: []string{
			`CREATE OR REPLACE VIEW entity_tags_resolved AS
/* Leg 1: direct tags */
SELECT et.entity_type, et.entity_id, et.tag_id
FROM entity_tags et
UNION ALL
/* Leg 2: inheritance DOWN — outcome CONTEXT tags → workunits (lifecycle excluded) */
SELECT 'workunit', w.id, et.tag_id
FROM workunits w
JOIN entity_tags et ON et.entity_type = 'outcome' AND et.entity_id = w.outcome_id
JOIN tags ti ON ti.id = et.tag_id AND ti.category = 'context'
WHERE NOT EXISTS (
  SELECT 1 FROM entity_tags e2 JOIN tags t2 ON t2.id = e2.tag_id
  WHERE e2.entity_type = 'workunit' AND e2.entity_id = w.id AND t2.tag_key = ti.tag_key
)
UNION ALL
/* Leg 3: promotion UP — workunit lifecycle tags → parent outcome (mode) */
SELECT 'outcome', o.id, dominant.tag_id
FROM outcomes o
JOIN (
  SELECT w2.outcome_id, et2.tag_id, t3.tag_key,
         ROW_NUMBER() OVER (PARTITION BY w2.outcome_id, t3.tag_key ORDER BY COUNT(*) DESC) AS rn
  FROM workunits w2
  JOIN entity_tags et2 ON et2.entity_type = 'workunit' AND et2.entity_id = w2.id
  JOIN tags t3 ON t3.id = et2.tag_id AND t3.category = 'lifecycle'
  GROUP BY w2.outcome_id, et2.tag_id, t3.tag_key
) dominant ON dominant.outcome_id = o.id AND dominant.rn = 1
WHERE NOT EXISTS (
  SELECT 1 FROM entity_tags e3 JOIN tags t4 ON t4.id = e3.tag_id
  WHERE e3.entity_type = 'outcome' AND e3.entity_id = o.id AND t4.tag_key = dominant.tag_key
)`,
		},
	},
	{
		Version: 42,
		Name:    "outcome-cost-rollup",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS outcome_cost_rollup (
				bucket_day   DATE         NOT NULL,
				outcome_id   VARCHAR(64)  NOT NULL,
				source_type  VARCHAR(32)  NOT NULL,
				source_id    VARCHAR(128) NOT NULL DEFAULT '',
				model        VARCHAR(64)  NOT NULL DEFAULT '',
				agent_name   VARCHAR(128) NOT NULL DEFAULT '',
				tokens       BIGINT UNSIGNED NOT NULL DEFAULT 0,
				cost_usd     DECIMAL(12,6) NOT NULL DEFAULT 0,
				PRIMARY KEY (bucket_day, outcome_id, source_type, source_id, model, agent_name),
				INDEX idx_ocr_outcome (outcome_id),
				INDEX idx_ocr_day (bucket_day)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		Version: 43,
		Name:    "cost-rollup-hourly-grain",
		Stmts: []string{
			`ALTER TABLE cost_rollup ADD COLUMN bucket_hour DATETIME NOT NULL DEFAULT '1970-01-01' AFTER bucket_day`,
			`UPDATE cost_rollup SET bucket_hour = bucket_day`,
			`ALTER TABLE cost_rollup DROP PRIMARY KEY, ADD PRIMARY KEY (bucket_hour, entity_type, entity_id, agent_name, model)`,
			`ALTER TABLE outcome_cost_rollup ADD COLUMN bucket_hour DATETIME NOT NULL DEFAULT '1970-01-01' AFTER bucket_day`,
			`UPDATE outcome_cost_rollup SET bucket_hour = bucket_day`,
			`ALTER TABLE outcome_cost_rollup DROP PRIMARY KEY, ADD PRIMARY KEY (bucket_hour, outcome_id, source_type, source_id, model, agent_name)`,
		},
	},
	{
		Version: 44,
		Name:    "tag-conventions",
		Stmts: []string{
			`ALTER TABLE tags ADD COLUMN scope VARCHAR(16) NOT NULL DEFAULT ''`,
			`ALTER TABLE tags ADD COLUMN exclusion_group VARCHAR(64) NOT NULL DEFAULT ''`,
			`ALTER TABLE tags ADD COLUMN auto_extract VARCHAR(32) NOT NULL DEFAULT ''`,
			`ALTER TABLE tags ADD COLUMN interview VARCHAR(16) NOT NULL DEFAULT 'propose'`,
			`UPDATE tags SET scope = 'outcome' WHERE tag_key IN ('product','priority','product-version','feature','bug')`,
			`UPDATE tags SET scope = 'workunit' WHERE tag_key IN ('component','phase','work-type','resolution')`,
			`UPDATE tags SET exclusion_group = 'work-scope' WHERE tag_key IN ('feature','bug')`,
			`UPDATE tags SET auto_extract = 'git' WHERE tag_key LIKE 'github.%' OR tag_key LIKE 'gitlab.%' OR tag_key LIKE 'git.%' OR tag_key LIKE 'jira.%' OR tag_key LIKE 'linear.%'`,
			`UPDATE tags SET interview = 'auto' WHERE tag_key LIKE 'github.%' OR tag_key LIKE 'gitlab.%' OR tag_key LIKE 'git.%'`,
			`UPDATE tags SET interview = 'skip' WHERE tag_key IN ('phase','work-type','resolution','lifecycle','component','user','source')`,
		},
	},
	{
		// Brief-directive recovery: attribute a focus-less remote TEAMMATE's cost
		// to the entity its dispatch brief told it to focus on (the mandated
		// wms_setFocus directive the teammate never executed). The remote scraper
		// ships the directive to /focus-timeline as a kind='focus' interval with
		// identity_source='brief_directive'; rollup --recover-directives resolves
		// it to an outcome and re-attributes the session's unallocated/skipped
		// messages with method='brief_directive_recovery'. Reversible via
		// --unrecover-directives. directive_evidence records the provenance.
		Version: 45,
		Name:    "brief-directive-recovery",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS directive_evidence (
				message_id    VARCHAR(128) NOT NULL,
				entity_type   VARCHAR(32)  NOT NULL DEFAULT '',
				entity_id     VARCHAR(128) NOT NULL DEFAULT '',
				session_id    VARCHAR(64)  NOT NULL DEFAULT '',
				agent_name    VARCHAR(128) NOT NULL DEFAULT '',
				directive_type VARCHAR(32) NOT NULL DEFAULT '',
				directive_id  VARCHAR(128) NOT NULL DEFAULT '',
				recovered_at  DATETIME(6)  NOT NULL,
				PRIMARY KEY (message_id),
				INDEX idx_de_entity (entity_type, entity_id),
				INDEX idx_de_session (session_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		// focus-interval-repair: provenance for the one-time repair of negative-width
		// focus intervals (ended_at < started_at) produced by the dual-writer / async
		// race before the focus-interval-dual-writer fix. rollup --repair-focus-intervals
		// recomputes each inverted row's ended_at from its successor in the
		// (session, agent) focus chain and records the prior (bad) ended_at here so the
		// repair is reversible via --unrepair-focus-intervals. interval_id is the PK of
		// the repaired wms_intervals row.
		Version: 46,
		Name:    "focus-interval-repair",
		Stmts: []string{
			`CREATE TABLE IF NOT EXISTS focus_interval_repair (
				interval_id     BIGINT UNSIGNED NOT NULL,
				prior_ended_at  DATETIME(6)  NULL,
				new_ended_at    DATETIME(6)  NULL,
				repaired_at     DATETIME(6)  NOT NULL,
				PRIMARY KEY (interval_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		},
	},
	{
		// lifecycle-category-backfill: repair tag value rows for system-managed
		// lifecycle keys (phase, work-type, resolution, lifecycle) that were
		// created with category='context' by the TagEntity / sweep_llm applyTag
		// code paths before the category-inheritance fix. Those code paths omitted
		// the category column in their INSERT, so new agent-created values (e.g.
		// phase:exec, phase:investigate) inherited the column DEFAULT 'context'
		// instead of the key's lifecycle category. The tag editor's loadKeyGroups
		// query sorts context-category rows first, so a single misclassified value
		// caused the entire key to appear in the context panel rather than the
		// lifecycle panel (L hotkey). This UPDATE is idempotent — safe on fresh
		// installs (no rows match) and heals any drifted instance.
		Version: 47,
		Name:    "lifecycle-category-backfill",
		Stmts: []string{
			`UPDATE tags SET category = 'lifecycle'
				WHERE tag_key IN ('phase', 'work-type', 'resolution', 'lifecycle')
				  AND category != 'lifecycle'`,
		},
	},
	{
		// Widen synthesis_evidence.confidence varchar(16)→varchar(32). The value
		// "temporal_correlation" (21 chars) written by synthesize_remote.go exceeds
		// the current limit, producing Error 1406 on every rollup sweep. MODIFY
		// COLUMN to a wider VARCHAR is supported and idempotent on MySQL 8.0 and
		// MariaDB 11.8 (re-run is a no-op), exactly as v31/v33 widened other columns.
		Version: 48,
		Name:    "widen-synthesis-confidence",
		Stmts: []string{
			`ALTER TABLE synthesis_evidence MODIFY confidence VARCHAR(32) NOT NULL DEFAULT ''`,
		},
	},
	{
		Version: 49,
		Name:    "add-facet-source",
		Stmts: []string{
			`ALTER TABLE tags ADD COLUMN facet_source VARCHAR(64) NOT NULL DEFAULT '' AFTER interview`,
			// Set facet_source on existing feature/bug rows.
			`UPDATE tags SET facet_source = 'work-type' WHERE tag_key IN ('feature', 'bug') AND facet_source = ''`,
			// Seed the 6 new facet keys with full metadata.
			// INSERT IGNORE so this is idempotent if the keys already exist from create-on-apply or DefineTag.
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description, scope, exclusion_group, interview, facet_source)
			 VALUES
			 ('refactor', '', 1, 'context', 'single', 'Refactor slug — identifies the specific refactoring purpose', 'outcome', 'work-scope', 'propose', 'work-type'),
			 ('infra', '', 1, 'context', 'single', 'Infrastructure slug — identifies the specific infra work', 'outcome', 'work-scope', 'propose', 'work-type'),
			 ('docs', '', 1, 'context', 'single', 'Documentation slug — identifies the specific doc effort', 'outcome', 'work-scope', 'propose', 'work-type'),
			 ('research', '', 1, 'context', 'single', 'Research slug — identifies the specific investigation', 'outcome', 'work-scope', 'propose', 'work-type'),
			 ('test', '', 1, 'context', 'single', 'Test slug — identifies the specific validation target', 'outcome', 'work-scope', 'propose', 'work-type'),
			 ('admin', '', 1, 'context', 'single', 'Admin slug — identifies the specific admin task', 'outcome', 'work-scope', 'propose', 'work-type')`,
			// For rows that already existed (from create-on-apply or DefineTag), set missing metadata.
			`UPDATE tags SET facet_source = 'work-type', scope = 'outcome', exclusion_group = 'work-scope', interview = 'propose', cardinality = 'single'
			 WHERE tag_key IN ('refactor', 'infra', 'docs', 'research', 'test', 'admin') AND facet_source = ''`,
			// Also ensure feature and bug have exclusion_group and scope (belt-and-suspenders with v44).
			`UPDATE tags SET exclusion_group = 'work-scope' WHERE tag_key IN ('feature', 'bug') AND exclusion_group = ''`,
		},
	},
}

// mergeProjectToProduct renames `project` tag rows to `product`, handling the
// unique-index conflict when a `product` stub already exists for the same value.
//
// For each `project` row:
//   - If no `product` row with the same tag_value exists: UPDATE tag_key to 'product'.
//     entity_tags bindings follow automatically (they hold tags.id, not key name).
//   - If a `product` row already exists for that value (the empty-value stub from v26):
//     entity_tags bindings on the project row must be re-pointed to the product row,
//     then the project row is deleted.
//
// Runs under the migrate advisory lock (same as backfillV1ToV3).
func mergeProjectToProduct(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Collect all `project` rows.
	rows, err := tx.QueryContext(ctx,
		`SELECT id, tag_value FROM tags WHERE tag_key = 'project'`)
	if err != nil {
		return fmt.Errorf("mergeProjectToProduct: query project rows: %w", err)
	}
	type projectRow struct {
		id       int64
		tagValue string
	}
	var projectRows []projectRow
	for rows.Next() {
		var r projectRow
		if err := rows.Scan(&r.id, &r.tagValue); err != nil {
			rows.Close()
			return fmt.Errorf("mergeProjectToProduct: scan: %w", err)
		}
		projectRows = append(projectRows, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("mergeProjectToProduct: rows err: %w", err)
	}

	for _, pr := range projectRows {
		// Check if a product row with the same value already exists.
		var productID int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM tags WHERE tag_key = 'product' AND tag_value = ?`,
			pr.tagValue).Scan(&productID)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("mergeProjectToProduct: lookup product row: %w", err)
		}

		if err == sql.ErrNoRows {
			// No conflict: rename in place. entity_tags bindings follow via tags.id.
			if _, err := tx.ExecContext(ctx,
				`UPDATE tags SET tag_key = 'product' WHERE id = ?`, pr.id); err != nil {
				return fmt.Errorf("mergeProjectToProduct: rename project row %d: %w", pr.id, err)
			}
			slog.Info("mergeProjectToProduct: renamed project row to product",
				"tag_id", pr.id, "tag_value", pr.tagValue)
		} else {
			// Conflict: a product row already exists. Re-point any entity_tags that
			// reference the project row to the product row, then delete the project row.
			if _, err := tx.ExecContext(ctx,
				`UPDATE entity_tags SET tag_id = ? WHERE tag_id = ?`,
				productID, pr.id); err != nil {
				return fmt.Errorf("mergeProjectToProduct: re-point entity_tags for row %d: %w", pr.id, err)
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM tags WHERE id = ?`, pr.id); err != nil {
				return fmt.Errorf("mergeProjectToProduct: delete project row %d: %w", pr.id, err)
			}
			slog.Info("mergeProjectToProduct: merged project row into existing product row",
				"project_tag_id", pr.id, "product_tag_id", productID, "tag_value", pr.tagValue)
		}
	}

	if len(projectRows) == 0 {
		slog.Info("mergeProjectToProduct: no project rows found, nothing to do")
	}
	return tx.Commit()
}

// v1StatusToV3 maps a v1 status (project/goal/task/workitem) to the v3 status
// machine plus any lifecycle/resolution tag the §10 mapping attaches with done.
// The returned tagKey/tagValue is "" when no tag applies.
func v1StatusToV3(v1 string) (status, tagKey, tagValue string) {
	switch v1 {
	case "planning", "open", "pending", "":
		return "pending", "", ""
	case "active", "assigned":
		return "active", "", ""
	case "review":
		return "review", "", ""
	case "blocked":
		return "blocked", "", ""
	case "complete", "achieved":
		return "done", "resolution", "achieved"
	case "abandoned":
		return "done", "resolution", "abandoned"
	case "archived":
		return "done", "lifecycle", "archived"
	default:
		return "pending", "", ""
	}
}

// backfillV1ToV3 reads the v1 tables and writes equivalent v3 Outcome/WorkUnit
// rows, edges, dependencies, and re-pointed tag/event/journal rows. It runs in a
// single transaction and is idempotent: a second run is a no-op because every
// write is INSERT IGNORE keyed on the deterministic v3 id (out-<id> / wu-<id>),
// and the re-point UPDATEs match only rows still carrying a v1 entity_type.
func backfillV1ToV3(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	now := time.Now().UTC()

	// applyTag upserts a tag vocabulary row (non-seed) and binds it to a v3
	// entity. Mirrors the store's TagEntity write path, inline so the migration
	// has no dependency on store methods.
	applyTag := func(entityType, entityID, key, val string) error {
		res, e := tx.ExecContext(ctx,
			`INSERT INTO tags (tag_key, tag_value, is_seed, description) VALUES (?, ?, 0, '')
			 ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`, key, val)
		if e != nil {
			return e
		}
		tagID, e := res.LastInsertId()
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx,
			`INSERT IGNORE INTO entity_tags (entity_type, entity_id, tag_id, source, applied_at)
			 VALUES (?, ?, ?, 'backfill', ?)`, entityType, entityID, tagID, now)
		return e
	}

	// 1. Projects → strategic Outcomes (out-<id>), tagged scope:strategic + project:<name>.
	projRows, err := tx.QueryContext(ctx,
		`SELECT id, name, description, status, focus, origin_host, origin_session, origin_agent FROM projects`)
	if err != nil {
		return fmt.Errorf("backfill read projects: %w", err)
	}
	type projRec struct{ id, name, desc, status, focus, oh, os, oa string }
	var projects []projRec
	for projRows.Next() {
		var p projRec
		if err := projRows.Scan(&p.id, &p.name, &p.desc, &p.status, &p.focus, &p.oh, &p.os, &p.oa); err != nil {
			projRows.Close() //nolint:errcheck
			return err
		}
		projects = append(projects, p)
	}
	projRows.Close() //nolint:errcheck
	if err := projRows.Err(); err != nil {
		return err
	}
	for _, p := range projects {
		oid := "out-" + p.id
		st, tagK, tagV := v1StatusToV3(p.status)
		if _, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO outcomes (id, title, description, status, prior_status,
				focus, origin_host, origin_session, origin_agent, created_at, updated_at)
			VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?)`,
			oid, p.name, p.desc, st, p.focus, p.oh, p.os, p.oa, now, now); err != nil {
			return fmt.Errorf("backfill insert outcome %s: %w", oid, err)
		}
		if err := applyTag("outcome", oid, "scope", "strategic"); err != nil {
			return err
		}
		if p.name != "" {
			if err := applyTag("outcome", oid, "project", p.name); err != nil {
				return err
			}
		}
		if tagK != "" {
			if err := applyTag("outcome", oid, tagK, tagV); err != nil {
				return err
			}
		}
	}

	// 2. Goals → tactical Outcomes (out-<id>), edge from project's outcome, scope:tactical.
	goalRows, err := tx.QueryContext(ctx,
		`SELECT id, title, project_id, description, status, focus, origin_host, origin_session, origin_agent FROM goals`)
	if err != nil {
		return fmt.Errorf("backfill read goals: %w", err)
	}
	type goalRec struct{ id, title, projectID, desc, status, focus, oh, os, oa string }
	var goals []goalRec
	for goalRows.Next() {
		var g goalRec
		if err := goalRows.Scan(&g.id, &g.title, &g.projectID, &g.desc, &g.status, &g.focus, &g.oh, &g.os, &g.oa); err != nil {
			goalRows.Close() //nolint:errcheck
			return err
		}
		goals = append(goals, g)
	}
	goalRows.Close() //nolint:errcheck
	if err := goalRows.Err(); err != nil {
		return err
	}
	for _, g := range goals {
		oid := "out-" + g.id
		st, tagK, tagV := v1StatusToV3(g.status)
		if _, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO outcomes (id, title, description, status, prior_status,
				focus, origin_host, origin_session, origin_agent, created_at, updated_at)
			VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?)`,
			oid, g.title, g.desc, st, g.focus, g.oh, g.os, g.oa, now, now); err != nil {
			return fmt.Errorf("backfill insert goal-outcome %s: %w", oid, err)
		}
		if err := applyTag("outcome", oid, "scope", "tactical"); err != nil {
			return err
		}
		if tagK != "" {
			if err := applyTag("outcome", oid, tagK, tagV); err != nil {
				return err
			}
		}
		// Edge parent(project outcome) → child(goal outcome), only if the parent exists.
		if g.projectID != "" {
			parent := "out-" + g.projectID
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outcomes WHERE id = ?`, parent).Scan(&n); err != nil {
				return err
			}
			if n > 0 && parent != oid {
				if _, err := tx.ExecContext(ctx,
					`INSERT IGNORE INTO outcome_edges (parent_id, child_id) VALUES (?, ?)`, parent, oid); err != nil {
					return fmt.Errorf("backfill edge %s→%s: %w", parent, oid, err)
				}
			}
		}
	}

	// 3. Tasks → WorkUnits (wu-<id>), outcome_id = goal's outcome (out-<goal_id>).
	// The v1 tasks table has no agent_id (agents live on work_items); the WorkUnit
	// gets an empty agent_id — assignment isn't carried from v1 tasks.
	taskRows, err := tx.QueryContext(ctx,
		`SELECT id, title, goal_id, description, status, focus, origin_host, origin_session, origin_agent FROM tasks`)
	if err != nil {
		return fmt.Errorf("backfill read tasks: %w", err)
	}
	type taskRec struct{ id, title, goalID, desc, status, focus, oh, os, oa string }
	var tasks []taskRec
	for taskRows.Next() {
		var t taskRec
		if err := taskRows.Scan(&t.id, &t.title, &t.goalID, &t.desc, &t.status, &t.focus, &t.oh, &t.os, &t.oa); err != nil {
			taskRows.Close() //nolint:errcheck
			return err
		}
		tasks = append(tasks, t)
	}
	taskRows.Close() //nolint:errcheck
	if err := taskRows.Err(); err != nil {
		return err
	}
	orphanTasks := 0
	for _, t := range tasks {
		wid := "wu-" + t.id
		outcomeID := "out-" + t.goalID
		// A WorkUnit needs a valid parent outcome (FK). If the task's goal didn't
		// resolve to an outcome (orphan/standalone task), skip it rather than
		// violate the FK — counted (logged below), preserved in archived_v1_tasks
		// by the later rename, recoverable by the operator. NEVER a silent drop.
		if t.goalID == "" {
			orphanTasks++
			continue
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outcomes WHERE id = ?`, outcomeID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			orphanTasks++
			continue
		}
		st, tagK, tagV := v1StatusToV3(t.status)
		if _, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO workunits (id, outcome_id, title, description, status, prior_status,
				agent_id, focus, origin_host, origin_session, origin_agent, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, ?, ?)`,
			wid, outcomeID, t.title, t.desc, st, t.focus, t.oh, t.os, t.oa, now, now); err != nil {
			return fmt.Errorf("backfill insert workunit %s: %w", wid, err)
		}
		if tagK != "" {
			if err := applyTag("workunit", wid, tagK, tagV); err != nil {
				return err
			}
		}
	}

	// 4. Re-point ancillary rows from v1 entity_type/entity_id to the v3 ids.
	// entity_tags, wms_event_records, wms_journal all key on (entity_type, entity_id).
	// project/goal → outcome with id out-<id>; task/workitem → workunit with id wu-<id>.
	// workitem maps onto its OWN wu-<workitem.id>? No: WorkItems are absorbed into
	// their task's WorkUnit, so a workitem's ancillary rows re-point to wu-<task.id>.
	// We resolve workitem→task via the work_items table before it is renamed.
	//
	// Re-point only when the mapped v3 target actually exists. Projects and goals
	// ALWAYS produce an out-<id> outcome (step 1/2 never skip), so those arms are
	// unconditional. Tasks, however, are SKIPPED for orphans (no resolvable parent
	// goal/outcome — step 3), so wu-<task.id> may not exist; an absorbed workitem
	// whose parent task was orphaned likewise has no target. Guarding on the target
	// keeps orphan-task ancillary rows on their original entity_type='task'/'workitem'
	// — consistent with the archived_v1_* row, recoverable, never moved onto a
	// phantom WorkUnit. (No FK on entity_id would catch the dangling write.)
	//
	// uniqueKeyed tables can collide when a task AND its absorbed workitem carry
	// the same tag (both re-point to wu-<task.id>): the second UPDATE would hit
	// the (entity_type, entity_id, tag_id) PK. For those tables we UPDATE IGNORE
	// (skip the colliding row) then DELETE the genuine collision leftovers — but
	// ONLY rows whose mapped target exists; orphan-task leftovers are preserved.
	repoint := func(table string, uniqueKeyed bool) error {
		ignore := ""
		if uniqueKeyed {
			ignore = "IGNORE "
		}
		// project → outcome, goal → outcome (out-<id> always exists)
		if _, e := tx.ExecContext(ctx,
			`UPDATE `+ignore+table+` SET entity_type='outcome', entity_id=CONCAT('out-', entity_id) WHERE entity_type='project'`); e != nil {
			return fmt.Errorf("repoint %s project: %w", table, e)
		}
		if _, e := tx.ExecContext(ctx,
			`UPDATE `+ignore+table+` SET entity_type='outcome', entity_id=CONCAT('out-', entity_id) WHERE entity_type='goal'`); e != nil {
			return fmt.Errorf("repoint %s goal: %w", table, e)
		}
		// task → workunit, only when wu-<task.id> was actually created (skip orphans)
		if _, e := tx.ExecContext(ctx,
			`UPDATE `+ignore+table+` a SET a.entity_type='workunit', a.entity_id=CONCAT('wu-', a.entity_id)
			 WHERE a.entity_type='task'
			   AND EXISTS (SELECT 1 FROM workunits w WHERE w.id = CONCAT('wu-', a.entity_id))`); e != nil {
			return fmt.Errorf("repoint %s task: %w", table, e)
		}
		// workitem → its task's workunit (wu-<task.id>). Resolve via work_items,
		// and only when that task's WorkUnit exists (skip workitems of orphan tasks).
		if _, e := tx.ExecContext(ctx,
			`UPDATE `+ignore+table+` a JOIN work_items wi ON a.entity_id = wi.id
			 SET a.entity_type='workunit', a.entity_id=CONCAT('wu-', wi.task_id)
			 WHERE a.entity_type='workitem'
			   AND EXISTS (SELECT 1 FROM workunits w WHERE w.id = CONCAT('wu-', wi.task_id))`); e != nil {
			return fmt.Errorf("repoint %s workitem: %w", table, e)
		}
		if uniqueKeyed {
			// Drop v1-typed rows skipped by UPDATE IGNORE on a PK collision — the
			// target binding already exists on the merged entity. Restricted to rows
			// whose mapped target EXISTS, so orphan-task/workitem leftovers (target
			// never created) are preserved rather than silently dropped.
			if _, e := tx.ExecContext(ctx,
				`DELETE a FROM `+table+` a WHERE
				   (a.entity_type IN ('project','goal')
				      AND EXISTS (SELECT 1 FROM outcomes o WHERE o.id = CONCAT('out-', a.entity_id)))
				   OR (a.entity_type='task'
				      AND EXISTS (SELECT 1 FROM workunits w WHERE w.id = CONCAT('wu-', a.entity_id)))
				   OR (a.entity_type='workitem'
				      AND EXISTS (SELECT 1 FROM work_items wi JOIN workunits w ON w.id = CONCAT('wu-', wi.task_id)
				                  WHERE wi.id = a.entity_id))`); e != nil {
				return fmt.Errorf("repoint %s cleanup: %w", table, e)
			}
		}
		return nil
	}
	if err := repoint("entity_tags", true); err != nil {
		return err
	}
	for _, tbl := range []string{"wms_event_records", "wms_journal"} {
		if err := repoint(tbl, false); err != nil {
			return err
		}
	}

	// 5. work_dependencies → entity_dependencies, remapping ids+types both sides.
	//    project/goal→outcome(out-), task→workunit(wu-), workitem→wu-<task.id>.
	// Only insert when BOTH mapped endpoints actually exist as a v3 outcome/workunit.
	// An orphan-task endpoint maps to a wu-<id> that was never created (step 3 skip);
	// inserting such a dependency would leave a real WorkUnit permanently blocked
	// behind a phantom blocker (evaluateUnblock → getEntityStatus errors). The
	// derived table computes the mapped (type,id) once; the outer EXISTS guards
	// drop any edge whose blocker or blocked side has no real v3 row.
	if _, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO entity_dependencies (blocker_type, blocker_id, blocked_type, blocked_id, created_at)
		SELECT m.blocker_type, m.blocker_id, m.blocked_type, m.blocked_id, ?
		FROM (
			SELECT
				CASE WHEN wd.blocker_type IN ('project','goal') THEN 'outcome'
				     WHEN wd.blocker_type IN ('task','workitem') THEN 'workunit' END AS blocker_type,
				CASE WHEN wd.blocker_type IN ('project','goal') THEN CONCAT('out-', wd.blocker_id)
				     WHEN wd.blocker_type = 'task' THEN CONCAT('wu-', wd.blocker_id)
				     WHEN wd.blocker_type = 'workitem' THEN CONCAT('wu-', (SELECT task_id FROM work_items WHERE id = wd.blocker_id)) END AS blocker_id,
				CASE WHEN wd.blocked_type IN ('project','goal') THEN 'outcome'
				     WHEN wd.blocked_type IN ('task','workitem') THEN 'workunit' END AS blocked_type,
				CASE WHEN wd.blocked_type IN ('project','goal') THEN CONCAT('out-', wd.blocked_id)
				     WHEN wd.blocked_type = 'task' THEN CONCAT('wu-', wd.blocked_id)
				     WHEN wd.blocked_type = 'workitem' THEN CONCAT('wu-', (SELECT task_id FROM work_items WHERE id = wd.blocked_id)) END AS blocked_id
			FROM work_dependencies wd
			WHERE wd.blocker_type IN ('project','goal','task','workitem')
			  AND wd.blocked_type IN ('project','goal','task','workitem')
		) m
		WHERE (
			(m.blocker_type='outcome'  AND EXISTS (SELECT 1 FROM outcomes  o WHERE o.id = m.blocker_id))
			OR (m.blocker_type='workunit' AND EXISTS (SELECT 1 FROM workunits w WHERE w.id = m.blocker_id))
		) AND (
			(m.blocked_type='outcome'  AND EXISTS (SELECT 1 FROM outcomes  o WHERE o.id = m.blocked_id))
			OR (m.blocked_type='workunit' AND EXISTS (SELECT 1 FROM workunits w WHERE w.id = m.blocked_id))
		)`, now); err != nil {
		return fmt.Errorf("backfill dependencies: %w", err)
	}

	// Count WorkItems absorbed (their tags/events folded into the task's WU; no
	// separate WU is created — §10 absorb).
	var workItems int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items`).Scan(&workItems); err != nil {
		return err
	}

	// Prominent, non-silent summary (operator rule: never drop rows silently).
	// orphanTasks are NOT lost — they remain in the v1 tasks table, preserved
	// through the later archived_v1_tasks rename and recoverable.
	slog.Warn("wms v1→v3 backfill complete",
		"projects_migrated", len(projects),
		"goals_migrated", len(goals),
		"tasks_migrated", len(tasks)-orphanTasks,
		"work_items_absorbed", workItems,
		"orphan_tasks_skipped", orphanTasks,
		"orphan_note", "skipped tasks have no resolvable parent goal/outcome; preserved in v1 tasks table (recoverable post-rename)")

	return tx.Commit()
}

// backfillWmsIntervals copies wms_event_records → wms_intervals (kind='state')
// and agent_focus_intervals → wms_intervals (kind='focus'), carries identity
// onto state rows from their contemporaneous focus intervals (§2.3), THEN remaps
// usage_attribution.interval_id from the old wms_event_records.id to the new
// wms_intervals.id (B3-design §3.1 R2). OD-2 R2 (FINAL): both kinds take FRESH
// auto-increment ids — NOT id-preservation. R1 (preserve the state id) was
// REJECTED because the W2 dual-write claims wms_intervals ids 1..N at runtime
// BEFORE this backfill runs, so an explicit-id INSERT would PK-collide on the
// live upgrade path (the dual-written rows already own those ids).
//
// Upgrade-safe: the state/focus INSERTs anti-join on NATURAL identity
// (kind, entity_type, entity_id, started_at), so rows already dual-written
// between v21 and v23 are skipped — no duplicate, no collision. The remap then
// fixes every historical interval_id (which still points at a wms_event_records
// id) to the corresponding new wms_intervals row via the same natural-identity
// 1:1 join, BEFORE the v25 rename archives wms_event_records.
//
// NOT wrapped in a single transaction: re-run safety comes from the NOT EXISTS /
// remap guards, not from atomicity; the whole Func runs while the caller holds
// the teamster_migrate advisory lock, so no concurrent migrate interleaves.
func backfillWmsIntervals(ctx context.Context, db *sql.DB) error {
	// 1. STATE rows — FRESH auto-increment ids (R2). identity_source='direct' when
	//    the source row already had identity, else '' (step 3 fills it). The
	//    anti-join on NATURAL identity (kind='state', entity, started_at) skips
	//    rows already dual-written in the v21→v23 upgrade window AND makes a re-run
	//    a no-op. NO explicit id column — that is the R1 collision R2 avoids.
	stateRes, err := db.ExecContext(ctx, `
		INSERT INTO wms_intervals
			(kind, entity_type, entity_id, state, session_id, agent_name, host,
			 started_at, ended_at, duration_ms, phase, phase_source, assembled_at,
			 cost_usd, cost_tokens, identity_source)
		SELECT 'state', entity_type, entity_id, state, session_id, agent_name, host,
		       started_at, ended_at, duration_ms, phase, phase_source, assembled_at,
		       cost_usd, cost_tokens,
		       CASE WHEN session_id <> '' THEN 'direct' ELSE '' END
		FROM wms_event_records er
		WHERE NOT EXISTS (SELECT 1 FROM wms_intervals wi
		                  WHERE wi.kind = 'state' AND wi.entity_type = er.entity_type
		                    AND wi.entity_id = er.entity_id AND wi.started_at = er.started_at)`)
	if err != nil {
		return fmt.Errorf("backfill state intervals: %w", err)
	}
	stateCopied, _ := stateRes.RowsAffected()

	// 2. FOCUS rows — FRESH ids; state=''; recompute duration_ms for closed rows
	//    (focus intervals never stored it). NOT EXISTS on (kind='focus', session,
	//    entity, started_at) skips already-dual-written rows and makes a re-run a
	//    no-op.
	focusRes, err := db.ExecContext(ctx, `
		INSERT INTO wms_intervals
			(kind, entity_type, entity_id, state, session_id, agent_name, host,
			 started_at, ended_at, duration_ms, identity_source)
		SELECT 'focus', entity_type, entity_id, '', session_id, agent_name, '',
		       started_at, ended_at,
		       CASE WHEN ended_at IS NOT NULL
		            THEN TIMESTAMPDIFF(MICROSECOND, started_at, ended_at) / 1000 ELSE NULL END,
		       'direct'
		FROM agent_focus_intervals afi
		WHERE NOT EXISTS (SELECT 1 FROM wms_intervals wi
		                  WHERE wi.kind = 'focus' AND wi.session_id = afi.session_id
		                    AND wi.entity_type = afi.entity_type AND wi.entity_id = afi.entity_id
		                    AND wi.started_at = afi.started_at)`)
	if err != nil {
		return fmt.Errorf("backfill focus intervals: %w", err)
	}
	focusCopied, _ := focusRes.RowsAffected()

	// 3. Identity-carry (§2.3): for each kind='state' row with empty identity, find
	//    the best-overlapping kind='focus' row over the same entity and inherit its
	//    session_id + (TRIM-normalized, MF-3) agent_name; mark identity_source=
	//    'carried'. Idempotent: the WHERE skips rows already 'direct'/'carried' or
	//    already stamped. ROW_NUMBER() OVER is valid on MySQL 8.0.2+ and MariaDB
	//    10.2+/11.8.
	carryRes, err := db.ExecContext(ctx, `
		UPDATE wms_intervals s
		JOIN (
			SELECT st.id AS state_id, f.session_id, f.agent_name,
			       ROW_NUMBER() OVER (
			         PARTITION BY st.id
			         ORDER BY LEAST(IFNULL(st.ended_at, NOW(6)), IFNULL(f.ended_at, NOW(6)))
			                - GREATEST(st.started_at, f.started_at) DESC
			       ) AS rn
			FROM wms_intervals st
			JOIN wms_intervals f
			  ON f.kind = 'focus'
			 AND f.entity_type = st.entity_type
			 AND f.entity_id   = st.entity_id
			 AND f.started_at  < IFNULL(st.ended_at, NOW(6))
			 AND IFNULL(f.ended_at, NOW(6)) > st.started_at
			 AND f.session_id <> ''
			WHERE st.kind = 'state' AND st.session_id = '' AND st.identity_source <> 'direct'
		) best ON best.state_id = s.id AND best.rn = 1
		SET s.session_id = best.session_id,
		    s.agent_name = TRIM(LEADING '@' FROM best.agent_name),
		    s.identity_source = 'carried'`)
	if err != nil {
		return fmt.Errorf("backfill identity-carry: %w", err)
	}
	carried, _ := carryRes.RowsAffected()

	// 4. REMAP usage_attribution.interval_id (R2): the v20 column holds the OLD
	//    wms_event_records.id. Re-point it at the NEW wms_intervals.id (kind='state')
	//    via the natural-identity 1:1 join, while wms_event_records still exists
	//    (before the v25 rename). After this, interval_id is valid against
	//    wms_intervals and survives the rename — no post-deploy step.
	//
	//    Idempotency under id-space OVERLAP (both tables auto-increment from 1, so a
	//    remapped interval_id can numerically equal an UNRELATED wms_event_records.id):
	//    value alone cannot tell "old event-record pointer" from "already-remapped
	//    interval pointer". We disambiguate by ENTITY — a temporal_join attribution
	//    row's covering interval is always for its OWN entity:
	//      - the event_records join is anchored to ua's own entity (er.entity_type/
	//        entity_id = ua.entity_type/entity_id), so a row can only ever map to an
	//        interval for its own entity;
	//      - the NOT EXISTS guard skips a row whose interval_id ALREADY resolves to a
	//        kind='state' wms_intervals row FOR THAT SAME ENTITY — i.e. genuinely
	//        already remapped. A not-yet-remapped row's interval_id is an event-record
	//        id; for the guard to false-skip it, that numeric id would have to also be
	//        a wms_intervals state id for the SAME entity — in which case the target is
	//        an equivalent interval for that entity anyway, so skipping is harmless.
	//    Result: a re-run (crash retry) touches nothing and never mis-fires.
	remapRes, err := db.ExecContext(ctx, `
		UPDATE usage_attribution ua
		JOIN wms_event_records er
		  ON ua.interval_id = er.id
		 AND er.entity_type = ua.entity_type AND er.entity_id = ua.entity_id
		JOIN wms_intervals wi
		  ON wi.kind = 'state' AND wi.entity_type = er.entity_type
		 AND wi.entity_id = er.entity_id AND wi.started_at = er.started_at
		SET ua.interval_id = wi.id
		WHERE ua.interval_id <> 0
		  AND NOT EXISTS (SELECT 1 FROM wms_intervals done
		                  WHERE done.id = ua.interval_id AND done.kind = 'state'
		                    AND done.entity_type = ua.entity_type
		                    AND done.entity_id = ua.entity_id)`)
	if err != nil {
		return fmt.Errorf("backfill remap interval_id: %w", err)
	}
	remapped, _ := remapRes.RowsAffected()

	slog.Warn("wms_intervals backfill complete",
		"state_rows_copied", stateCopied,
		"focus_rows_copied", focusCopied,
		"identity_carried", carried,
		"interval_id_remapped", remapped,
		"note", "R2: fresh ids both kinds (dual-write-safe); interval_id remapped old event_records.id → new wms_intervals.id before v25 rename")

	return nil
}

// migrationStep is a single named version step. All statements within a step
// run in order; failure aborts the migration and the version is NOT recorded.
// Func, if non-nil, runs after all Stmts succeed and may perform data
// transformations that require Go logic rather than plain DDL.
type migrationStep struct {
	Version int
	Name    string
	Stmts   []string
	Func    func(ctx context.Context, db *sql.DB) error
}

// migrateLockName is the MySQL named lock that serializes migrate() across the
// several processes that open the store concurrently at install/boot (hookd,
// wms-mcp, classify, rollup, and the explicit `teamster store migrate` step).
const migrateLockName = "teamster_migrate"

// migrateLockTimeout is how long a losing caller waits for the winner to finish
// migrating before giving up. Migrations are small DDL; a generous bound still
// fails loudly rather than hanging a process forever.
const migrateLockTimeout = 30 * time.Second

// highestKnownVersion returns the largest Version among the migration steps
// this binary ships — the schema version it can bring a database up to.
func highestKnownVersion() int {
	max := 0
	for _, step := range migrations {
		if step.Version > max {
			max = step.Version
		}
	}
	return max
}

// migrate brings db to current schema. The schema_version table tracks which
// steps have already been applied. Idempotent: running against a current
// schema is a no-op.
//
// Concurrency: several processes call this against the same database within a
// few milliseconds at install/boot. MySQL DDL is not transactional and the
// loop is not atomic, so two callers could otherwise both see a version as
// unapplied and both run its (non-idempotent) statements — the loser hitting
// "Error 1060 Duplicate column". To prevent that we serialize the whole run
// behind a MySQL named advisory lock held on a single pinned connection: the
// winner migrates while losers block on GET_LOCK, then proceed and find every
// version already recorded (the version scan happens after the lock is held,
// so it observes the winner's committed rows).
func migrate(ctx context.Context, db *sql.DB) error {
	// Pin one connection for the whole run: GET_LOCK/RELEASE_LOCK are
	// connection-scoped, and the migration statements must run on the same
	// connection that holds the lock.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migrate connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	var locked sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT GET_LOCK(?, ?)`, migrateLockName, int(migrateLockTimeout.Seconds()),
	).Scan(&locked); err != nil {
		return fmt.Errorf("acquire migrate lock: %w", err)
	}
	// GET_LOCK returns 1 on success, 0 on timeout, NULL on error (e.g. killed).
	if !locked.Valid || locked.Int64 != 1 {
		return fmt.Errorf("acquire migrate lock %q: timed out after %s (another migration in progress)", migrateLockName, migrateLockTimeout)
	}
	// Release on every exit path, including error and panic. Use a fresh
	// context: the caller's ctx may already be done by the time we unwind.
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, relErr := conn.ExecContext(relCtx, `SELECT RELEASE_LOCK(?)`, migrateLockName); relErr != nil {
			slog.Warn("release migrate lock failed", "lock", migrateLockName, "error", relErr)
		}
	}()

	return runMigrations(ctx, conn, db)
}

// runMigrations applies every unapplied step. conn is the pinned connection the
// caller holds the migrate advisory lock on; the schema_version reads/writes and
// the per-step DDL run on it. db is the original pool, passed to a step's Func
// (e.g. the v1→v3 backfill) which opens its own statements — that is still
// serialized because losers block at GET_LOCK on conn. Reading schema_version
// here (after the lock is held) is what lets a waiting loser no-op: it sees the
// winner's recorded versions.
func runMigrations(ctx context.Context, conn *sql.Conn, db *sql.DB) error {
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
			version INT NOT NULL PRIMARY KEY,
			name    VARCHAR(128) NOT NULL,
			applied_at DATETIME(6) NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	applied := map[int]bool{}
	maxApplied := 0
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_version`)
	if err != nil {
		return fmt.Errorf("list schema_version: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		applied[v] = true
		if v > maxApplied {
			maxApplied = v
		}
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return err
	}
	// Forward-compatibility guard: refuse to run against a database whose schema
	// was written by a newer binary. Applying only the steps we know would leave
	// us operating against a schema we don't understand (hub/remote skew or a
	// post-downgrade case), so fail loudly instead.
	if highest := highestKnownVersion(); maxApplied > highest {
		return fmt.Errorf("database schema v%d is newer than this binary supports (v%d) — upgrade the binary", maxApplied, highest)
	}
	for _, step := range migrations {
		if applied[step.Version] {
			continue
		}
		for _, stmt := range step.Stmts {
			if err := execMigrationStmt(ctx, conn, stmt); err != nil {
				return fmt.Errorf("apply v%d %s: %w", step.Version, step.Name, err)
			}
		}
		if step.Func != nil {
			if err := step.Func(ctx, db); err != nil {
				return fmt.Errorf("migration %d %s func: %w", step.Version, step.Name, err)
			}
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO schema_version (version, name, applied_at) VALUES (?, ?, UTC_TIMESTAMP(6))`,
			step.Version, step.Name,
		); err != nil {
			return fmt.Errorf("record v%d: %w", step.Version, err)
		}
	}
	return nil
}

// execMigrationStmt runs one migration statement. For an ALTER TABLE whose only
// clauses are ADD COLUMN, it first drops any clause whose column already exists
// (checked via information_schema, which is identical on MySQL and MariaDB) and
// runs only the remaining clauses — so a re-run, or a step that partially
// applied before a crash, is a no-op instead of a duplicate-column error. The
// advisory lock already prevents the concurrent race; this is defense-in-depth.
//
// Portability: we use an information_schema column-exists guard rather than
// MariaDB's `ADD COLUMN IF NOT EXISTS` (not valid on MySQL < 8.0.29) or catching
// a backend error number. Non-ADD-COLUMN statements run unchanged.
//
// Per-statement atomicity assumption: the partial-rewrite path below rebuilds a
// multi-ADD ALTER from ONLY the clauses whose columns are still missing. This is
// correct for every current migration because a multi-ADD-COLUMN ALTER TABLE is
// atomic per statement on both MySQL and MariaDB — either all its columns land
// or none do, so a crash never leaves a strict SUBSET of the columns present.
// The guard therefore only ever sees "all present" (skip) or "none present"
// (re-run the whole statement verbatim); a mixed subset cannot occur.
//
// FUTURE RISK: this breaks if a single ADD-COLUMN ALTER ever mixes a column name
// that ALSO exists from an earlier migration AND a later clause whose `AFTER`
// target is one of the missing columns in the same statement. Then the rebuilt
// ALTER would contain only the missing clauses, and an `AFTER <col>` referencing
// a clause we dropped would dangle (unknown column) or reorder unexpectedly. No
// current migration has overlapping column names across statements, so this path
// is safe today; a future migration that does must not rely on this rewrite.
func execMigrationStmt(ctx context.Context, conn *sql.Conn, stmt string) error {
	table, clauses, ok := parseAddColumnAlter(stmt)
	if !ok {
		_, err := conn.ExecContext(ctx, stmt)
		return err
	}
	var missing []string
	for _, c := range clauses {
		exists, err := columnExists(ctx, conn, table, c.column)
		if err != nil {
			return err
		}
		if !exists {
			missing = append(missing, c.clause)
		}
	}
	if len(missing) == 0 {
		return nil // every target column already present
	}
	rebuilt := fmt.Sprintf("ALTER TABLE %s %s", table, strings.Join(missing, ",\n"))
	_, err := conn.ExecContext(ctx, rebuilt)
	return err
}

// columnExists reports whether table has column in the current schema. DATABASE()
// scopes the lookup to the connection's active schema on both MySQL and MariaDB.
func columnExists(ctx context.Context, conn *sql.Conn, table, column string) (bool, error) {
	var n int
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("column-exists check %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

type addColumnClause struct {
	column string // the column name being added
	clause string // the full clause, e.g. "ADD COLUMN x INT NOT NULL DEFAULT 0 AFTER y"
}

// parseAddColumnAlter returns the table and the per-column ADD COLUMN clauses if
// stmt is an ALTER TABLE whose every comma-separated clause is ADD COLUMN. It
// reports ok=false for any other statement (a mixed ALTER with DROP/MODIFY/ADD
// INDEX, a non-ALTER, or anything it can't confidently parse) so such statements
// run verbatim — we only special-case the shape we can rewrite safely.
func parseAddColumnAlter(stmt string) (table string, clauses []addColumnClause, ok bool) {
	// Normalize whitespace to single spaces for prefix matching; keep a trimmed
	// copy for splitting clauses.
	flat := strings.Join(strings.Fields(stmt), " ")
	const prefix = "ALTER TABLE "
	if !strings.HasPrefix(strings.ToUpper(flat), prefix) {
		return "", nil, false
	}
	rest := flat[len(prefix):]
	// Table name is the first token (no backtick-quoted names are used here).
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return "", nil, false
	}
	table = rest[:sp]
	body := strings.TrimSpace(rest[sp+1:])

	// Split into clauses on TOP-LEVEL commas only: commas inside parentheses
	// (e.g. DECIMAL(14,6), ENUM('a','b')) belong to a column type, not a clause
	// boundary. Quotes are tracked so a comma inside a string default is also
	// ignored.
	for _, raw := range splitTopLevel(body) {
		clause := strings.TrimSpace(raw)
		up := strings.ToUpper(clause)
		if !strings.HasPrefix(up, "ADD COLUMN ") {
			return "", nil, false // a non-ADD-COLUMN clause → don't rewrite
		}
		fields := strings.Fields(clause)
		if len(fields) < 3 {
			return "", nil, false
		}
		clauses = append(clauses, addColumnClause{column: fields[2], clause: clause})
	}
	if len(clauses) == 0 {
		return "", nil, false
	}
	return table, clauses, true
}

// splitTopLevel splits s on commas that are not inside parentheses or single
// quotes, so a comma within DECIMAL(14,6) or a quoted DEFAULT does not break a
// clause. A doubled ” inside a quoted string is a literal quote (SQL escaping).
func splitTopLevel(s string) []string {
	var parts []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			if inQuote && i+1 < len(s) && s[i+1] == '\'' {
				i++ // skip the escaped quote
				continue
			}
			inQuote = !inQuote
		case inQuote:
			// inside a string: ignore structural characters
		case c == '(':
			depth++
		case c == ')':
			if depth > 0 {
				depth--
			}
		case c == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

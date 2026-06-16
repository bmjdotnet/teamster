-- Least-privilege read-only MySQL user for the Grafana "Teamster MySQL"
-- datasource (uid teamster-mysql). SELECT only, scoped to the spine tables the
-- D1-D4 dashboards query. This is NOT the TEAMSTER_STORE_DSN account — Grafana
-- never writes.
--
-- @install applies this during a `grafana-mode=install` install:
--   1. Substitute the two placeholders below:
--        __GRAFANA_DB_USER__      -> the GrafanaDBUser provisioning var (default 'grafana_ro')
--        __GRAFANA_DB_PASSWORD__  -> the GrafanaDBPassword provisioning var (generated, never in git)
--        __STORE_DB__             -> the StoreDB (database name parsed from TEAMSTER_STORE_DSN, e.g. 'teamster')
--   2. Run it against the WMS MySQL as a user with CREATE USER + GRANT OPTION.
--        Under store-mode=install that is the fresh MySQL admin; under store-mode=managed
--        the StoreDSN app account is least-privilege and will NOT have these, so a separate
--        admin/socket-root path is required.
--        e.g. `mysql --host=<StoreHost> --port=<StorePort> -u <admin> -p < grafana-readonly-user.sql`
-- Idempotent: CREATE USER IF NOT EXISTS + ALTER USER refreshes the password on
-- re-run; GRANTs are additive. Re-running never escalates beyond SELECT.
--
-- Connection host is '%' so the datasource can reach MySQL whether the spine is
-- local or on another host; tighten to a specific host if your fabric requires.

CREATE USER IF NOT EXISTS '__GRAFANA_DB_USER__'@'%' IDENTIFIED BY '__GRAFANA_DB_PASSWORD__';
ALTER USER '__GRAFANA_DB_USER__'@'%' IDENTIFIED BY '__GRAFANA_DB_PASSWORD__';

-- Fact + attribution + rollup spine (cost panels).
GRANT SELECT ON `__STORE_DB__`.`token_ledger`        TO '__GRAFANA_DB_USER__'@'%';
GRANT SELECT ON `__STORE_DB__`.`usage_attribution`   TO '__GRAFANA_DB_USER__'@'%';
GRANT SELECT ON `__STORE_DB__`.`cost_rollup`         TO '__GRAFANA_DB_USER__'@'%';
GRANT SELECT ON `__STORE_DB__`.`wms_intervals`       TO '__GRAFANA_DB_USER__'@'%';

-- Tag/dimension spine ($dimension / $tag_key faceting).
GRANT SELECT ON `__STORE_DB__`.`tags`                TO '__GRAFANA_DB_USER__'@'%';
GRANT SELECT ON `__STORE_DB__`.`entity_tags`         TO '__GRAFANA_DB_USER__'@'%';

-- Entity tables (per-entity labels/joins for the $dimension selector).
-- v3-LIVE entity tables only. Do NOT add projects/goals/tasks/work_items here:
-- migration v17 (migrations.go RENAME TABLE) renamed them to archived_v1_*, so on
-- any v3-migrated DB the bare names DO NOT EXIST. A GRANT on a missing table fails
-- with ERROR 1146 and — because mysql runs this as one batch (no --force, by
-- design, so real failures aren't masked) — ABORTS the whole script, leaving
-- grafana_ro uncreated and every D1-D4 MySQL panel unauthorized. The dashboards
-- only need outcomes + workunits anyway; if a panel ever needs archived v1 data,
-- grant on archived_v1_* explicitly. Keep this list in lockstep with the v3 schema.
--
-- Same precedent applies to the B3 interval unification: wms_event_records and
-- agent_focus_intervals collapse into wms_intervals (granted above), and migration
-- v25 RENAMEs the originals to archived_v2_*. Do NOT grant the bare old names or the
-- archived_v2_* names — granting a renamed table aborts the batch with ERROR 1146,
-- exactly as for v1. The single wms_intervals grant carries all state + focus rows.
GRANT SELECT ON `__STORE_DB__`.`outcomes`            TO '__GRAFANA_DB_USER__'@'%';
GRANT SELECT ON `__STORE_DB__`.`workunits`           TO '__GRAFANA_DB_USER__'@'%';
GRANT SELECT ON `__STORE_DB__`.`sessions`            TO '__GRAFANA_DB_USER__'@'%';

-- v29 cost views (migration v29). CREATE OR REPLACE VIEW is idempotent so
-- these exist on any DB that has run v29 or had the views created manually.
-- Views are treated the same as tables for GRANT purposes.
GRANT SELECT ON `__STORE_DB__`.`cost_facts`            TO '__GRAFANA_DB_USER__'@'%';
GRANT SELECT ON `__STORE_DB__`.`entity_tags_resolved`  TO '__GRAFANA_DB_USER__'@'%';

-- WMS lifecycle journal (status transitions, cycle-time panels).
GRANT SELECT ON `__STORE_DB__`.`wms_journal`           TO '__GRAFANA_DB_USER__'@'%';

-- Outcome cost hierarchy (v42 migration). Materialized by rollup binary.
GRANT SELECT ON `__STORE_DB__`.`outcome_cost_rollup`   TO '__GRAFANA_DB_USER__'@'%';

FLUSH PRIVILEGES;

-- Least-privilege read-only MySQL user for `teamster clone`'s source-side
-- verification queries (I7 — WP3-data-leg.md §4, DESIGN.md I7). SELECT only,
-- scoped to the two databases a clone ever restores from (teamster,
-- claude_telemetry). This is NOT the TEAMSTER_STORE_DSN app account, which
-- holds GRANT ALL — clone must never run its verification queries against
-- the source through that credential.
--
-- @install applies this on every install where MySQL is locally
-- administrable (mirrors provision_grafana_ro's pattern exactly):
--   1. Substitute the two placeholders below:
--        __CLONE_VERIFY_RO_USER__      -> "clone_verify_ro"
--        __CLONE_VERIFY_RO_PASSWORD__  -> generated, never in git
--        __STORE_DB__                  -> the StoreDB (database name parsed
--                                          from TEAMSTER_STORE_DSN, e.g. 'teamster')
--   2. Run it against the WMS MySQL as a user with CREATE USER + GRANT OPTION
--      (socket-root `sudo mysql`, same as provision_grafana_ro).
-- Idempotent: CREATE USER IF NOT EXISTS + ALTER USER refreshes the password
-- on re-run; GRANTs are additive. Re-running never escalates beyond SELECT.
--
-- Host-scoped to 'localhost' — unlike grafana_ro (which serves a remote
-- Grafana datasource), clone's own source-side verification queries always
-- run locally on the source host itself (R9: `teamster clone` runs from the
-- source instance, never over SSH against itself).
--
-- claude_telemetry is granted unconditionally: it may not exist yet on a
-- host that predates that database's own install-time provisioning, in
-- which case this GRANT is a harmless no-op against a database that isn't
-- there — CREATE USER/ALTER USER above still succeed either way.

CREATE USER IF NOT EXISTS '__CLONE_VERIFY_RO_USER__'@'localhost' IDENTIFIED BY '__CLONE_VERIFY_RO_PASSWORD__';
ALTER USER '__CLONE_VERIFY_RO_USER__'@'localhost' IDENTIFIED BY '__CLONE_VERIFY_RO_PASSWORD__';

GRANT SELECT ON `__STORE_DB__`.* TO '__CLONE_VERIFY_RO_USER__'@'localhost';
GRANT SELECT ON `claude_telemetry`.* TO '__CLONE_VERIFY_RO_USER__'@'localhost';

FLUSH PRIVILEGES;

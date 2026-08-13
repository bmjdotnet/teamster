#!/usr/bin/env bash
# Runs the full Go test suite against a real MySQL backend so store/migration
# tests exercise MySQL instead of silently t.Skip-ing (TEAMSTER_TEST_MYSQL_DSN
# unset). Reuses an already-running test MySQL on the target port if one
# answers; otherwise starts a throwaway container and tears it down on exit.
# Any container this script starts fresh also gets the sentinel database
# internal/store/testguard requires before it will let a test touch it — see
# SENTINEL_SCHEMA below.
#
# The container is tuned for test-suite speed, never production durability —
# it is disposable BY DESIGN (see MYSQL_TUNED_* below for the flag-by-flag
# rationale, and CONTRIBUTING.md / CLAUDE.md's "test MySQL" notes for the
# summary). If it ever ends up in a bad state, `docker rm -f` it — for the
# ephemeral container this script does that itself on exit; for the
# persistent one below, `docker rm -f teamster-test-mysql` and recreate with
# `--persistent`. Never try to repair a test database in place.
#
# Usage:
#   scripts/test-with-mysql.sh [go test args...]
#       Run the suite. Reuses a MySQL already listening on MYSQL_TEST_PORT;
#       otherwise starts an ephemeral (--rm) tuned container and tears it
#       down on exit. Package parallelism is capped at MYSQL_TEST_PARALLELISM
#       (default 4, overridable by the env var or by passing your own -p in
#       [go test args...]) — see the rationale by MYSQL_TEST_PARALLELISM
#       below; tuning the container stopped high parallelism being fatal, not
#       wasteful.
#
#   scripts/test-with-mysql.sh --persistent
#       (Re)create a long-lived tuned container (no --rm, survives after this
#       script exits) instead of running tests. Fails loudly — rather than
#       silently replacing it — if a container by that name already exists;
#       this is the one supported way to stand up or recreate the shared test
#       MySQL instance, so a hand-rolled `docker run` should never be needed.
set -euo pipefail

# Must match internal/store/testguard.SentinelSchema exactly — that package
# refuses to let any test run DDL against a server missing this database (no
# escape hatch; see its doc comment). Its presence is the one-time, explicit
# "this server is a disposable test instance" marker, so it is only ever
# created here on a container THIS script just started fresh — never on an
# already-running server it merely reused, which would silently mark
# something the script didn't provision as test-safe.
SENTINEL_SCHEMA="_teamster_test_server"

MYSQL_TEST_HOST="${MYSQL_TEST_HOST:-127.0.0.1}"
MYSQL_TEST_PORT="${MYSQL_TEST_PORT:-13306}"

# MYSQL_TEST_PARALLELISM caps `go test`'s package-level parallelism (-p) when
# testing against the tuned MySQL. Left at Go's default (GOMAXPROCS, i.e. host
# CPU count), every migration-running package races the SAME server at once —
# tuning alone stops that being FATAL (see the flag rationale below), it
# doesn't stop it being wasteful: measured on this host (8 CPUs, isolated
# scratch container, `go test ./... -skip <flaky-unrelated-test> -count=1`,
# five runs, one -p value per run, no other load on the container):
#
#   -p 8 (default/GOMAXPROCS): 196.2s   -p 3: 167.0s
#   -p 6:                      197.2s   -p 2: 206.3s
#   -p 4:                      167.9s
#
# The knee is p=3/p=4 (~15% faster than the default, and clearly faster than
# going lower still) — NOT a "more cores = faster" curve. That's expected:
# `internal/store/mysql`'s migration path serializes on a single MySQL named
# advisory lock (`teamster_migrate` in internal/store/mysql/migrations.go),
# shared across every schema on the server regardless of which package holds
# it — raising -p doesn't parallelize migrations themselves, it just changes
# how many packages queue for that one lock at once, and each queued caller
# times out its wait after `migrateLockTimeout` (30s, in the same file) if
# the holder doesn't finish first. None of the 5 measured -p values came
# anywhere close to that 30s wait in practice (checked the logs), but the
# margin thins as -p rises and the suite grows — this cap is deliberately on
# the low side of the knee rather than exactly at it, trading a little more
# wall-clock for headroom. 4 was picked over 3 (same wall-clock in the
# measurement, essentially noise) so the many fast pure-Go packages
# (internal/display, internal/pricing, internal/redact, ...) still get to
# overlap rather than being squeezed by a cap sized only for the DB-bound
# ones. Not derived from nproc/2 or any other formula — MySQL's own
# single-migration-lock throughput is the real bottleneck, not CPU count, so
# a different host's core count doesn't imply a different right answer here;
# override this if a different host or CI environment measures otherwise.
#
# Deliberately NOT raising migrateLockTimeout itself as additional headroom:
# it's production code shared with a live install's own migration path (a
# real installer waiting on a stuck migration), not test-only, so widening it
# changes real deploy behavior for a margin the measurements above show
# isn't currently needed.
#
# Re-verified in a second sweep with quiet conditions actively checked (not
# assumed) around every single -p run — pgrep for other go build/vet/test
# processes and `uptime` load average, both before and after: 8:177.1s,
# 6:181.0s, 4:171.2s, 3:171.6s, 2:208.8s. Same shape, same knee at p=3/p=4,
# absolute numbers shifted a few % (normal run-to-run variance) but the
# conclusion is unchanged. The first sweep's numbers above stand as originally
# measured; this note exists because that first sweep ran on a host that,
# unknown at the time, had another agent's flight-check repeatedly saturating
# 2+ cores nearby — a *parallelism* cap is exactly the kind of measurement
# CPU noise can quietly bias, so it was re-run rather than left uncertain.
MYSQL_TEST_PARALLELISM="${MYSQL_TEST_PARALLELISM:-4}"

PERSISTENT=0
if [[ "${1:-}" == "--persistent" ]]; then
	PERSISTENT=1
	shift
fi

# Ephemeral runs and the persistent instance use different default container
# names (both overridable via MYSQL_TEST_CONTAINER) so `--persistent`'s
# default matches the long-running instance's established name rather than
# the throwaway-run default.
if [[ "$PERSISTENT" -eq 1 ]]; then
	MYSQL_TEST_CONTAINER="${MYSQL_TEST_CONTAINER:-teamster-test-mysql}"
else
	MYSQL_TEST_CONTAINER="${MYSQL_TEST_CONTAINER:-teamster-mysql-test-ephemeral}"
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DSN="mysql://root:test@${MYSQL_TEST_HOST}:${MYSQL_TEST_PORT}/"

# --- Tuning for a throwaway test database ---
#
# Every test creates a fresh schema and runs the full ~63-migration chain, so
# DDL/fsync cost dominates wall-clock time far more than it would for normal
# application queries. Stock `mysql:8.0` ships production-durability defaults
# (fsync every commit, binlog on, doublewrite on, 128MB buffer pool) on a
# disk-backed volume — safe for data you keep, wasteful for data you throw
# away every run. None of the flags below are appropriate for a database
# anyone needs to survive a crash; that tradeoff is the entire point here.
#
# Split into two arrays because docker distinguishes host/container-runtime
# options (before the image name) from the command passed to the image's
# entrypoint (after it) — the mysql:8.0 entrypoint forwards trailing args
# straight to `mysqld`.
MYSQL_TUNED_DOCKER_ARGS=(
	# Datadir lives entirely in RAM: zero physical disk I/O for any of it.
	# Also means the container's data cannot outlive the container — exactly
	# the disposability this whole tuning profile assumes.
	--tmpfs /var/lib/mysql:rw,size=8g
)
MYSQL_TUNED_MYSQLD_ARGS=(
	# No binary log. Nothing in this suite exercises replication, GTIDs, or
	# point-in-time recovery (verified: no test references binlog/GTID).
	--skip-log-bin
	# Don't fsync the InnoDB redo log on every transaction commit (flushed
	# ~once/sec instead). Even with the datadir on tmpfs, MySQL still issues
	# flush calls through the VFS layer for durability guarantees we don't
	# need here; this skips them.
	--innodb-flush-log-at-trx-commit=0
	# No doublewrite buffer. It exists to protect against torn pages after an
	# unclean crash — irrelevant when a corrupted container is removed and
	# recreated from scratch, never repaired in place.
	--innodb-doublewrite=0
	# Skip fsync/fdatasync on data-file writes entirely. A real, documented
	# InnoDB option (intended for exactly this: benchmarking/throwaway
	# instances), not a hack.
	--innodb-flush-method=nosync
	# Default is 128MB. Let the whole test workload's working set fit in
	# memory instead of thrashing the pool across the many schemas a
	# parallel `go test` run creates.
	--innodb-buffer-pool-size=4G
	# Bigger redo log = fewer log-switch checkpoints during the DDL-heavy
	# migration burst every fresh test schema runs.
	--innodb-log-file-size=512M
	# Default (151) can be tight under go test's per-package parallelism
	# opening many connections at once.
	--max-connections=500
	# Saves the several-hundred-MB baseline memory cost and small per-query
	# instrumentation overhead; nothing here introspects its own
	# performance_schema.
	--performance-schema=OFF
)

wait_for_mysql() {
	local container="$1"
	echo "waiting for $container to accept connections..." >&2
	for _ in $(seq 1 60); do
		if docker exec "$container" mysqladmin ping -h127.0.0.1 -uroot -ptest --silent >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	echo "error: $container never became ready" >&2
	return 1
}

# create_sentinel marks container as a verified-safe disposable test server —
# see SENTINEL_SCHEMA above. Only call this on a container the script just
# started itself.
create_sentinel() {
	local container="$1"
	docker exec "$container" mysql -h127.0.0.1 -uroot -ptest \
		-e "CREATE DATABASE IF NOT EXISTS ${SENTINEL_SCHEMA}" >/dev/null
}

port_open() {
	(exec 3<>"/dev/tcp/${MYSQL_TEST_HOST}/${MYSQL_TEST_PORT}") 2>/dev/null
}

if [[ "$PERSISTENT" -eq 1 ]]; then
	command -v docker >/dev/null 2>&1 || {
		echo "error: docker not found" >&2
		exit 1
	}
	if docker inspect "$MYSQL_TEST_CONTAINER" >/dev/null 2>&1; then
		echo "error: a container named '$MYSQL_TEST_CONTAINER' already exists." >&2
		echo "Remove it first if you mean to recreate it: docker rm -f $MYSQL_TEST_CONTAINER" >&2
		echo "(This destroys its data — disposability is the whole point — so make sure nothing needs it live first, and coordinate with anyone else pointed at it.)" >&2
		exit 1
	fi
	echo "creating persistent tuned MySQL container '$MYSQL_TEST_CONTAINER' on port $MYSQL_TEST_PORT" >&2
	docker run -d --name "$MYSQL_TEST_CONTAINER" \
		-p "${MYSQL_TEST_PORT}:3306" \
		-e MYSQL_ROOT_PASSWORD=test \
		"${MYSQL_TUNED_DOCKER_ARGS[@]}" \
		mysql:8.0 \
		"${MYSQL_TUNED_MYSQLD_ARGS[@]}" >/dev/null
	wait_for_mysql "$MYSQL_TEST_CONTAINER"
	create_sentinel "$MYSQL_TEST_CONTAINER"
	echo "'$MYSQL_TEST_CONTAINER' is up on port $MYSQL_TEST_PORT, sentinel database created." >&2
	echo "export TEAMSTER_TEST_MYSQL_DSN='${DSN}'" >&2
	exit 0
fi

started_container=""
cleanup() {
	if [[ -n "$started_container" ]]; then
		echo "stopping ephemeral container $started_container" >&2
		docker stop "$started_container" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

if port_open; then
	echo "reusing existing MySQL at ${MYSQL_TEST_HOST}:${MYSQL_TEST_PORT}" >&2
else
	command -v docker >/dev/null 2>&1 || {
		echo "error: docker not found and nothing is listening on ${MYSQL_TEST_HOST}:${MYSQL_TEST_PORT}" >&2
		echo "install docker, or export TEAMSTER_TEST_MYSQL_DSN yourself and run: cd src && go test ./..." >&2
		exit 1
	}
	echo "starting ephemeral tuned MySQL container $MYSQL_TEST_CONTAINER on port $MYSQL_TEST_PORT" >&2
	docker run --rm -d --name "$MYSQL_TEST_CONTAINER" \
		-p "${MYSQL_TEST_PORT}:3306" \
		-e MYSQL_ROOT_PASSWORD=test \
		"${MYSQL_TUNED_DOCKER_ARGS[@]}" \
		mysql:8.0 \
		"${MYSQL_TUNED_MYSQLD_ARGS[@]}" >/dev/null
	started_container="$MYSQL_TEST_CONTAINER"

	if ! wait_for_mysql "$MYSQL_TEST_CONTAINER"; then
		exit 1
	fi
	create_sentinel "$MYSQL_TEST_CONTAINER"
fi

cd "$REPO_ROOT/src"
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"
export TEAMSTER_TEST_MYSQL_DSN="$DSN"
# -p first so an explicit -p in "$@" (Go's flag parsing: last occurrence
# wins) still overrides MYSQL_TEST_PARALLELISM for a one-off run.
go test -p "$MYSQL_TEST_PARALLELISM" ./... "$@"

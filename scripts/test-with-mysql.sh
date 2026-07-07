#!/usr/bin/env bash
# Runs the full Go test suite against a real MySQL backend so store/migration
# tests exercise MySQL instead of silently t.Skip-ing (TEAMSTER_TEST_MYSQL_DSN
# unset). Reuses an already-running test MySQL on the target port if one
# answers; otherwise starts a throwaway container and tears it down on exit.
#
# Usage: scripts/test-with-mysql.sh [go test args...]
set -euo pipefail

MYSQL_TEST_HOST="${MYSQL_TEST_HOST:-127.0.0.1}"
MYSQL_TEST_PORT="${MYSQL_TEST_PORT:-13306}"
MYSQL_TEST_CONTAINER="${MYSQL_TEST_CONTAINER:-teamster-mysql-test-ephemeral}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DSN="mysql://root:test@${MYSQL_TEST_HOST}:${MYSQL_TEST_PORT}/"

started_container=""
cleanup() {
	if [[ -n "$started_container" ]]; then
		echo "stopping ephemeral container $started_container" >&2
		docker stop "$started_container" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

port_open() {
	(exec 3<>"/dev/tcp/${MYSQL_TEST_HOST}/${MYSQL_TEST_PORT}") 2>/dev/null
}

if port_open; then
	echo "reusing existing MySQL at ${MYSQL_TEST_HOST}:${MYSQL_TEST_PORT}" >&2
else
	command -v docker >/dev/null 2>&1 || {
		echo "error: docker not found and nothing is listening on ${MYSQL_TEST_HOST}:${MYSQL_TEST_PORT}" >&2
		echo "install docker, or export TEAMSTER_TEST_MYSQL_DSN yourself and run: cd src && go test ./..." >&2
		exit 1
	}
	echo "starting ephemeral MySQL container $MYSQL_TEST_CONTAINER on port $MYSQL_TEST_PORT" >&2
	docker run --rm -d --name "$MYSQL_TEST_CONTAINER" \
		-p "${MYSQL_TEST_PORT}:3306" \
		-e MYSQL_ROOT_PASSWORD=test \
		mysql:8.0 >/dev/null
	started_container="$MYSQL_TEST_CONTAINER"

	echo "waiting for $MYSQL_TEST_CONTAINER to accept connections..." >&2
	for _ in $(seq 1 60); do
		if docker exec "$MYSQL_TEST_CONTAINER" mysqladmin ping -h127.0.0.1 -uroot -ptest --silent >/dev/null 2>&1; then
			break
		fi
		sleep 1
	done
	if ! docker exec "$MYSQL_TEST_CONTAINER" mysqladmin ping -h127.0.0.1 -uroot -ptest --silent >/dev/null 2>&1; then
		echo "error: $MYSQL_TEST_CONTAINER never became ready" >&2
		exit 1
	fi
fi

cd "$REPO_ROOT/src"
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"
export TEAMSTER_TEST_MYSQL_DSN="$DSN"
go test ./... "$@"

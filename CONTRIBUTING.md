# Contributing to Teamster

Thanks for your interest in contributing to Teamster.

## Prerequisites

- **Go 1.25+** (check with `go version`)
- **MySQL or MariaDB** -- required for store integration tests (unit tests
  run without it)
- **Linux** -- the only supported platform

## Getting started

```bash
git clone https://github.com/bmjdotnet/teamster.git
cd teamster/src
go build ./...
go test ./...
go vet ./...
```

Store and migration tests require `TEAMSTER_TEST_MYSQL_DSN` (see below for
what happens if it's unset). The easiest way to run them is:

```bash
scripts/test-with-mysql.sh
```

This starts a throwaway MySQL 8 container (reusing one already listening on
`127.0.0.1:13306` if present), runs `go test ./...` against it, and tears
down any container it started.

**The test container is deliberately non-durable and disposable — this is
not a bug.** Every test creates a fresh schema and runs the full migration
chain, so DDL/fsync cost dominates wall-clock time. Stock MySQL defaults
(fsync every commit, binlog on, doublewrite on, 128MB buffer pool) are safe
for data you keep and needlessly slow for data you throw away every run; the
container `test-with-mysql.sh` starts trades that durability for roughly a
10x speedup (see the script for the flag-by-flag rationale). If the test
database ever gets into a bad state, `docker rm -f` it and let the script
recreate it clean — never try to repair it in place, and never point
`TEAMSTER_TEST_MYSQL_DSN` at a database that holds anything you'd miss.

To stand up (or recreate) the shared, long-lived instance other tests expect
at `127.0.0.1:13306` — rather than the script's own throwaway-per-run
container — use `scripts/test-with-mysql.sh --persistent`. It refuses to run
if a container by that name already exists, so recreating one requires an
explicit `docker rm -f teamster-test-mysql` first — a deliberate speed bump
before destroying shared test state.

To point at MySQL yourself, the DSN must be `mysql://` URL form, server-level
(no database name, trailing slash) so the per-test-schema harness can create
and drop isolated schemas:

```bash
export TEAMSTER_TEST_MYSQL_DSN='mysql://root:test@127.0.0.1:13306/'
cd src && go test ./...
```

**The test suite refuses to run against a server that isn't a verified
disposable test instance, and refuses to skip silently just because
`TEAMSTER_TEST_MYSQL_DSN` isn't set.** Two things enforce this
(`internal/store/testguard`):

- The target server must carry a sentinel database, `_teamster_test_server`.
  `scripts/test-with-mysql.sh` creates it automatically on any container it
  starts (fresh ephemeral run, or `--persistent`) — you don't need to do
  anything if you use the script. Pointing at a server yourself that the
  script didn't provision needs a one-time:
  ```bash
  mysql -h<host> -P<port> -u<user> -p<pass> -e "CREATE DATABASE _teamster_test_server"
  ```
  A server carrying a database literally named `teamster` — the live WMS
  install's schema — is refused unconditionally, sentinel or not. Neither
  check has a bypass; they exist specifically so a `TEAMSTER_TEST_MYSQL_DSN`
  typo can't point the test suite at a real Teamster install (the repo's own
  golden rule: "never test against the live instance you are sitting in").
- `TEAMSTER_TEST_MYSQL_DSN` unset (or set but unreachable) is a **hard
  failure**, not a silent skip — a `go test ./...` run that never touched
  MySQL used to still report green, which hid real mysql-only regressions.
  If you're intentionally running without MySQL (unit tests only, no
  Docker), set `TEAMSTER_TEST_ALLOW_SKIP=1` to get the old skip behavior back
  for that case. This escape hatch does **not** apply to the sentinel/
  live-schema checks above.

## Development workflow

1. Fork the repository and create a feature branch
2. Make your changes
3. Run `go build ./...`, `go test ./...`, and `go vet ./...`
4. Commit with a short imperative message focused on the "why"
5. Open a pull request

## Code style

- Follow existing conventions in the file you're editing
- No unnecessary comments -- the code should be self-documenting
- `go vet` must pass with no warnings
- Don't add features, abstractions, or cleanup beyond what the change requires

## Commit messages

Short imperative style. Focus on **why**, not what. Examples from this repo:

```
fix(grafana): land on dashboard list, rename three dashboards
feat(wms): add status filter to wms_listOutcomes
fix(export): require target dir as positional arg, remove hardcoded default
```

## Pull requests

- Describe what the change does and why
- Reference any related issue
- Include a test plan (how you verified it works)
- Keep changes focused -- one logical change per PR

## Testing

For changes touching the installer, hook client, or plugin, a clean install
on a test environment (not your live instance) is expected before marking
the work done. Unit tests passing is necessary but not sufficient.

## Reporting bugs

Open a GitHub issue with:
- What you expected to happen
- What actually happened
- Steps to reproduce
- Relevant environment details (OS, Go version, MySQL/MariaDB version)

## License

By contributing, you agree that your contributions will be licensed under
the [MIT License](LICENSE).

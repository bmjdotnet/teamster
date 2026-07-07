# Phase 00 baseline — d93c118

Recorded by @baseline (Storewright persistence-API kit, Phase 00). Canonical
known-red set confirmed by @team-lead (independently re-verified).

## Environment

- Git SHA: `d93c118d32fdec13df9510612c6b14e875e13835` (worktree
  `teamster-wt-phase-00-baseline`, branched from `main` via `tm wt new`; this
  worktree is now the home for the entire persistence-API kit effort — no
  per-phase worktrees, no folds to main until the whole effort completes).
- Timestamp: 2026-07-06.
- `TEAMSTER_TEST_MYSQL_DSN=mysql://root:test@127.0.0.1:13306` (exactly as
  specified; no trailing slash, no db path). Confirmed effective: 0 `SKIP`
  lines anywhere in the full `-v` test output; `internal/store/mysql`,
  `internal/store`, and `internal/rollup` all show real multi-hundred-second
  runtimes, not vacuous skips.
- Go toolchain: system go1.22.2, auto-switched to go1.25.0 per `go.mod`'s
  `go 1.25.0` directive (via `GOTOOLCHAIN`).

## Canonical gate command (all phases, per @team-lead)

```
cd src
export TEAMSTER_TEST_MYSQL_DSN='mysql://root:test@127.0.0.1:13306'
GOFLAGS=-buildvcs=false go build ./... && go vet ./... && go test ./...
```

`GOFLAGS=-buildvcs=false` is required unconditionally in this environment.
Without it, `go build ./...` fails (exit 1) for a reason unrelated to source
correctness: linking any `cmd/*` binary triggers Go's VCS build-info
stamping, which forks a `git status --porcelain` subprocess; in this
environment (traced with `strace`) that subprocess runs with cwd `$HOME`
instead of the module directory, and `$HOME` is not a git repository at
all, so the subprocess exits 128 and Go treats that as fatal. This is a
host/toolchain quirk (Go 1.25 auto-toolchain VCS stamping), not a code issue
— `go build ./internal/...` (no linked binaries) is clean without the flag,
and `GOFLAGS=-buildvcs=false go build ./...` is clean. VCS build-info
stamping is irrelevant to compilation correctness, which is what the gate
validates. `go vet` and `go test` are unaffected either way (no VCS stamping
involved).

## Canonical known-red set (2 tests)

Pre-existing, unrelated to the persistence-API kit's own scope, not ours to
fix in Phase 00. This is the set future phases' "no new red tests" gates
compare against.

| Test | Package | Failure |
|---|---|---|
| `TestSynthesizeRemoteOrphans_Idempotent` | `internal/rollup` | `synthesize_remote_test.go:236: pass1 synthesized=0, want 1` |
| `TestSynthesizeRemoteOrphans_DryRunWritesNothing` | `internal/rollup` | `synthesize_remote_test.go:338: dry-run synthesized=0, want 1 (must plan)` |

Both reproduced deterministically on repeat isolated `-run` invocations, and
independent of DSN format (trailing slash makes no difference). Root cause
not diagnosed — reviewed `synthesize_remote.go`'s orphan-candidate query and
concurrent-focus lookup and found no `time.Now()`-based cutoff, so it is not
a "wall-clock drifted past a fixed test date" issue. Confirmed out of scope
for Phase 00 by @team-lead; not investigated further, logged as the known-red
pair.

## Confirmed green (diverges from the kit's stale assumption)

The kit's original brief named these two as the expected known-reds. Both
are **green** at d93c118 — confirmed twice (full-suite run and isolated
`-run` rerun):

- `TestV30_WorkTypeRequiredByDefault` (`internal/store/mysql`)
- `TestUpdateWorkUnitStatus_HardEnforce` (`internal/store/mysql`)

The persisted memory record `store-tag-required-pretest-fail` claiming these
are pre-existing reds is stale for this SHA and should be updated/removed.

## Test-harness fix applied (test-only, zero prod risk)

`internal/server/brief_directive_test.go`'s `rawDriverDSN` helper assumed the
DSN always has a `/` after `host:port`:

```go
// before
slash := strings.Index(hostpart, "/")
host := hostpart[:slash]

// after
host := hostpart
if slash := strings.Index(hostpart, "/"); slash >= 0 {
    host = hostpart[:slash]
}
```

The exact DSN specified for this task (`mysql://root:test@127.0.0.1:13306`,
no trailing slash) has no `/` after the host:port, so the old code computed
`hostpart[:-1]` and panicked (`slice bounds out of range [:-1]`), taking down
`TestWriteFocusInterval_DualWriterDedup` with it. `internal/rollup`'s own DSN
parser (`rollup_test.go`'s `splitOn`) already handled the missing-slash case
safely; this helper did not. Confirmed fixed: `TestWriteFocusInterval_DualWriterDedup`
now passes (see full gate run below).

## Full gate result (post-fix)

See the full gate log for the complete run. Summary:
`go build` clean (with `-buildvcs=false`), `go vet` clean, `go test` shows
exactly the 2 canonical known-red tests above and nothing else.

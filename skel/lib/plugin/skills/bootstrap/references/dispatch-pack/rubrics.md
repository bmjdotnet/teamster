# Review Rubrics

These checklists are the ADVERSARIAL REVIEW gate from `execution-loop.md`.
Apply the relevant rubrics to every review. Each item is a concrete yes/no
check. "Probably fine" is not a passing answer.

A review that doesn't reach a finding on at least a few items wasn't thorough.
If every item passes cleanly, say so explicitly — don't just say "LGTM."

---

## Code Quality

- [ ] All identifiers (variables, functions, types) name what they *are*, not what they *do*
- [ ] No function longer than can be read without scrolling (rough limit: ~50 lines)
- [ ] No dead code: unreachable branches, unused variables, commented-out blocks
- [ ] No TODO or FIXME without a linked WMS task ID
- [ ] Error paths return errors; they do not silently swallow failures
- [ ] Error messages identify what failed and why, not just that it failed
- [ ] No magic numbers or strings — named constants for anything reused or meaningful
- [ ] No unnecessary abstraction: the code does what was asked, not what might be needed later
- [ ] No unnecessary comments: the code is self-documenting; comments explain WHY, not WHAT

---

## Architecture

- [ ] Each component has a single responsibility — it does one thing
- [ ] No circular dependencies between packages
- [ ] Interface boundaries are respected: callers depend on interfaces, not implementations
- [ ] New code follows the existing patterns in the file/package — no style islands
- [ ] No leaking of internal state across package boundaries
- [ ] Changes to shared interfaces (MCP tools, JSONL fields, hook contracts) are backward-compatible OR explicitly breaking with a migration path
- [ ] No cross-cutting concerns added without architectural justification
- [ ] The change is scoped to what the task required — no scope creep into adjacent code

---

## Security

- [ ] No command injection: external input never reaches `exec.Command` unsanitized
- [ ] No hardcoded secrets, tokens, passwords, or keys anywhere in the diff
- [ ] Input from external sources (user input, HTTP requests, env vars) is validated at the boundary
- [ ] No SQL string concatenation — parameterized queries only
- [ ] No path traversal: file paths from external input are cleaned and confined
- [ ] No new HTTP endpoints without authentication or explicit justification for public access
- [ ] Dependencies added to go.mod are from known, maintained sources
- [ ] No sensitive data written to logs or JSONL fields

---

## Testing

- [ ] New behavior has test coverage — happy path and at least one failure path
- [ ] Existing tests still pass: `go test ./...` clean
- [ ] Tests test behavior, not implementation — they don't reach into private fields
- [ ] Test names describe the scenario, not just the function under test
- [ ] No tests that always pass regardless of behavior (vacuous assertions)
- [ ] Integration-level behavior is covered, not just unit-level
- [ ] Edge cases are covered: empty input, zero values, boundary conditions
- [ ] Tests don't require manual setup or teardown the reviewer can't reproduce

---

## Planning

- [ ] The diff matches the task description — it does what was asked
- [ ] No unrelated changes mixed into the diff (refactors, formatting, renaming)
- [ ] No partial implementations: if a feature is incomplete, it is gated or documented
- [ ] No features added beyond what the task required
- [ ] If the task required a schema change, a migration is included
- [ ] If the task required a config change, documentation is updated
- [ ] Commit message describes *why*, not *what* — the diff already shows what
- [ ] WMS task status will reflect the actual state after this commit

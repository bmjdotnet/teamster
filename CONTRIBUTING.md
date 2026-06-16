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

Store and migration tests skip unless `TEAMSTER_TEST_MYSQL_DSN` is set.
To run them, point the DSN at a server-level connection (no database name).
The DSN must be server-level (note the trailing slash with no database name)
so the per-test-schema harness can create and drop isolated schemas:

```bash
export TEAMSTER_TEST_MYSQL_DSN='root:test@tcp(127.0.0.1:3306)/'
go test ./...
```

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

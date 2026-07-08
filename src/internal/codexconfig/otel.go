package codexconfig

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmjdotnet/teamster/internal/installbackup"
)

// OtelSpec describes the [otel] table Teamster writes into Codex's
// config.toml, pointing Codex's own OTLP metrics export at Teamster's
// dedicated Codex-only otelcol receiver (skel/etc/otelcol.yaml.tmpl's
// `otlp/codex` receiver — dashboards/fleet metrics only; this table never
// feeds token_ledger).
//
// exporter (logs/traces) is always rendered as "none" — not a field on this
// struct, see TeamsterOtelSpec's doc comment for why: Codex's OTLP HTTP
// exporter always POSTs to the bare endpoint root regardless of configured
// path (live-verified 2026-07-07 against Codex 0.137.0), so enabling logs
// alongside metrics on one receiver instance is a collector startup panic,
// not merely redundant.
//
// Unlike MCPServerSpec (SkipIfPresent — an operator's post-install hand-edit
// to a server table survives reinstall), this section is always written
// AlwaysUpsert: the verified-loadable shape is exact and narrow (inline-table
// backend selectors only; a bare non-"none"/"statsig" string on any of
// exporter/metrics_exporter/trace_exporter fails Codex's own config loader —
// research/otel-v1.md §3, verification-round3.md's OTEL matrix), so a stale
// prior write (a changed collector port, a flipped --otelcol-mode) must be
// replaced on every install run, never preserved.
type OtelSpec struct {
	// Environment mirrors Claude Code's
	// OTEL_RESOURCE_ATTRIBUTES=deployment.environment=... (config.Env).
	Environment string
	// MetricsBackend is the inline-table key naming the metrics export
	// destination: "otlp-http" or "otlp-grpc" are the only two backend names
	// verification-round3.md confirmed the struct-variant schema accepts.
	// TeamsterOtelSpec always uses "otlp-http" — live-verified 2026-07-07:
	// Codex's otlp-grpc exporter never even attempted a TCP connection to
	// the configured endpoint (two syntax forms tried, zero packets seen on
	// a raw listener), while otlp-http reliably delivered real payloads.
	MetricsBackend string
	// MetricsEndpoint is Teamster's dedicated Codex-only otelcol receiver,
	// e.g. "http://localhost:4329" (cfg.OtelCodexHTTPPort) — never
	// cfg.OtelHTTPPort, the receiver Claude Code shares (see
	// skel/etc/otelcol.yaml.tmpl's `otlp/codex` receiver doc comment for why
	// a dedicated receiver is required, not optional). The path component,
	// if any, is ignored by Codex's exporter (live-verified) — always POSTs
	// to the endpoint's bare root.
	MetricsEndpoint string
	// MetricsProtocol is the otlp-http inline table's `protocol` key. Unlike
	// otel-v1.md's original assumption, this field is MANDATORY for
	// otlp-http, not optional — live-verified 2026-07-07: omitting it fails
	// Codex's config load with `missing field \`protocol\` in
	// otel.metrics_exporter`, not just a schema nicety. render() defaults to
	// "binary" (the kit's verified-loadable value) if this is left empty and
	// MetricsBackend is "otlp-http", so a caller can't accidentally produce
	// a non-loading config by omission; set it explicitly for clarity.
	MetricsProtocol string
}

// render produces the [otel] block body. log_user_prompt, exporter, and
// trace_exporter are fixed, not fields on OtelSpec:
//   - log_user_prompt must stay false (the documented privacy default —
//     otel-v1.md §1.3, the JSONL tailer already gives Teamster full local
//     prompt content, there's no reason to also ship it off-host).
//   - exporter (logs/traces default) is fixed to "none" — see OtelSpec's doc
//     comment for the collector-panic reason this isn't merely redundant.
//   - trace_exporter is fixed to "none" — traces aren't part of this
//     integration (otel-v1.md §3).
func (s OtelSpec) render() string {
	var b strings.Builder
	b.WriteString("[otel]\n")
	fmt.Fprintf(&b, "environment = %s\n", quoteTOMLString(s.Environment))
	b.WriteString("log_user_prompt = false\n")
	b.WriteString("exporter = \"none\"\n")
	fmt.Fprintf(&b, "metrics_exporter = %s\n", s.metricsExporterInlineTable())
	b.WriteString("trace_exporter = \"none\"\n")
	return b.String()
}

func (s OtelSpec) metricsExporterInlineTable() string {
	protocol := s.MetricsProtocol
	if protocol == "" && s.MetricsBackend == "otlp-http" {
		protocol = "binary" // required field for otlp-http — see MetricsProtocol's doc comment.
	}
	if protocol != "" {
		return fmt.Sprintf("{ %s = { endpoint = %s, protocol = %s } }", s.MetricsBackend, quoteTOMLString(s.MetricsEndpoint), quoteTOMLString(protocol))
	}
	return fmt.Sprintf("{ %s = { endpoint = %s } }", s.MetricsBackend, quoteTOMLString(s.MetricsEndpoint))
}

// TeamsterOtelSpec builds the [otel] table Teamster writes for Codex.
// codexMetricsEndpoint is Teamster's dedicated Codex-only otelcol receiver
// (e.g. fmt.Sprintf("http://localhost:%d", cfg.OtelCodexHTTPPort) — NEVER
// cfg.OtelHTTPPort, the receiver Claude Code shares), and environment mirrors
// cfg.Env.
//
// Logs/traces are disabled (exporter="none") on purpose, not merely to keep
// the block small: Codex's OTLP HTTP exporter (OTel-OTLP-Exporter-Rust
// 0.31.0, live-verified 2026-07-07 against Codex 0.137.0) always POSTs to the
// bare endpoint root regardless of any path in the configured endpoint. If
// both metrics and logs were enabled, they'd collide on that same root path
// on one receiver instance, which panics the collector at startup ("multiple
// registrations for /") — a hard outage, not a soft degradation. Since
// Teamster doesn't need Codex's OTEL logs for anything (the JSONL tailer +
// hooks already give full local prompt/tool content, otel-v1.md §1.3),
// disabling them is the correct design, not a workaround for a bug.
func TeamsterOtelSpec(codexMetricsEndpoint, environment string) OtelSpec {
	return OtelSpec{
		Environment:     environment,
		MetricsBackend:  "otlp-http",
		MetricsEndpoint: codexMetricsEndpoint,
		MetricsProtocol: "binary",
	}
}

// WriteOtelConfig upserts the [otel] table into the config.toml at path
// (AlwaysUpsert — see OtelSpec's doc comment), then runs the post-write
// doctor gate (otel-v1.md's non-negotiable requirement, same contract as
// WriteMCPServers). On gate failure it rolls back to the pre-write backup and
// returns an error — callers must not treat a partially-applied,
// doctor-failing config.toml as installed.
//
// literalHeader "[otel]" guards the one case AlwaysUpsert's normal
// re-write-every-run behavior can't: an operator (or some other tool) that
// already hand-authored an unmarked [otel] table before Teamster ever ran.
// Writing a second [otel] header would be a TOML redefinition — exactly the
// syntax-conflict failure class verification-round3.md documents — so that
// case is left untouched and reported as UnmarkedCollision instead, same as
// WriteMCPServers' collision handling.
func WriteOtelConfig(path string, spec OtelSpec) (WriteResult, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return WriteResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	content := string(data)

	content, ur := UpsertSection(content, "otel", spec.render(), "[otel]", AlwaysUpsert)
	result := WriteResult{Servers: map[string]UpsertResult{"otel": ur}}
	if ur.UnmarkedCollision {
		slog.Warn("codexconfig: [otel] table already defined outside Teamster's markers — left untouched, OTEL export is NOT configured by Teamster on this host",
			"path", path)
		return result, nil
	}
	if !ur.Changed {
		return result, nil
	}

	backupPath, err := installbackup.Backup(path)
	if err != nil {
		return result, fmt.Errorf("backup %s before write: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return result, err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return result, fmt.Errorf("write %s: %w", path, err)
	}

	doctorResult, err := RunDoctorGate()
	result.Doctor = doctorResult
	if err != nil {
		return result, fmt.Errorf("run doctor gate: %w", err)
	}
	if doctorResult.Status == DoctorFailed {
		if rbErr := installbackup.Restore(backupPath, path); rbErr != nil {
			return result, fmt.Errorf("doctor gate rejected the write (%s), AND rollback failed: %w", doctorResult.Summary, rbErr)
		}
		result.RolledBack = true
		return result, fmt.Errorf("codex config.toml [otel] write rejected by doctor gate, rolled back: %s", doctorResult.Summary)
	}
	return result, nil
}

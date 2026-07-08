package codexconfig

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bmjdotnet/teamster/internal/installbackup"
)

// MCPServerSpec describes one [mcp_servers.<id>] table this installer owns
// and upserts skip-if-present — see UpsertSection's SkipIfPresent policy: an
// operator's own hand-edit to a previously-written block survives every
// subsequent install run. Do not add a TrustLevel/project field here — the
// installer must never write projects.*.trust_level (installer-gap.md
// design decision; that's the operator's call, not the installer's).
type MCPServerSpec struct {
	// ID is the mcp_servers table key, e.g. "wms" or "activity".
	ID string
	// Command is the absolute path to the server binary.
	Command string
	// DefaultToolsApprovalMode is normally "approve" (the verified one-line
	// fix for codex exec's silent-cancel-without-a-TTY behavior). Left
	// empty, the key is omitted entirely rather than written as "".
	DefaultToolsApprovalMode string
	// Env is rendered as a single inline table on the env key (never a
	// [mcp_servers.<id>.env] sub-table — both are valid TOML and parse
	// identically, but the inline form matches every other Teamster-written
	// block in this file and keeps the rendered section visually compact).
	// Keys are sorted for deterministic, diffable output.
	Env map[string]string
}

func (s MCPServerSpec) sectionName() string   { return mcpServerSectionName(s.ID) }
func (s MCPServerSpec) literalHeader() string { return mcpServerLiteralHeader(s.ID) }

func (s MCPServerSpec) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", s.ID)
	fmt.Fprintf(&b, "command = %s\n", quoteTOMLString(s.Command))
	if s.DefaultToolsApprovalMode != "" {
		fmt.Fprintf(&b, "default_tools_approval_mode = %s\n", quoteTOMLString(s.DefaultToolsApprovalMode))
	}
	if len(s.Env) > 0 {
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, fmt.Sprintf("%s = %s", k, quoteTOMLString(s.Env[k])))
		}
		fmt.Fprintf(&b, "env = { %s }\n", strings.Join(pairs, ", "))
	}
	return b.String()
}

// quoteTOMLString renders s as a TOML basic string. Go's strconv.Quote
// produces output compatible with TOML basic-string escaping (both use
// backslash-escaped \", \\, and control characters; both leave printable
// UTF-8 unescaped) for the value set this package ever renders — absolute
// paths, DSNs, hostnames, mode names. This is not a general TOML string
// encoder and doesn't need to be one: every value passed through it here is
// installer-controlled, not arbitrary user input.
func quoteTOMLString(s string) string {
	return strconv.Quote(s)
}

// WriteResult summarizes what WriteMCPServers did, keyed by MCPServerSpec.ID,
// for the installer's own logging. SkippedExisting and UnmarkedCollision are
// expected steady-state outcomes on a rerun, not failures — only a non-nil
// error from WriteMCPServers itself is a failure.
type WriteResult struct {
	Servers    map[string]UpsertResult
	Doctor     DoctorResult
	RolledBack bool
}

// WriteMCPServers non-destructively upserts each spec's [mcp_servers.<id>]
// table into the config.toml at path, then runs the post-write doctor gate
// (otel-v1.md's non-negotiable requirement: every config.toml write must be
// gated, because a malformed write doesn't just disable one feature, it
// breaks every codex invocation on the host). On gate failure, it rolls back
// to the pre-write backup and returns an error — callers must not treat a
// partially-applied, doctor-failing config.toml as installed.
//
// If every spec was already present (all SkippedExisting or
// UnmarkedCollision, nothing actually changed), the file is not rewritten at
// all and the doctor gate is not re-run — there is nothing new to validate.
func WriteMCPServers(path string, specs []MCPServerSpec) (WriteResult, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return WriteResult{}, fmt.Errorf("read %s: %w", path, err)
	}
	content := string(data)

	result := WriteResult{Servers: make(map[string]UpsertResult, len(specs))}
	changed := false
	for _, spec := range specs {
		var ur UpsertResult
		content, ur = UpsertSection(content, spec.sectionName(), spec.render(), spec.literalHeader(), SkipIfPresent)
		result.Servers[spec.ID] = ur
		if ur.UnmarkedCollision {
			slog.Warn("codexconfig: mcp server table already defined outside Teamster's markers — left untouched, Teamster's required env/approval-mode fields are NOT present on it",
				"id", spec.ID, "path", path)
		}
		if ur.Changed {
			changed = true
		}
	}
	if !changed {
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
		return result, fmt.Errorf("codex config.toml write rejected by doctor gate, rolled back: %s", doctorResult.Summary)
	}
	return result, nil
}

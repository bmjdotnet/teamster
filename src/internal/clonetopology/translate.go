// Package clonetopology translates a source instance's teamster.yaml into
// the lib/installrunner.sh flag vector for installing a clone. Pure
// function: YAML in, flag vector out — no I/O, no network, no SSH. See the
// WP2 topology-translator design doc for the full specification.
//
// The core idea (DESIGN.md §3): instance identity travels, host topology is
// re-derived. Translate is mostly a filter, not a hostname-rewriting
// engine — most fields are either forced to a clone-specific value
// (§3's always-explicit modes) or dropped outright and left for
// lib/installrunner.sh's own fresh-install machinery (os.Hostname,
// findFreePort, openssl rand) to self-derive.
package clonetopology

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bmjdotnet/teamster/internal/teamsteryaml"
)

// SourceConfig is the source instance's teamster.yaml — the same schema
// teamster-install writes, so Translate reads the real type instead of a
// hand-duplicated, drift-prone copy (WP2 §8).
type SourceConfig = teamsteryaml.Config

// DefaultEnvLabel is applied when TargetSpec.EnvLabel is empty. Shipping
// the source's own "production" env label on a disposable clone would make
// its OTEL data indistinguishable from the real hub's in Grafana.
const DefaultEnvLabel = "clone"

// TargetSpec is what the clone tool already knows before translation runs —
// resolved by earlier steps (CLI parsing, WP1's ref resolution), never
// derived from SourceConfig.
type TargetSpec struct {
	Basedir  string // "" = let installrunner.sh default to ~/teamster
	EnvLabel string // "" = DefaultEnvLabel ("clone")
}

// Translate produces the exact lib/installrunner.sh argv (excluding the
// binary path itself) for installing a clone. It never reads or writes
// files, never shells out, never inspects the network.
//
// SourceConfig is consulted for exactly one field (Env, informational only,
// with a safe fallback) — every mode/DSN/relay field is either forced or
// dropped regardless of what the source contains. This is deliberate, not
// an oversight (WP2 §6): a translator that mostly ignores its input is a
// translator that can't be broken by a weird one, and an all-zero-value
// SourceConfig{} must produce output identical to a fully-populated one
// (see the test table) — that invariant only holds because so little of
// the source is actually read.
func Translate(src SourceConfig, tgt TargetSpec) ([]string, error) {
	if err := validateTargetSpec(tgt, src.Clone.ForbiddenBasedirs); err != nil {
		return nil, err
	}

	envLabel := tgt.EnvLabel
	if envLabel == "" {
		envLabel = DefaultEnvLabel
	}

	// Always-explicit mode flags (WP2 §3) — omitting any of these either
	// silently skips URL wiring (hookd/otelcol/prometheus/grafana) or, for
	// store, skips provisioning a database entirely. R2 has already decided
	// every value: full managed stack, no --profile. Source config is
	// consulted for none of these five, regardless of what it contains.
	flags := []string{
		"--hookd-mode=systemd",
		"--store-mode=install",
		// R5b: clone-specific, always emitted for a clone — never the new
		// default for an ordinary --store-mode=install caller, which keeps
		// today's MariaDB behavior via lib/installrunner.sh's own default.
		"--store-engine=mysql-8.4",
		"--otelcol-mode=install",
		"--prometheus-mode=install",
		"--grafana-mode=install",
		"--env=" + envLabel,
	}

	// Everything else in the mapping table (WP2 §2) resolves to "omit the
	// flag" — ports/health/hostnames self-derive at install time
	// (os.Hostname, findFreePort), store.dsn is never copied (I3 — the
	// installer auto-generates a fresh one when --store-mode=install and no
	// --store-dsn is given), and relay/repl_push_remote are dropped
	// unconditionally regardless of source content (R1/I2 — no
	// conditional logic, not even a check).

	if tgt.Basedir != "" {
		flags = append(flags, "--basedir="+tgt.Basedir)
	}

	// A clone target has no prior install to merely stage-and-inspect —
	// this is the "replace global state" path, same as any fresh hub
	// install (WP2 §5's worked example).
	flags = append(flags, "--wire")

	return flags, nil
}

// validateTargetSpec is the one thing Translate actively validates rather
// than merely satisfies by omission (WP2 §6) — an operator or future
// caller could pass an explicit --basedir violating I5, and nothing else
// in the pipeline would catch that. forbiddenPrefixes comes from the
// source's own teamster.yaml (Clone.ForbiddenBasedirs) — which mounts are
// shared between a source and its clone targets is a deployment fact, not
// a constant this package can hardcode.
func validateTargetSpec(tgt TargetSpec, forbiddenPrefixes []string) error {
	if tgt.Basedir == "" {
		return nil
	}
	clean := filepath.Clean(tgt.Basedir)
	for _, prefix := range forbiddenPrefixes {
		p := filepath.Clean(prefix)
		if clean == p || strings.HasPrefix(clean, p+string(filepath.Separator)) {
			return fmt.Errorf("clonetopology: --basedir=%s is under %s, a forbidden prefix (shared storage the source can see); choose a basedir outside shared mounts", tgt.Basedir, prefix)
		}
	}
	return nil
}

package codexconfig

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// DoctorStatus is the outcome of the post-write validation gate.
type DoctorStatus int

const (
	// DoctorOK: the config.toml this installer just wrote still loads.
	DoctorOK DoctorStatus = iota
	// DoctorFailed: codex could not load the config.toml. Every codex
	// invocation on the host will fail until this is fixed — the caller
	// must roll back to the pre-write backup, never leave this in place.
	DoctorFailed
	// DoctorSkipped: the codex binary is not on PATH. Not a failure — a
	// host without Codex installed has nothing to gate. Callers on a
	// codex-less host should treat this the same as DoctorOK for control
	// flow (nothing to roll back), while logging that the gate didn't
	// actually run.
	DoctorSkipped
)

// DoctorResult is what RunDoctorGate found.
type DoctorResult struct {
	Status      DoctorStatus
	Summary     string
	Remediation string
}

// doctorReport mirrors the subset of `codex --strict-config doctor --json`'s
// schema this package reads. codex's own schemaVersion is 1 as of 0.137.0;
// unrecognized fields are ignored, so this tolerates the report growing new
// checks/fields without a code change — only checks["config.load"] is read.
type doctorReport struct {
	Checks map[string]struct {
		Status      string `json:"status"`
		Summary     string `json:"summary"`
		Remediation string `json:"remediation"`
	} `json:"checks"`
}

// RunDoctorGate runs `codex --strict-config doctor --json` and reports
// whether the config.toml just written is still loadable.
//
// It gates on checks["config.load"].status alone — deliberately never on the
// report's own top-level overallStatus. Verified live (2026-07-07, isolated
// CODEX_HOME, codex 0.137.0): overallStatus is "fail" any time an unrelated
// check fails — e.g. auth.credentials fails on any host that hasn't run
// `codex login` — even when config.load itself is "ok". Gating on
// overallStatus would make the installer roll back a perfectly good
// config.toml write because the *installing* host/user happens not to be
// logged into Codex, which has nothing to do with whether the write was
// well-formed.
//
// RunDoctorGate does not take a CODEX_HOME override — it inherits the
// caller's process environment, matching the doctor's own default resolution
// (unset CODEX_HOME means ~/.codex). Tests that must not touch a real
// ~/.codex set CODEX_HOME via t.Setenv before calling this.
func RunDoctorGate() (DoctorResult, error) {
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		return DoctorResult{Status: DoctorSkipped, Summary: "codex not found in PATH"}, nil
	}

	cmd := exec.Command(codexPath, "--strict-config", "doctor", "--json")
	out, runErr := cmd.Output()
	// `codex doctor` exits non-zero whenever ANY check fails, including ones
	// unrelated to config load (see the overallStatus note above) — that is
	// expected, not a real error, as long as stdout actually carries the
	// JSON report. Only treat this as a hard error if we got no output at
	// all (codex crashed, wrong flag, etc.).
	if len(out) == 0 && runErr != nil {
		return DoctorResult{}, fmt.Errorf("run %s --strict-config doctor --json: %w", codexPath, runErr)
	}

	var report doctorReport
	if jsonErr := json.Unmarshal(out, &report); jsonErr != nil {
		return DoctorResult{}, fmt.Errorf("parse codex doctor --json output: %w", jsonErr)
	}

	check, ok := report.Checks["config.load"]
	if !ok {
		return DoctorResult{}, fmt.Errorf(`codex doctor --json output missing checks["config.load"]`)
	}
	if check.Status != "ok" {
		return DoctorResult{Status: DoctorFailed, Summary: check.Summary, Remediation: check.Remediation}, nil
	}
	return DoctorResult{Status: DoctorOK, Summary: check.Summary}, nil
}

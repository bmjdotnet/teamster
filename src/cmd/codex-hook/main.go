// Command codex-hook is the Codex CLI hook client. It reads one hook event
// JSON payload from stdin — Codex's own hook wire format, near-identical to
// Claude Code's (session_id, hook_event_name, tool_name, tool_input,
// tool_response, transcript_path, cwd, plus Codex-specific fields like
// model/permission_mode/source/turn_id) — enriches it (internal/codexhook),
// and POSTs it to hookd's /event endpoint. Registered via
// codexconfig.TeamsterHookSpecs for SessionStart/PreToolUse/PostToolUse (see
// internal/codexconfig), with the matching trust-state blocks the installer
// writes alongside it — a hook with no trust entry is silently never
// invoked, not an error this binary can detect or report.
//
// Design choice (Go, not a Python sibling of skel/lib/hook/teamster.py):
// v1 Codex support is hub-local only (docs/... kit README §1 — Codex
// remotes are [later]), so there is no remote-host, no-Go-required
// constraint driving this choice the way there is for teamster.py. Every
// other hub-local Teamster binary (hookd, wms-mcp, activity-mcp, teamster,
// rollup, classify, token-scraper) is already Go, built by the same
// lib/installrunner.sh compile step — a Go binary fits that distribution
// model exactly, and lets this client import internal/redact directly
// instead of hand-porting its rules a second time (as teamster.py already
// must, and documents doing, for the Python-only remote case).
//
// Reliability contract (non-negotiable, matches cmd/teamster and
// teamster.py): exits 0 in every case, including a panic — hooks run
// synchronously, so a hung or crashing hook stalls every `codex` invocation
// on the host (the exact class of failure hooks.json's "mere presence adds
// exec latency" bug already demonstrates is possible from hook machinery
// alone). 2-second HTTP timeout. Every error is logged to
// ~/teamster/var/hook-errors.log and swallowed, never surfaced to Codex.
//
// This client deliberately never computes or writes token cost — that's
// codex-scraper's job (WP3), which reads the rollout JSONL, the only
// channel that carries real per-turn token counts — and never writes
// ~/.claude/current-session-id or any file the Claude-Code-specific
// WMS-attribution fallback reads (WP1's fail-safe requirement: a Codex
// process writing that file would silently steal attribution from a
// concurrent Claude Code session on the same host).
//
// Not implemented (open item, see this task's final report): echoing
// hookd's additionalContext back as hook stdout on PreToolUse, the way both
// existing clients do for Claude Code's focus-nudge. Whether Codex's hook
// protocol consumes a JSON stdout payload from a PreToolUse hook the same
// way Claude Code does is unverified for 0.137.0; writing an unexpected
// stdout shape risked interfering with `codex exec` rather than being
// silently ignored, and verifying it was outside this task's explicit scope.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/bmjdotnet/teamster/internal/codexhook"
	"github.com/bmjdotnet/teamster/internal/config"
)

func main() {
	defer func() {
		recover() //nolint:errcheck // panic safety: must never crash codex
		os.Exit(0)
	}()

	cfg, err := config.Load()
	if err != nil {
		os.Exit(0)
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		os.Exit(0)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		logError(cfg.Host, cfg.HookServerURL, "json_unmarshal", err.Error(), 0)
		os.Exit(0)
	}

	codexhook.Enrich(data, cfg.Host)

	if cfg.HookServerURL == "" {
		os.Exit(0)
	}

	body, err := json.Marshal(data)
	if err != nil {
		logError(cfg.Host, cfg.HookServerURL, "json_marshal", err.Error(), 0)
		os.Exit(0)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.HookServerURL, bytes.NewReader(body))
	if err != nil {
		os.Exit(0)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logError(cfg.Host, cfg.HookServerURL, "http_error", fmt.Sprintf("event=%v: %v", data["hook_event_name"], err), 0)
		os.Exit(0)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain so the connection can be reused; errors here are moot, we exit 0 regardless
	if resp.StatusCode >= 400 {
		logError(cfg.Host, cfg.HookServerURL, "http_status", fmt.Sprintf("event=%v", data["hook_event_name"]), resp.StatusCode)
	}
}

// logError appends a JSON line to ~/teamster/var/hook-errors.log, rotating
// at ~1MB. Mirrors teamster.py's _log_error exactly (same file, same shape)
// so both clients' failures show up in one place. Never raises/panics past
// this function — logging a hook client's own failure must never become a
// second failure.
func logError(host, url, errType, msg string, httpStatus int) {
	defer func() { recover() }() //nolint:errcheck

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	logDir := filepath.Join(home, "teamster", "var")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	logPath := filepath.Join(logDir, "hook-errors.log")
	if info, statErr := os.Stat(logPath); statErr == nil && info.Size() > 1_000_000 {
		_ = os.Rename(logPath, logPath+".old")
	}

	entry := map[string]interface{}{
		"ts":         time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"host":       host,
		"url":        url,
		"error_type": errType,
		"error_msg":  msg,
	}
	if httpStatus != 0 {
		entry["http_status"] = httpStatus
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(line) //nolint:errcheck // best-effort logging, nothing to do on failure
}

#!/usr/bin/env python3
"""Codex CLI hook client (Python). Reads a Codex hook event from stdin,
enriches with host identity, POSTs to the hub's hookd. Fire-and-forget.

Sibling of teamster.py (the Claude Code hook client) — imports its
redaction and error-logging helpers directly rather than re-implementing
them a third time (Go already has its own copy in internal/redact; this
file is Python's second copy of that logic, not a third). Deliberately
thinner than teamster.py's own main(): Codex v1 is solo-only (no Agent
Teams), so there is no dedup-file or session-mode-marker logic to port,
and no macOS agentName-from-transcript derivation (Codex v1 is hub-local
Linux only — see the kit's README, Codex remotes are a later feature).

Design note: an earlier Go prototype of this client (cmd/codex-hook) was
superseded by operator directive — client-side hook code should be Python
wherever possible, avoiding a Go toolchain requirement on any host that
runs Codex, the same reasoning that keeps teamster.py itself in Python
for remote installs. The payload-mapping logic below is a direct port of
that prototype; only the language changed.

Registered via codexconfig.TeamsterHookSpecs (src/internal/codexconfig)
for SessionStart/PreToolUse/PostToolUse, alongside the matching
[hooks.state] trust blocks the installer writes — a hook with no trust
entry is silently never invoked, not an error this script can detect.

This client must NEVER:
  - write ~/.claude/current-session-id or any file the Claude-Code-specific
    WMS-attribution fallback reads (WP1's fail-safe requirement: a Codex
    process writing that file would silently steal attribution from a
    concurrent Claude Code session on the same host).
  - compute or write token cost — that's codex-scraper's job (WP3), which
    reads the rollout JSONL, the only channel that carries real per-turn
    token counts.

Reliability contract (non-negotiable, matches teamster.py and the Go hook
client): exit 0 in every case, including an unexpected exception — hooks
run synchronously, so a hung or crashing hook stalls every `codex`
invocation on the host. 2-second HTTP timeout. Every error is logged to
~/teamster/var/hook-errors.log (the same file teamster.py logs to) and
swallowed, never surfaced to Codex.

Not implemented (open item, carried over from the Go prototype): echoing
hookd's additionalContext back as hook stdout on PreToolUse, the focus-nudge
parity Claude Code's hook gets. Whether Codex's hook protocol consumes a
JSON stdout payload from a PreToolUse hook the same way Claude Code does is
unverified for 0.137.0; writing an unexpected stdout shape risked
interfering with `codex exec` rather than being silently ignored, and
verifying it was outside this task's scope.
"""
from __future__ import annotations
import json
import os
import socket
import sys
import urllib.error
import urllib.request

# teamster.py lives alongside this file in the installed lib/hook/
# directory (both are copied there by the installer) — Python auto-adds a
# script's own directory to sys.path[0] when run directly, so this import
# resolves without any path manipulation, exactly like importing a
# same-directory module normally would.
from teamster import _log_error, _redact_event  # noqa: E402


def main():
    host = os.environ.get("TEAMSTER_HOST") or socket.gethostname().split(".")[0]
    url = os.environ.get("TEAMSTER_HOOK_SERVER_URL", "")

    try:
        raw = sys.stdin.read()
        if not raw.strip():
            return
        event = json.loads(raw)
    except Exception as e:
        _log_error(host, url, type(e).__name__, str(e))
        return

    event.setdefault("_host", host)

    # Codex sends "model" on every hook event already (unlike Claude Code,
    # whose payload carries no model field at all) — just copy it over,
    # no settings.json lookup needed the way teamster.py's getModel() does.
    model = event.get("model")
    if model and not event.get("_model"):
        event["_model"] = model

    # Mask any credential inlined in a shell command before it ever leaves
    # this host. Reuses teamster.py's exact redaction rules verbatim (same
    # function, same regexes) — see that module's docstring for the
    # byte-parity-with-Go (internal/redact) rationale this shares.
    _redact_event(event)

    if not url:
        return

    # Cap large fields hookd's buildRecord never persists — same limits as
    # teamster.py (hookd's body limit is 1MB; a big MCP tool_response can
    # otherwise get the whole POST rejected outright).
    for key in ("tool_response", "stop_response", "last_assistant_message"):
        v = event.get(key)
        if isinstance(v, str) and len(v) > 1024:
            event[key] = v[:1024]
    ti = event.get("tool_input")
    if isinstance(ti, str) and len(ti) > 32768:
        event["tool_input"] = ti[:32768]

    body = json.dumps(event).encode("utf-8")
    req = urllib.request.Request(url, data=body, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=2)
    except urllib.error.HTTPError as e:
        _log_error(host, url, "HTTPError",
                   f"event={event.get('hook_event_name')!r}: {e}",
                   http_status=e.code)
    except Exception as e:
        _log_error(host, url, type(e).__name__,
                   f"event={event.get('hook_event_name')!r}: {e}")
    # never block codex on hub unavailability


if __name__ == "__main__":
    # Belt-and-suspenders on top of main()'s own internal try/except blocks:
    # an unexpected exception anywhere in this script must still exit 0 —
    # hooks run synchronously in `codex`'s own process tree, so a nonzero
    # exit or an uncaught traceback is exactly the failure mode the
    # reliability contract exists to prevent.
    try:
        main()
    except Exception:
        pass
    sys.exit(0)

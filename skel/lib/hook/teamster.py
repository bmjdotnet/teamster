#!/usr/bin/env python3
"""Teamster hook client (Python). Reads a Claude Code hook event from stdin,
enriches with host identity, POSTs to the hub's hookd. Fire-and-forget."""
from __future__ import annotations
import json, os, re, socket, subprocess, sys, urllib.request, urllib.error
from datetime import datetime, timezone

# Bounded transcript scan for agentName — stop after this many bytes to keep
# each hook invocation fast. agentName appears near the top of child transcripts.
_TRANSCRIPT_SCAN_LIMIT = 256 * 1024  # 256 KB

_LOG_MAX_BYTES = 1_000_000  # rotate at ~1 MB

_REDACTED = "<redacted>"

# Credential shapes masked in shell commands before the event leaves this host.
# Byte-parity with src/internal/redact (the hub choke point) so a secret never
# even reaches the wire. Each pattern keeps the surrounding flag/key/structure
# and replaces only the secret value. Focused on DB/DSN/env/HTTP-auth vectors,
# not a universal scanner. Unquoted value classes stop at shell separators
# (;, &, |, whitespace) so a trailing separator survives redaction. Order
# matters: specific shapes precede the generic net.
_REDACT_RULES = [
    # scheme://user:SECRET@host — runs before the -u rule so a DSN passed as
    # -u <dsn> is masked here, not mistaken for basic auth.
    (re.compile(r"([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/@]+:)[^\s@/]+(@)"), r"\1" + _REDACTED + r"\2"),
    # --password=SECRET / --password SECRET (unambiguous; runs for every tool)
    (re.compile(r"(--?password)(=|\s+)('[^']*'|\"[^\"]*\"|[^\s;&|]+)"), r"\1\2" + _REDACTED),
    # MYSQL_PWD=… PGPASSWORD=… *_TOKEN= *_SECRET= *API_KEY= …
    (re.compile(r"(?i)(^|\s)([A-Z][A-Z0-9_]*(?:PASSWORD|PASSWD|_PWD|_TOKEN|_SECRET|API_?KEY)=)('[^']*'|\"[^\"]*\"|[^\s;&|]+)"), r"\1\2" + _REDACTED),
    # curl -u user:SECRET is handled by _redact_user_auth (match-then-decide).
    # Authorization: Bearer/Basic/… <token>
    (re.compile(r"(?i)(authorization:\s*(?:bearer|basic|token|digest)\s+)[^\s\"']+"), r"\1" + _REDACTED),
    # generic key=value secret params (value stops at shell separators ; & |)
    (re.compile(r"(?i)\b(password|passwd|pwd|token|api[_-]?key|secret|access[_-]?key)=('[^']*'|\"[^\"]*\"|[^\s;&|'\"]+)"), r"\1=" + _REDACTED),
]

# curl -u/--user basic auth. A regex alone can't both fully mask a password of
# any shape AND skip a credentialed DSN (no lookahead parity with RE2), so the
# value is inspected in code. Mirrors redactUserAuth in src/internal/redact.
_USER_AUTH = re.compile(r"(-u|--user)(=|\s+)([^\s;&|]+)")


def _redact_user_auth(s):
    """Mask the password in a curl -u/--user argument, regardless of its leading
    character. Match-then-decide on the value:
      - already contains the placeholder -> a credentialed DSN the userinfo rule
        masked; leave unchanged so it is not double-mangled.
      - has a ":" -> basic auth user:pass; mask everything after the FIRST ":".
      - has no ":" -> bare username; not a secret; unchanged."""
    def repl(m):
        flag, sep, val = m.group(1), m.group(2), m.group(3)
        if _REDACTED in val:
            return m.group(0)
        i = val.find(":")
        if i < 0:
            return m.group(0)
        return flag + sep + val[:i + 1] + _REDACTED
    return _USER_AUTH.sub(repl, s)

# Attached mysql password -p<x> is too broad to run unconditionally (-p is also
# ssh's port, docker's port-map, etc.), so it applies only to mysql-family
# invocations. A bare -p (interactive prompt) is left alone. Mirrors mysqlRules.
_MYSQL_FAMILY = re.compile(r"\b(?:mysql|mariadb|mysqldump|mysqladmin|mysqlshow|mysqlcheck)\b")
_REDACT_MYSQL_RULES = [
    (re.compile(r"(^|\s)(-p)'[^']*'"), r"\1\2" + _REDACTED),
    (re.compile(r'(^|\s)(-p)"[^"]*"'), r"\1\2" + _REDACTED),
    (re.compile(r"(^|\s)(-p)[^\s'\"]+"), r"\1\2" + _REDACTED),
]


def _redact(s):
    """Mask credential shapes in a shell-command string. Never raises."""
    try:
        for pat, repl in _REDACT_RULES:
            s = pat.sub(repl, s)
        s = _redact_user_auth(s)
        if _MYSQL_FAMILY.search(s):
            for pat, repl in _REDACT_MYSQL_RULES:
                s = pat.sub(repl, s)
        return s
    except Exception:
        return s


def _redact_event(event):
    """Redact the command field of a tool event in place. Never raises — a
    redaction failure must not block Claude Code or drop the event."""
    try:
        ti = event.get("tool_input")
        if isinstance(ti, dict):
            cmd = ti.get("command")
            if isinstance(cmd, str) and cmd:
                ti["command"] = _redact(cmd)
        elif isinstance(ti, str) and ti:
            event["tool_input"] = _redact(ti)
    except Exception:
        pass


def _read_version() -> str:
    """Read the install's stamped version. install.sh writes ~/teamster/VERSION;
    a source checkout has VERSION at the repo root. Falls back to 'dev'."""
    here = os.path.dirname(os.path.abspath(__file__))
    for path in (
        os.path.join(os.path.expanduser("~"), "teamster", "VERSION"),
        os.path.join(here, "..", "..", "..", "VERSION"),
    ):
        try:
            with open(path) as f:
                v = f.read().strip()
                if v:
                    return v
        except OSError:
            continue
    return "dev"


__version__ = _read_version()


def _dump_raw_debug(raw: str, event: dict) -> None:
    """Append a capture record to ~/teamster/var/raw-hook-debug.jsonl.

    Activated by setting TEAMSTER_DEBUG_RAW to any non-empty value, e.g.:
        TEAMSTER_DEBUG_RAW=1 claude
    The file captures the verbatim stdin string plus a structured envelope
    with identity fields — useful for diagnosing missing agent_type/agent_id
    on remote hosts. Rotates at ~5 MB like hook-errors.log. Never raises.
    """
    try:
        log_dir = os.path.join(os.path.expanduser("~"), "teamster", "var")
        os.makedirs(log_dir, exist_ok=True)
        log_path = os.path.join(log_dir, "raw-hook-debug.jsonl")
        try:
            if os.path.getsize(log_path) > 5_000_000:
                os.replace(log_path, log_path + ".old")
        except OSError:
            pass
        record = {
            "captured_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "hook_event_name": event.get("hook_event_name"),
            "tool_name": event.get("tool_name"),
            "session_id": event.get("session_id"),
            "top_level_keys": sorted(event.keys()),
            "raw_stdin": raw,
        }
        with open(log_path, "a") as f:
            f.write(json.dumps(record) + "\n")
    except Exception:
        pass  # debug logging must never block Claude Code


def _log_error(host: str, url: str, error_type: str, error_msg: str, http_status: int = 0) -> None:
    """Append a JSON line to ~/teamster/var/hook-errors.log. Never raises."""
    try:
        log_dir = os.path.join(os.path.expanduser("~"), "teamster", "var")
        os.makedirs(log_dir, exist_ok=True)
        log_path = os.path.join(log_dir, "hook-errors.log")
        # rotate: rename to .old when file exceeds _LOG_MAX_BYTES
        try:
            if os.path.getsize(log_path) > _LOG_MAX_BYTES:
                os.replace(log_path, log_path + ".old")
        except OSError:
            pass
        entry = {
            "ts": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "host": host,
            "url": url,
            "error_type": error_type,
            "error_msg": error_msg,
        }
        if http_status:
            entry["http_status"] = http_status
        with open(log_path, "a") as f:
            f.write(json.dumps(entry) + "\n")
    except Exception:
        pass  # logging failure must never block Claude Code


def _agent_name_from_transcript(path):
    """Derive agentName from a Claude Code transcript file.

    On macOS, dispatched teammates run as top-level sessions and carry no
    agent_type in hook payloads. Their transcript's early lines contain a
    top-level "agentName" field (e.g. {"agentName":"PizzaDude",...}) that
    identifies the teammate. Lead sessions have no agentName.

    Scans at most _TRANSCRIPT_SCAN_LIMIT bytes, stopping at the first line
    containing a non-empty agentName. No caching — this client is fork-per-event
    so each invocation is a fresh process.

    Returns the agentName string, or "" if not found or on any error.
    Never raises.
    """
    result = ""
    try:
        bytes_read = 0
        with open(path, "rb") as f:
            for raw_line in f:
                bytes_read += len(raw_line)
                if bytes_read > _TRANSCRIPT_SCAN_LIMIT:
                    break
                if b"agentName" not in raw_line:
                    continue
                try:
                    rec = json.loads(raw_line)
                    name = rec.get("agentName", "")
                    if name and isinstance(name, str):
                        result = name
                        break
                except (json.JSONDecodeError, TypeError, AttributeError):
                    continue
    except Exception:
        pass  # unreadable transcript → leave event unchanged
    return result


def _parent_claude_args() -> dict:
    """Read --parent-session-id and --agent-name from the parent process's
    command line (macOS: separate-session teammates launched with these flags).
    Returns a dict with keys present only when found. Best-effort, never raises."""
    try:
        ppid = os.getppid()
        if sys.platform == "darwin":
            out = subprocess.check_output(
                ["ps", "-p", str(ppid), "-o", "args="],
                timeout=1, stderr=subprocess.DEVNULL,
            ).decode("utf-8", errors="replace")
        else:
            try:
                with open(f"/proc/{ppid}/cmdline", "rb") as f:
                    out = f.read().decode("utf-8", errors="replace").replace("\0", " ")
            except OSError:
                return {}
        result = {}
        m = re.search(r"--parent-session-id\s+(\S+)", out)
        if m:
            result["parent_session_id"] = m.group(1)
        m = re.search(r"--agent-name\s+(\S+)", out)
        if m:
            result["agent_name"] = m.group(1)
        m = re.search(r"--team-name\s+(\S+)", out)
        if m:
            result["team_name"] = m.group(1)
        return result
    except Exception:
        return {}


def main():
    host = os.environ.get("TEAMSTER_HOST", socket.gethostname().split('.')[0])
    url = os.environ.get("TEAMSTER_HOOK_SERVER_URL", "")

    try:
        raw = sys.stdin.read()
        if not raw.strip():
            return
        event = json.loads(raw)
    except Exception as e:
        _log_error(host, url, type(e).__name__, str(e))
        return  # never block Claude on parse failure

    # Opt-in raw capture: dump verbatim stdin + identity envelope before any
    # redaction or field-capping. Set TEAMSTER_DEBUG_RAW=1 to activate.
    if os.environ.get("TEAMSTER_DEBUG_RAW"):
        _dump_raw_debug(raw, event)

    event.setdefault("_host", host)

    # On macOS, teammate sessions carry no agent_type in hook payloads.
    # Derive it from the transcript's agentName field so hookd can resolve
    # the teammate identity (EnrichRecord sets _agent_name = "@"+agent_type).
    # Only runs when agent_type is absent/empty and transcript_path is present.
    if not event.get("agent_type"):
        transcript_path = event.get("transcript_path", "")
        if transcript_path:
            name = _agent_name_from_transcript(transcript_path)
            if name:
                event["agent_type"] = name

    # macOS separate-session teammates: read --parent-session-id,
    # --agent-name, and --team-name from the parent Claude Code process's
    # command line. These flags are how CC launches each teammate, but they
    # don't appear in hook payloads — inject them so hookd can establish
    # parent linkage and use the real name (not generic "@Explore").
    parent_args = _parent_claude_args()
    if parent_args.get("parent_session_id"):
        event.setdefault("_parent_session_id", parent_args["parent_session_id"])
    if parent_args.get("agent_name"):
        event["_agent_name"] = "@" + parent_args["agent_name"]
    if parent_args.get("team_name"):
        event.setdefault("_team_name", parent_args["team_name"])

    # Mask any credential inlined in a Bash command before it leaves this host.
    # Defense in depth: the hub redacts again at ingest, but scrubbing here means
    # the secret never even touches the wire from a remote client.
    _redact_event(event)

    if not url:
        return

    # Cap large fields that hookd's buildRecord never persists. Without this,
    # PostToolUse events with big tool_response exceed hookd's 1MB body limit.
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
        resp = urllib.request.urlopen(req, timeout=2)
        resp_body = resp.read(4096)
        hook_event = event.get("hook_event_name")
        if resp_body and hook_event in ("PreToolUse", "UserPromptSubmit"):
            try:
                resp_data = json.loads(resp_body)
                ctx = resp_data.get("additionalContext", "")
                if ctx:
                    out = {"hookSpecificOutput": {
                        "hookEventName": hook_event,
                        "additionalContext": ctx,
                    }}
                    sys.stdout.write(json.dumps(out))
            except (json.JSONDecodeError, TypeError):
                pass
    except urllib.error.HTTPError as e:
        _log_error(host, url, "HTTPError",
                   f"event={event.get('hook_event_name')!r}: {e}",
                   http_status=e.code)
    except Exception as e:
        _log_error(host, url, type(e).__name__,
                   f"event={event.get('hook_event_name')!r}: {e}")
    # never block Claude on hub unavailability


if __name__ == "__main__":
    main()

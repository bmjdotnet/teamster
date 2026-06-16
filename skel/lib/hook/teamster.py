#!/usr/bin/env python3
"""Teamster hook client (Python). Reads a Claude Code hook event from stdin,
enriches with host identity, POSTs to the hub's hookd. Fire-and-forget."""
import json, os, re, socket, sys, urllib.request, urllib.error
from datetime import datetime, timezone

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

    event.setdefault("_host", host)

    if event.get("tool_name") == "TeamCreate":
        ti = event.get("tool_input") or {}
        if isinstance(ti, dict):
            name = ti.get("team_name", "")
            if name:
                event["_team"] = name

    # Mask any credential inlined in a Bash command before it leaves this host.
    # Defense in depth: the hub redacts again at ingest, but scrubbing here means
    # the secret never even touches the wire from a remote client.
    _redact_event(event)

    if not url:
        return

    body = json.dumps(event).encode("utf-8")
    req = urllib.request.Request(url, data=body, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        resp = urllib.request.urlopen(req, timeout=2)
        resp_body = resp.read(4096)
        if resp_body and event.get("hook_event_name") == "PreToolUse":
            try:
                resp_data = json.loads(resp_body)
                ctx = resp_data.get("additionalContext", "")
                if ctx:
                    out = {"hookSpecificOutput": {
                        "hookEventName": "PreToolUse",
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

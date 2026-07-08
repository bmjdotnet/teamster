#!/usr/bin/env python3
"""codex-scraper: tails Codex CLI rollout JSONL files
($CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl and
$CODEX_HOME/archived_sessions/) and is the sole writer of Codex cost/ledger
data on a Go-less remote: it POSTs per-token_count telemetry rows to hookd's
/telemetry endpoint (same wire contract token-scraper.py uses for Claude
Code) and upserts the Codex sessions row via hookd's /session endpoint (the
remote has no direct store connection, unlike the hub-local Go codex-scraper
this file ports — see src/cmd/codex-scraper/{rollout,scraper}.go, the
reference implementation and this port's spec).

Oneshot, not a daemon: driven by cron (Linux, every 10 min) or a launchd
agent (macOS) — mirrors the hub-local Go binary's systemd-timer design, not
token-scraper.py's --daemon poll loop. One poll() call per invocation, then
exit 0 unconditionally.

Single-ledger-writer invariant (LESSONS.md §6): this tailer is the ONLY
component that writes Codex cost. codex-hook.py (feed-only) and OTEL
(dashboards-only) must never gain a ledger-writing path — don't add one.

Scope boundaries (matches the Go v1 scope): solo-only (no Codex Agent
Teams), --ephemeral exec runs are invisible (Codex never persists their
rollout file), and mcp_tool_call_end / response_item(function_call) events
are parsed for schema fidelity (Ok/Err discrimination) but not shipped as
telemetry — no wire contract exists for per-tool-call Codex activity yet.

Reliability contract (non-negotiable, shipped-to-remote code, matches
codex-hook.py's posture): pure stdlib only, exit 0 in every case including
an unexpected exception, HTTP timeouts capped at 2s, never block, every
error logged to ~/teamster/var/codex-scraper-errors.log and swallowed.
"""
from __future__ import annotations

import argparse
import getpass
import json
import logging
import os
import socket
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone

_LOG_MAX_BYTES = 1_000_000  # rotate at ~1 MB, matches token-scraper.py
_HTTP_TIMEOUT = 2  # seconds — reliability contract cap for remote-shipped code


def _read_version() -> str:
    """Read ~/teamster/VERSION; fall back to 'dev'. Mirrors token-scraper.py."""
    candidates = [
        os.path.join(os.path.expanduser("~"), "teamster", "VERSION"),
        os.path.join(os.path.dirname(os.path.abspath(__file__)),
                     "..", "..", "..", "VERSION"),
    ]
    for path in candidates:
        try:
            with open(path) as f:
                v = f.read().strip()
                if v:
                    return v
        except OSError:
            continue
    return "dev"


__version__ = _read_version()

# ---------------------------------------------------------------------------
# Pricing table — OpenAI/Codex model families only.
#
# SYNC ANCHOR: mirrors the Codex-model entries of internal/pricing/pricing.go
# (Known map). The pricing-externalization effort (the pricing kit) has NOT
# landed store-backed rates as of this port — this is a THIRD hand-maintained
# copy of the same numbers (Go pricing.go is the first, token-scraper.py's
# Claude-side _KNOWN is the second). Do not let this drift silently; when the
# pricing kit ships,
# replace this table with its store-backed lookup and delete the duplicate.
#
# ModelPricing: (input, output, cache_read, cache_write) — USD per token.
# OpenAI publishes no cache-write tier for any of these — cache_write is
# always 0 (unlike Anthropic's four-bucket split).
# ---------------------------------------------------------------------------
_KNOWN: dict = {
    "gpt-5.5":      (0.000005,   0.00003,   0.0000005,   0.0),
    "gpt-5.5-pro":  (0.00003,    0.00018,   0.0,         0.0),  # no cached-input tier published
    "gpt-5.4":      (0.0000025,  0.000015,  0.00000025,  0.0),
    "gpt-5.4-mini": (0.00000075, 0.0000045, 0.000000075, 0.0),
    "gpt-5.4-nano": (0.0000002,  0.00000125, 0.00000002, 0.0),
    "gpt-5.3-codex": (0.00000175, 0.000014, 0.000000175, 0.0),
    # gpt-5.1-codex/gpt-5.2-codex/o3/o4-mini are real selectable Codex model
    # IDs with no current published rate (superseded / not a standalone
    # priced row) — deliberately NOT given a fabricated entry; they fall
    # through to the loud-warning + $0 path below, matching pricing.go.
}


def _price_for(model: str):
    """Resolve (input, output, cache_read, cache_write) USD-per-token rates.

    Lookup order matches pricing.go's priceFor exactly:
    1. exact match:
    2. longest _KNOWN key that is a prefix of model (dated/suffixed variants).
    3. no same-class fallback exists for OpenAI model families (unlike the
       Claude side's opus/sonnet/haiku/fable classes) — an unmatched model
       logs a loud warning and prices at $0 rather than guessing.
    """
    if model in _KNOWN:
        return _KNOWN[model]

    best_key = ""
    best_rates = None
    for key, rates in _KNOWN.items():
        if model.startswith(key) and len(key) > len(best_key):
            best_key = key
            best_rates = rates
    if best_rates is not None:
        return best_rates

    logging.warning(
        "no pricing entry for model; costing at $0 — add rates to _KNOWN "
        "model=%s", model)
    return None


def compute_cost(model: str, input_tokens: int, output_tokens: int,
                  cache_read_tokens: int, cache_write_tokens: int) -> float:
    """Return total USD cost for the given token counts. $0 for unknown models."""
    rates = _price_for(model)
    if rates is None:
        return 0.0
    inp, out, cr, cw = rates
    return (input_tokens * inp + output_tokens * out
            + cache_read_tokens * cr + cache_write_tokens * cw)


# ---------------------------------------------------------------------------
# Error logging (mirrors token-scraper.py's _log_error pattern; a dedicated
# file, not token-scraper's scraper-errors.log, so the two tailers' error
# streams don't interleave ambiguously on a host that runs both).
# ---------------------------------------------------------------------------

def _log_error(msg: str, **extra) -> None:
    """Append a JSON line to ~/teamster/var/codex-scraper-errors.log. Never raises."""
    try:
        log_dir = os.path.join(os.path.expanduser("~"), "teamster", "var")
        os.makedirs(log_dir, exist_ok=True)
        log_path = os.path.join(log_dir, "codex-scraper-errors.log")
        try:
            if os.path.getsize(log_path) > _LOG_MAX_BYTES:
                os.replace(log_path, log_path + ".old")
        except OSError:
            pass
        entry = {
            "ts": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "msg": msg,
        }
        entry.update(extra)
        with open(log_path, "a") as f:
            f.write(json.dumps(entry) + "\n")
    except Exception:
        pass  # logging must never crash the scraper


# ---------------------------------------------------------------------------
# mcp_tool_call_end result discrimination
# ---------------------------------------------------------------------------

def _mcp_call_ok(raw):
    """Parse a mcp_tool_call_end event's `result` field and report whether it
    was a success (Ok) as opposed to a cancellation/denial/failure (Err — a
    distinct shape, not a separate event type). Returns (ok, matched); both
    False when raw is empty/None or matches neither shape.

    Accepts either a raw JSON string (mirrors the Go mcpCallOK's
    json.RawMessage input, and this port's own tests) or an already-decoded
    dict (the shape process_line sees after json.loads-ing a whole rollout
    line) — never raises.
    """
    if raw is None or raw == "":
        return False, False
    if isinstance(raw, (str, bytes)):
        try:
            raw = json.loads(raw)
        except (ValueError, TypeError):
            return False, False
    if not isinstance(raw, dict):
        return False, False
    if raw.get("Err") is not None:
        return False, True
    if raw.get("Ok") is not None:
        return True, True
    return False, False


# ---------------------------------------------------------------------------
# Cursor
# ---------------------------------------------------------------------------

def _new_cursor() -> dict:
    """One entry per rollout file: read progress + cached session identity.

    Persisted so a scraper restart (or a file relocated by `codex
    archive`/`unarchive`, which moves the file to a new path without
    changing its content) never loses track of what's already been ledgered.

    session_id vs thread_id (subagent fix — LESSONS.md §2, THE single point
    of child->parent identity resolution for Codex subagents): a thread_spawn
    subagent's rollout file has session_meta.session_id == the PARENT
    thread's id (session_meta.id is the file's own thread id). session_id
    here is the parent-resolved id (falling back to id on 0.137.0 files that
    lack session_id entirely) — it's what ledger rows and the sessions
    upsert use, so subagent spend books under the SAME session as the
    parent's own focus intervals. thread_id is always the file's own id,
    used ONLY for message_id derivation so a parent file's seq 1..N and a
    subagent file's seq 1..M can never collide onto the same
    codex:<id>:<seq> key (which session_id-keying would cause, since
    multiple subagent files can share one session_id).

    seq is the count of token_count events already ledgered from this file.
    Codex's token_count events carry no content-derived unique id, so the
    tailer manufactures one from (thread_id, seq) — stable because rollout
    files are strictly append-only: re-scanning from offset 0 (e.g. after an
    archive-triggered path change loses the cursor) reproduces the identical
    sequence, so the derived message_id matches what's already ledgered and
    hookd's uq_message-keyed upsert makes the re-insert a harmless no-op.
    """
    return {
        "offset": 0,
        "seq": 0,
        "session_id": "",
        "thread_id": "",
        "agent_name": "",  # "@"+agent_role for a subagent thread, else ""
        "cwd": "",
        "originator": "",
        "cli_version": "",
        "model": "",  # last-known model, updated per turn_context
    }


class _PostFailed(Exception):
    """Raised when a telemetry POST fails, so the read loop stops here and
    does not advance the cursor past the unsent row (retried next poll)."""


# ---------------------------------------------------------------------------
# Scraper
# ---------------------------------------------------------------------------

class Scraper:
    def __init__(self, *, telemetry_url: str, session_url: str, host: str,
                 username: str, roots, cursor_path: str, dry_run: bool):
        self.telemetry_url = telemetry_url
        self.session_url = session_url
        self.host = host
        self.username = username
        self.roots = list(roots)
        self.cursor_path = cursor_path
        self.dry_run = dry_run
        self.cursors: dict = {}  # path -> cursor dict (see _new_cursor)

    # ------------------------------------------------------------------
    # Cursor persistence
    # ------------------------------------------------------------------

    def load_cursors(self) -> None:
        try:
            with open(self.cursor_path) as f:
                data = json.load(f)
            if isinstance(data, dict):
                self.cursors = data
        except FileNotFoundError:
            pass  # normal first run
        except Exception as exc:
            logging.warning("loading cursors failed, starting fresh: %s", exc)

    def save_cursors(self) -> None:
        """Atomic save: write to temp file then os.replace()."""
        tmp = self.cursor_path + ".tmp"
        try:
            os.makedirs(os.path.dirname(self.cursor_path), exist_ok=True)
            with open(tmp, "w") as f:
                json.dump(self.cursors, f)
            os.replace(tmp, self.cursor_path)
        except Exception as exc:
            _log_error("save_cursors failed", error=str(exc))
            logging.error("save_cursors failed: %s", exc)

    # ------------------------------------------------------------------
    # Discovery
    # ------------------------------------------------------------------

    def discover_files(self):
        """Walk every configured root for *.jsonl files. Missing roots are
        skipped silently (a fresh CODEX_HOME may not have archived_sessions/
        yet — os.walk on a non-existent directory simply yields nothing, no
        exception, matching Go's WalkDir-error-swallowing discoverFiles).
        Sorted for deterministic processing order across runs."""
        files = []
        for root in self.roots:
            for dirpath, _dirnames, filenames in os.walk(root):
                for name in filenames:
                    if name.endswith(".jsonl"):
                        files.append(os.path.join(dirpath, name))
        files.sort()
        return files

    # ------------------------------------------------------------------
    # Poll
    # ------------------------------------------------------------------

    def poll(self) -> None:
        """Process new bytes in every discovered rollout file once, then
        persist cursor state. Oneshot — called exactly once per invocation
        (cron/launchd timer), matching the Go binary's systemd-timer design."""
        for path in self.discover_files():
            try:
                self.process_file(path)
            except Exception as exc:
                logging.error("process file error path=%s: %s", path, exc)
                _log_error("process file error", path=path, error=str(exc))

        if not self.dry_run:
            self.save_cursors()

    def process_file(self, path: str) -> None:
        """Ingest new bytes from one rollout JSONL file: parse each complete
        line, update the cursor's cached session identity, emit ledger rows
        for token_count events, and upsert the sessions row once per call if
        anything session-identifying changed."""
        try:
            st = os.stat(path)
        except OSError:
            return  # file disappeared between discovery and stat

        cursor = self.cursors.setdefault(path, _new_cursor())

        # Reset if the file was truncated/rotated (defensive; rollout files
        # are normally append-only, but history.max_bytes retention is a
        # documented Codex-side event whose exact on-disk effect isn't
        # guaranteed forever).
        if st.st_size < cursor["offset"]:
            cursor.clear()
            cursor.update(_new_cursor())

        if st.st_size == cursor["offset"]:
            return

        try:
            f = open(path, "rb")
        except OSError:
            return
        try:
            self._read_file(f, path, cursor)
        finally:
            f.close()

    def _read_file(self, f, path: str, cursor: dict) -> None:
        """Inner read loop — mirrors the Go bufio.Scanner loop in
        processFile(). new_offset tracks the byte offset of the end of the
        last *successfully flushed* line; on a POST failure it stops without
        advancing past the failed line (retried from there next poll)."""
        if cursor["offset"] > 0:
            f.seek(cursor["offset"])

        session_changed = False
        pos = cursor["offset"]
        new_offset = cursor["offset"]

        while True:
            raw = f.readline()
            if not raw:
                break  # true EOF
            if not raw.endswith(b"\n"):
                # Trailing bytes with no newline yet: Codex may still be
                # writing this line. Leave it uncommitted; the next poll
                # re-reads it once the write completes.
                break

            line_len = len(raw)
            trimmed = raw.rstrip(b"\r\n")

            if trimmed.strip():
                try:
                    if self._process_line(trimmed, cursor, path):
                        session_changed = True
                except _PostFailed:
                    # Stop here; do not advance past this unsent row. It
                    # will be retried from this offset on the next poll.
                    cursor["offset"] = new_offset
                    return
                except Exception as exc:
                    logging.debug("skipping unparseable rollout line path=%s: %s",
                                  path, exc)

            pos += line_len
            new_offset = pos

        cursor["offset"] = new_offset

        if session_changed and cursor["session_id"]:
            self._upsert_codex_session(cursor)

    def _process_line(self, raw: bytes, cursor: dict, path: str) -> bool:
        """Parse one rollout JSONL line and update cursor state. Returns True
        when the cursor's cached session identity changed (caller upserts
        the sessions row once per file-scan, not per line). May raise
        _PostFailed (propagated from _emit_ledger_row) or any parse
        exception (caller logs+skips, still advancing past the bad line)."""
        env = json.loads(raw)
        if not isinstance(env, dict):
            raise ValueError("envelope is not a JSON object")

        typ = env.get("type", "")
        payload = env.get("payload") or {}
        if not isinstance(payload, dict):
            payload = {}

        if typ == "session_meta":
            pid = payload.get("id", "")
            if pid:
                cursor["thread_id"] = pid
                # session_id is the parent thread's id for a subagent file
                # (or this file's own id for a top-level file on 0.142.x,
                # where session_id == id); fall back to id on 0.137.0 files,
                # which have no session_id field at all.
                sid = payload.get("session_id", "")
                cursor["session_id"] = sid if sid else pid
                role = payload.get("agent_role", "")
                if role:
                    cursor["agent_name"] = "@" + role
                cursor["cwd"] = payload.get("cwd", "")
                cursor["originator"] = payload.get("originator", "")
                cursor["cli_version"] = payload.get("cli_version", "")
                return True
            return False

        if typ == "turn_context":
            model = payload.get("model", "")
            # Upstream bug (openai/codex#20981): some internal Codex
            # sub-tasks (e.g. an auto-review pass) report the literal model
            # string "codex-auto-review" instead of the real underlying
            # model. Ignore the sentinel and keep whichever real model was
            # last seen.
            if model and model != "codex-auto-review" and model != cursor["model"]:
                cursor["model"] = model
                return True
            return False

        if typ == "event_msg":
            etype = payload.get("type", "")
            if etype == "token_count":
                info = payload.get("info")
                if not info:
                    return False
                last_usage = info.get("last_token_usage") or {}
                self._emit_ledger_row(env.get("timestamp", ""), last_usage, cursor)
                return False
            if etype == "mcp_tool_call_end":
                # Branch on result.Ok vs result.Err (a cancelled/denied call
                # is an Err, same event type as a success). No wire contract
                # exists yet to ship this as telemetry — logged only,
                # documented v1 scope boundary (matches the Go scraper).
                ok, matched = _mcp_call_ok(payload.get("result"))
                invocation = payload.get("invocation")
                if matched and invocation:
                    logging.debug("mcp tool call path=%s server=%s tool=%s ok=%s",
                                  path, invocation.get("server"),
                                  invocation.get("tool"), ok)
            return False

        # response_item (function_call/function_call_output, e.g.
        # exec_command): schema understood, not consumed in v1 (no ledger or
        # session-identity signal lives here).
        return False

    def _emit_ledger_row(self, timestamp: str, last_usage: dict, cursor: dict) -> None:
        """Build and POST one telemetry row from a token_count event's
        last_token_usage. Ledger derivation rule (binding, LESSONS.md §3):
        use last_token_usage ONLY — never total_token_usage (cumulative
        across the whole session; summing it double-counts).

        Token bucket derivation: cached_input_tokens is a SUBSET of
        input_tokens, and reasoning_output_tokens is a SUBSET of
        output_tokens (both informational breakdowns, not additional
        tokens) — total_tokens == input_tokens + output_tokens always. So:
          - uncached input (billed at the full input rate)
              = input_tokens - cached_input_tokens
          - cache-read tokens (billed at the cheaper cache-read rate)
              = cached_input_tokens
          - output tokens, as-is (reasoning_output_tokens is already inside
            this number; OpenAI bills it at the output rate by inclusion,
            not by adding it again)
          - cache-write is always 0 (no Codex/OpenAI equivalent)
        This differs from Claude Code's transcript semantics, where
        input_tokens already excludes cache reads — do not copy that
        assumption here.
        """
        seq = cursor["seq"]
        cursor["seq"] = seq + 1

        # Keyed by thread_id (this file's own id), never session_id: see
        # _new_cursor's doc for why session_id-keying would collide two
        # files' seq counters onto the same message_id.
        message_id = "codex:%s:%06d" % (cursor["thread_id"], seq)

        input_tokens = int(last_usage.get("input_tokens") or 0)
        cached_input_tokens = int(last_usage.get("cached_input_tokens") or 0)
        output_tokens = int(last_usage.get("output_tokens") or 0)
        reasoning_output_tokens = int(last_usage.get("reasoning_output_tokens") or 0)
        total_tokens = int(last_usage.get("total_tokens") or 0)

        if total_tokens != 0 and total_tokens != input_tokens + output_tokens:
            logging.warning(
                "codex-scraper: token_count arithmetic violated expected invariant "
                "(total_tokens != input_tokens + output_tokens) — upstream semantics "
                "may have drifted; derivation below may be wrong for this row "
                "session_id=%s input=%d output=%d total=%d",
                cursor["session_id"], input_tokens, output_tokens, total_tokens)

        uncached_input = input_tokens - cached_input_tokens
        if uncached_input < 0:
            logging.warning(
                "codex-scraper: cached_input_tokens exceeds input_tokens, clamping "
                "to 0 session_id=%s input=%d cached_input=%d",
                cursor["session_id"], input_tokens, cached_input_tokens)
            uncached_input = 0

        cost_usd = compute_cost(cursor["model"], uncached_input, output_tokens,
                                 cached_input_tokens, 0)
        ts = _format_event_timestamp(timestamp)

        row = {
            "message_id": message_id,
            "session_id": cursor["session_id"],
            "agent_name": cursor["agent_name"],  # "" for direct/parent spend, "@"+role for a subagent thread
            "host": self.host,
            "username": self.username,
            "model": cursor["model"],
            "input_tokens": uncached_input,
            "output_tokens": output_tokens,
            "cache_read_tokens": cached_input_tokens,
            "cache_write_tokens": 0,
            "cost_usd": cost_usd,
            "timestamp": ts,
            "runtime": "codex",
            "reasoning_output_tokens": reasoning_output_tokens,
        }

        if self.dry_run:
            logging.info(
                "dry-run session_id=%s message_id=%s model=%s cost_usd=%.6f "
                "input=%d output=%d",
                row["session_id"], row["message_id"], row["model"],
                row["cost_usd"], row["input_tokens"], row["output_tokens"])
            return

        if not self._post_telemetry(row):
            logging.error("telemetry POST failed session_id=%s message_id=%s",
                          row["session_id"], row["message_id"])
            cursor["seq"] = seq  # do not consume the sequence number for an unsent row
            raise _PostFailed()

    def _post_telemetry(self, row: dict) -> bool:
        """POST one row to hookd's /telemetry. Returns True on 202 Accepted."""
        try:
            body = json.dumps(row).encode("utf-8")
            req = urllib.request.Request(
                self.telemetry_url, data=body, method="POST",
                headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=_HTTP_TIMEOUT) as resp:
                resp.read()
                if resp.status != 202:
                    _log_error("telemetry POST non-202", status=resp.status,
                               session_id=row.get("session_id"),
                               message_id=row.get("message_id"))
                    return False
            return True
        except urllib.error.HTTPError as exc:
            _log_error("telemetry POST HTTPError", status=exc.code,
                       session_id=row.get("session_id"),
                       message_id=row.get("message_id"), error=str(exc))
            return False
        except Exception as exc:
            _log_error("telemetry POST error", session_id=row.get("session_id"),
                       message_id=row.get("message_id"), error=str(exc))
            return False

    def _upsert_codex_session(self, cursor: dict) -> None:
        """Upsert this tailer's owned view of the Codex sessions row via
        hookd's /session endpoint (WP-R3) — the remote-side equivalent of
        the hub-local Go scraper's direct store.UpsertSession call, which a
        Go-less remote cannot make. Best-effort: an error is logged, not
        raised — session-row freshness is not the ledger's correctness
        boundary; cost still flows even if this upsert lags/fails.

        Wire contract proposed to @hookd (see WP-R3 coordination): the
        client does not track first_seen locally, matching the Go struct's
        own behavior of stamping FirstSeen=LastSeen=now() on every call and
        relying on the upsert's ON DUPLICATE KEY UPDATE to leave the
        original first_seen untouched on a conflict.
        """
        if not self.session_url:
            logging.warning("codex-scraper: no session_url configured, skipping "
                            "session upsert session_id=%s", cursor["session_id"])
            return

        body = {
            "session_id": cursor["session_id"],
            "agent_name": cursor["agent_name"],  # "" for parent/direct-spend row, "@"+role for a subagent thread's own row
            "host": self.host,
            "username": self.username,
            "runtime": "codex",
            "cwd": cursor["cwd"],
            "model": cursor["model"],
            "originator": cursor["originator"],
            "cli_version": cursor["cli_version"],
        }

        if self.dry_run:
            logging.info("dry-run session upsert=%r", body)
            return

        try:
            data = json.dumps(body).encode("utf-8")
            req = urllib.request.Request(
                self.session_url, data=data, method="POST",
                headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=_HTTP_TIMEOUT) as resp:
                resp.read()
                if resp.status not in (200, 202):
                    _log_error("session POST non-2xx", status=resp.status,
                               session_id=cursor["session_id"])
                    logging.error("codex-scraper: session upsert non-2xx status=%d "
                                 "session_id=%s", resp.status, cursor["session_id"])
        except urllib.error.HTTPError as exc:
            _log_error("session POST HTTPError", status=exc.code,
                       session_id=cursor["session_id"], error=str(exc))
            logging.error("codex-scraper: session upsert failed session_id=%s: %s",
                         cursor["session_id"], exc)
        except Exception as exc:
            _log_error("session POST error", session_id=cursor["session_id"],
                       error=str(exc))
            logging.error("codex-scraper: session upsert failed session_id=%s: %s",
                         cursor["session_id"], exc)


def _format_event_timestamp(raw: str) -> str:
    """Normalize a rollout event's own timestamp to UTC RFC3339-with-nanos
    string form. LESSONS.md §3 (binding): timestamps must be EVENT time, not
    scrape time — attribution's temporal join depends on it. Falls back to
    now() (logged) only when the raw timestamp is missing/unparseable."""
    if raw:
        try:
            s = raw
            if s.endswith("Z"):
                s = s[:-1] + "+00:00"
            dt = datetime.fromisoformat(s)
            return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f000Z")
        except Exception:
            logging.warning("codex-scraper: bad timestamp, using now raw=%r", raw)
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f000Z")


# ---------------------------------------------------------------------------
# Config helpers
# ---------------------------------------------------------------------------

def _data_dir() -> str:
    """Resolve ~/teamster/var, matching token-scraper.py's _data_dir()."""
    home = os.path.expanduser("~")
    basedir = os.environ.get("TEAMSTER_BASEDIR", "")
    if basedir:
        d = os.path.join(basedir, "var")
        os.makedirs(d, exist_ok=True)
        return d
    for candidate in (os.path.join(home, "teamster"), "/usr/local/teamster"):
        var_dir = os.path.join(candidate, "var")
        if os.path.isdir(var_dir):
            return var_dir
    legacy = os.path.join(home, ".local", "share", "teamster")
    if os.path.isdir(legacy):
        return legacy
    default = os.path.join(home, "teamster", "var")
    os.makedirs(default, exist_ok=True)
    return default


def _hub_base_url() -> str:
    """Derive the hookd base URL (no trailing /event, /telemetry, /session)
    from TEAMSTER_HOOK_SERVER_URL, matching token-scraper.py's convention.
    The hub URL must be the hub's HOSTNAME, never localhost — a healed
    localhost breaks remotes (LESSONS.md §7)."""
    hook_url = os.environ.get("TEAMSTER_HOOK_SERVER_URL", "http://localhost:9125/event")
    base = hook_url
    if base.endswith("/event"):
        base = base[:-len("/event")]
    return base


def _telemetry_url() -> str:
    direct = os.environ.get("TEAMSTER_TELEMETRY_URL", "")
    if direct:
        return direct
    return _hub_base_url() + "/telemetry"


def _session_url() -> str:
    direct = os.environ.get("TEAMSTER_SESSION_URL", "")
    if direct:
        return direct
    return _hub_base_url() + "/session"


def _codex_home() -> str:
    return os.environ.get("CODEX_HOME") or os.path.join(os.path.expanduser("~"), ".codex")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main() -> int:
    parser = argparse.ArgumentParser(
        description="codex-scraper: ingest Codex CLI rollout token usage")
    parser.add_argument("--version", "-v", action="store_true",
                        help="print version and exit")
    args = parser.parse_args()

    if args.version:
        print(f"codex-scraper {__version__}")
        return 0

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
        stream=sys.stderr)

    codex_home = _codex_home()
    roots = [
        os.path.join(codex_home, "sessions"),
        os.path.join(codex_home, "archived_sessions"),
    ]
    if v := os.environ.get("CODEX_SCRAPER_SESSION_ROOTS", ""):
        roots = v.split(",")

    dry_run = os.environ.get("SCRAPER_DRY_RUN", "") in ("true", "1")

    telemetry_url = _telemetry_url()
    session_url = _session_url()
    data_dir = _data_dir()
    cursor_path = os.path.join(data_dir, "codex-scraper-cursors.json")

    host = os.environ.get("TEAMSTER_HOST", "")
    if not host:
        try:
            host = socket.gethostname().split(".")[0]
        except Exception:
            host = "localhost"

    username = os.environ.get("TEAMSTER_USER", "")
    if not username:
        try:
            username = getpass.getuser()
        except Exception:
            username = ""

    logging.info("codex-scraper %s starting roots=%s telemetry_url=%s "
                "session_url=%s dry_run=%s", __version__, roots, telemetry_url,
                session_url, dry_run)

    scraper = Scraper(
        telemetry_url=telemetry_url,
        session_url=session_url,
        host=host,
        username=username,
        roots=roots,
        cursor_path=cursor_path,
        dry_run=dry_run,
    )
    scraper.load_cursors()

    try:
        scraper.poll()
    except Exception as exc:
        _log_error("poll error", error=str(exc))
        logging.error("poll error: %s", exc)

    return 0


if __name__ == "__main__":
    # Belt-and-suspenders on top of main()'s own internal try/except: this
    # process must exit 0 in every case (reliability contract) — a cron/
    # launchd invocation that exits non-zero or crashes generates unwanted
    # noise on every host that runs it.
    try:
        sys.exit(main())
    except Exception:
        sys.exit(0)

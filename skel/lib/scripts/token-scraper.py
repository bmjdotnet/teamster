#!/usr/bin/env python3
"""token-scraper: reads Claude Code session JSONL files and POSTs per-message
token usage rows to hookd's /telemetry endpoint, and extracts wms_setFocus
events for focus-timeline recovery.

Default mode: single poll — process new bytes, save cursors, exit. Designed
for cron or systemd timer use.

--daemon mode: continuous poll loop with SIGINT/SIGTERM handling.

Pure-stdlib Python 3.8+. No third-party dependencies.
"""
from __future__ import annotations

import argparse
import glob
import json
import logging
import os
import pathlib
import shutil
import signal
import socket
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

# ---------------------------------------------------------------------------
# Version
# ---------------------------------------------------------------------------

_LOG_MAX_BYTES = 1_000_000  # rotate at ~1 MB


def _read_version() -> str:
    """Read ~/teamster/VERSION; fall back to 'dev'."""
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
# Pricing table
# SYNC ANCHOR: keep in sync with internal/pricing/pricing.go
# ---------------------------------------------------------------------------

# ModelPricing: (input, output, cache_read, cache_write) — USD per token.
_KNOWN: dict[str, tuple[float, float, float, float]] = {
    "claude-opus-4-5":  (0.000005,  0.000025,  0.0000005,  0.00000625),
    "claude-opus-4-6":  (0.000005,  0.000025,  0.0000005,  0.00000625),
    "claude-opus-4-7":  (0.000005,  0.000025,  0.0000005,  0.00000625),
    # opus-4-8 explicit entry: all opus 4.5+ are $5/$25 per Mtok; only
    # opus 4.0/4.1 were $15/$75.  Derived from COMPLETED anchor session a856fa7e.
    "claude-opus-4-8":   (0.000005,  0.000025,  0.0000005,  0.00000625),
    "claude-sonnet-4-5": (0.000003,  0.000015,  0.0000003,  0.00000375),
    "claude-sonnet-4-6": (0.000003,  0.000015,  0.0000003,  0.00000375),
    "claude-haiku-4-5":  (0.0000008, 0.000004,  0.00000008, 0.000001),
    # fable-5: 2x opus-4-8 tier (operator-confirmed).
    # Derived from COMPLETED anchor session a856fa7e (-1.5% vs OTel $154.69).
    "claude-fable-5":    (0.00001,   0.00005,   0.000001,   0.0000125),
}

# Same-class fallback rates (most-recent known rate per class).
_CLASS_RATES: dict[str, tuple[float, float, float, float]] = {
    "opus":   (0.000005,  0.000025,  0.0000005,  0.00000625),
    "sonnet": (0.000003,  0.000015,  0.0000003,  0.00000375),
    "haiku":  (0.0000008, 0.000004,  0.00000008, 0.000001),
    "fable":  (0.00001,   0.00005,   0.000001,   0.0000125),
}


def _price_for(model: str) -> tuple[float, float, float, float] | None:
    """Resolve pricing for a model string.

    Lookup order:
    1. Exact match in _KNOWN.
    2. Longest key in _KNOWN that is a prefix of model (handles dated suffixes
       like claude-sonnet-4-5-20250929).
    3. Same-class fallback via _CLASS_RATES (logs a warning — estimate only).
    Returns None when no pricing can be determined.
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

    for cls, rates in _CLASS_RATES.items():
        if cls in model:
            logging.warning(
                "priced model via same-class fallback (estimate, not authoritative) "
                "model=%s class=%s", model, cls)
            return rates

    return None


def compute_cost(model: str, input_tokens: int, output_tokens: int,
                 cache_read_tokens: int, cache_write_tokens: int) -> float:
    """Return total USD cost for the given token counts. Returns 0 for unknown models."""
    rates = _price_for(model)
    if rates is None:
        return 0.0
    inp, out, cr, cw = rates
    return (input_tokens * inp + output_tokens * out
            + cache_read_tokens * cr + cache_write_tokens * cw)


# ---------------------------------------------------------------------------
# Error logging (mirrors teamster.py pattern)
# ---------------------------------------------------------------------------

def _log_error(msg: str, **extra) -> None:
    """Append a JSON line to ~/teamster/var/scraper-errors.log. Never raises."""
    try:
        log_dir = os.path.join(os.path.expanduser("~"), "teamster", "var")
        os.makedirs(log_dir, exist_ok=True)
        log_path = os.path.join(log_dir, "scraper-errors.log")
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
# agentName resolution from transcript (macOS top-level teammate sessions)
# ---------------------------------------------------------------------------

# On macOS, teammate sessions are top-level JSONL files with no .meta.json
# sidecar, so _agent_name_for() returns "". Scan the transcript itself for the
# first top-level "agentName" record and use it as the agent identity.
# Bounded to _AGENTNAME_SCAN_LIMIT bytes; agentName appears near the top.
_AGENTNAME_SCAN_LIMIT = 256 * 1024  # 256 KB — matches teamster.py constant

# Per-process memo: file path -> "@agentName" or "" (lead sessions).
# The scraper is long-running in daemon mode; this avoids re-scanning on
# every poll interval for sessions whose agentName is already known.
_agentname_cache: dict[str, str] = {}


def _agent_name_from_transcript(path: str) -> str:
    """Scan the head of a session transcript for a top-level agentName field.

    On macOS, dispatched teammates run as top-level sessions whose transcript
    contains an early record like {"agentName":"PizzaDude",...}. Lead sessions
    have no agentName. Returns "@<agentName>" or "" (not found / any error).

    Results are memoised in _agentname_cache keyed by file path so repeated
    poll() calls skip re-reading known-identified files. Empty results are NOT
    cached — if agentName hasn't been written yet (race between scraper poll
    and transcript write), the next poll will retry. Never raises.
    """
    if path in _agentname_cache:
        return _agentname_cache[path]
    result = ""
    try:
        bytes_read = 0
        with open(path, "rb") as f:
            for raw_line in f:
                bytes_read += len(raw_line)
                if bytes_read > _AGENTNAME_SCAN_LIMIT:
                    break
                if b"agentName" not in raw_line:
                    continue
                try:
                    rec = json.loads(raw_line)
                    name = rec.get("agentName", "")
                    if name and isinstance(name, str):
                        result = "@" + name
                        break
                except (json.JSONDecodeError, TypeError, AttributeError):
                    continue
    except Exception:
        pass  # unreadable transcript → leave agent_name empty
    # Only cache non-empty results. If agentName hasn't been written yet
    # (race: scraper polls the transcript before the first agentName record
    # appears), a miss here lets the next poll retry rather than permanently
    # misattributing this teammate's cost to the lead session.
    if result:
        _agentname_cache[path] = result
    return result


# ---------------------------------------------------------------------------
# Long-context ("[1m]") config
# ---------------------------------------------------------------------------

# Claude Code's API response (transcript message.model) never echoes back the
# "[1m]" long-context-beta annotation — it's a client-side label that only
# lives in ~/.claude/settings.json's "model" field (e.g. "claude-fable-5[1m]").
# Mirrors the Go scraper's identical constant and health-collector's copy
# (cmd/health-collector/main.go's longContextSuffix), which reads it back out
# of token_ledger.model.
_LONG_CONTEXT_SUFFIX = "[1m]"


def _configured_model() -> str:
    """Read the CLI's configured model from ~/.claude/settings.json.

    Mirrors Go's configuredModel() (cmd/token-scraper/main.go). Returns "" on
    any error (missing file, malformed JSON, no "model" key) — the caller then
    applies no suffix.
    """
    try:
        path = os.path.join(os.path.expanduser("~"), ".claude", "settings.json")
        with open(path) as f:
            data = json.load(f)
        model = data.get("model", "")
        return model if isinstance(model, str) else ""
    except Exception:
        return ""


# ---------------------------------------------------------------------------
# Dedup / usage helpers
# ---------------------------------------------------------------------------

def _dedup_key(line: dict) -> str:
    """Return the dedup key for a session transcript line.

    Matches Go dedupKey(): uses message.id + "|" + requestId when message.id
    is present; falls back to the top-level uuid so older lines are each their
    own group.
    """
    msg_id = (line.get("message") or {}).get("id", "")
    if not msg_id:
        return line.get("uuid", "")
    return msg_id + "|" + line.get("requestId", "")


def _usage_from_line(line: dict) -> dict:
    """Extract a usage dict from a parsed assistant transcript line."""
    msg = line.get("message") or {}
    usage = msg.get("usage") or {}
    cache_creation = usage.get("cache_creation") or {}
    content = msg.get("content") or []

    n_text = n_tool_use = n_thinking = 0
    for block in content:
        t = block.get("type", "")
        if t == "text":
            n_text += 1
        elif t == "tool_use":
            n_tool_use += 1
        elif t == "thinking":
            n_thinking += 1

    input_tokens = int(usage.get("input_tokens") or 0)
    output_tokens = int(usage.get("output_tokens") or 0)
    cache_read = int(usage.get("cache_read_input_tokens") or 0)
    cache_write = int(usage.get("cache_creation_input_tokens") or 0)
    cache_write_1h = int(cache_creation.get("ephemeral_1h_input_tokens") or 0)
    cache_write_5m = int(cache_creation.get("ephemeral_5m_input_tokens") or 0)

    return {
        "message_id":        _dedup_key(line),
        "session_id":        line.get("sessionId", ""),
        "timestamp":         line.get("timestamp", ""),
        "model":             msg.get("model", ""),
        "input_tokens":      input_tokens,
        "output_tokens":     output_tokens,
        "cache_read_tokens": cache_read,
        "cache_write_tokens": cache_write,
        "cache_write_1h":    cache_write_1h,
        "cache_write_5m":    cache_write_5m,
        "n_text":            n_text,
        "n_tool_use":        n_tool_use,
        "n_thinking":        n_thinking,
        "total_input":       input_tokens + cache_read + cache_write,
        "stop_reason":       msg.get("stop_reason", ""),
        "service_tier":      usage.get("service_tier", ""),
        "speed":             usage.get("speed", ""),
    }


def _merge_usage(dst: dict, src: dict) -> None:
    """Fold src into dst keeping per-field MAX; recompute total_input.

    Matches Go mergeUsage() exactly. Early streamed snapshots carry partial
    output_tokens or fewer content blocks; the final line has the true totals.
    """
    for field in ("input_tokens", "output_tokens", "cache_read_tokens",
                  "cache_write_tokens", "cache_write_1h", "cache_write_5m"):
        if src[field] > dst[field]:
            dst[field] = src[field]
    for field in ("n_text", "n_tool_use", "n_thinking"):
        if src[field] > dst[field]:
            dst[field] = src[field]
    # Recompute rather than max independently (matches Go).
    dst["total_input"] = (dst["input_tokens"]
                          + dst["cache_read_tokens"]
                          + dst["cache_write_tokens"])
    if not dst["stop_reason"]:
        dst["stop_reason"] = src["stop_reason"]


# ---------------------------------------------------------------------------
# Focus-event extraction (mirrors Go transcript.appendFocusEvents)
# ---------------------------------------------------------------------------

_SET_FOCUS_NAMES = frozenset({
    "mcp__wms__wms_setFocus",
    "wms_setFocus",
    "setFocus",
})


def _extract_focus_events(line: dict, session_id: str, agent_name: str,
                          host: str, username: str) -> list[dict]:
    """Extract wms_setFocus tool_use blocks from a parsed JSONL line.

    Mirrors Go appendFocusEvents(): cheap pre-filter on raw content, then
    structured walk of message.content blocks looking for tool_use with a
    known setFocus name variant.
    """
    if line.get("type") != "assistant":
        return []
    msg = line.get("message") or {}
    content = msg.get("content")
    if not content or not isinstance(content, list):
        return []

    ts = line.get("timestamp", "")
    line_agent = agent_name
    if not line_agent:
        at = line.get("agentType", "")
        if at:
            line_agent = "@" + at

    events = []
    for block in content:
        if not isinstance(block, dict):
            continue
        if block.get("type") != "tool_use":
            continue
        if block.get("name") not in _SET_FOCUS_NAMES:
            continue
        inp = block.get("input") or {}
        entity_type = inp.get("entityType", "")
        entity_id = inp.get("entityID", "")
        if not entity_type and not entity_id:
            continue
        events.append({
            "type": "focus_timeline",
            "session_id": session_id,
            "host": host,
            "username": username,
            "agent_name": line_agent,
            "entity_type": entity_type,
            "entity_id": entity_id,
            "focus": inp.get("focus", ""),
            "timestamp": ts,
        })
    return events


# ---------------------------------------------------------------------------
# Brief-directive extraction (focus-less teammate fallback)
# ---------------------------------------------------------------------------
#
# A teammate dispatched by a lead receives a `teammate-message` brief whose
# FIRST instruction is the Teamster-mandated dispatch directive (see
# skel/lib/plugin/skills/bootstrap/SKILL.md §"Write the technical brief"):
#
#     wms_setFocus(entityType="workunit", entityID="<id>", focus="<short what>")
#
# A teammate that does its work but NEVER actually calls wms_setFocus produces a
# focus-less session whose cost would otherwise stay unallocated forever (the
# hub can't read the Mac's transcript). The brief, however, deterministically
# names the entity the teammate WAS told to focus on — a protocol-grounded link,
# not a heuristic guess. We parse that directive from the teammate's own
# transcript head and ship it to the hub as the session's INTENDED focus.
#
# This is emitted ONLY for sessions with no real setFocus (a real call always
# wins — see poll()), and the hub treats it as subordinate to any real focus
# interval (identity_source='brief_directive', never overrides 'remote_scraper'
# or 'direct'). The directive's timestamp is the session's first-message ts, so
# the intended focus covers the whole session.

# The directive may appear with single or double quotes, or unquoted, and the
# two fields may be in either order. We match each field independently within
# the brief rather than assuming a fixed call shape, so reformatting the brief
# (line wraps, extra args) does not break extraction.
_DIRECTIVE_ENTITY_TYPE_RE = None
_DIRECTIVE_ENTITY_ID_RE = None
_DIRECTIVE_FOCUS_RE = None


def _directive_res():
    """Lazily compile the brief-directive regexes (py3.6-safe; no module-scope
    typed globals)."""
    global _DIRECTIVE_ENTITY_TYPE_RE, _DIRECTIVE_ENTITY_ID_RE, _DIRECTIVE_FOCUS_RE
    if _DIRECTIVE_ENTITY_TYPE_RE is None:
        import re
        _DIRECTIVE_ENTITY_TYPE_RE = re.compile(
            r"entityType\s*[=:]\s*[\"']?([A-Za-z]+)")
        _DIRECTIVE_ENTITY_ID_RE = re.compile(
            r"entityID\s*[=:]\s*[\"']?([A-Za-z0-9._-]+)")
        _DIRECTIVE_FOCUS_RE = re.compile(
            r"focus\s*[=:]\s*[\"']([^\"']{1,200})[\"']")
    return _DIRECTIVE_ENTITY_TYPE_RE, _DIRECTIVE_ENTITY_ID_RE, _DIRECTIVE_FOCUS_RE


# Per-process memo: file path -> directive event dict, or "" when the head has
# been scanned and holds no usable directive. Mirrors _agentname_cache so the
# daemon does not re-scan known files every poll. A None (absent) entry means
# "not yet scanned"; a "" entry means "scanned, no directive" (cached so we
# stop re-reading briefless sessions).
_directive_cache: dict = {}


def _line_text(line: dict) -> str:
    """Flatten a user/assistant message's content into a single string.

    Content may be a plain string or a list of blocks; we concatenate the text
    of any string blocks. Never raises.
    """
    msg = line.get("message") or {}
    content = msg.get("content")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for block in content:
            if isinstance(block, dict):
                t = block.get("text")
                if isinstance(t, str):
                    parts.append(t)
            elif isinstance(block, str):
                parts.append(block)
        return "".join(parts)
    return ""


def _extract_brief_directive(path: str, agent_name: str, host: str,
                             username: str) -> dict:
    """Scan a transcript head for a focus-less teammate's brief directive.

    Returns a single 'focus_timeline' event dict carrying the entity the brief
    told the teammate to focus on (with type='brief_directive'), or "" when the
    head holds no parseable directive. The event's timestamp is the session's
    first record timestamp so the intended focus covers the whole session.

    Bounded to _AGENTNAME_SCAN_LIMIT bytes (the brief is the first user message).
    Memoised per path. Never raises.
    """
    cached = _directive_cache.get(path)
    if cached is not None:
        return cached or ""

    result = ""
    try:
        et_re, eid_re, focus_re = _directive_res()
        session_id = ""
        first_ts = ""
        bytes_read = 0
        with open(path, "rb") as f:
            for raw_line in f:
                bytes_read += len(raw_line)
                if bytes_read > _AGENTNAME_SCAN_LIMIT:
                    break
                try:
                    line = json.loads(raw_line)
                except (json.JSONDecodeError, ValueError):
                    continue
                if not isinstance(line, dict):
                    continue
                if not session_id:
                    session_id = line.get("sessionId", "")
                if not first_ts:
                    ts = line.get("timestamp", "")
                    if ts:
                        first_ts = ts
                # The directive lives in the first teammate-message user record.
                if line.get("type") != "user":
                    continue
                text = _line_text(line)
                if "teammate-message" not in text and "setFocus" not in text:
                    continue
                et_m = et_re.search(text)
                eid_m = eid_re.search(text)
                if not et_m or not eid_m:
                    continue
                focus_m = focus_re.search(text)
                line_agent = agent_name
                if not line_agent:
                    at = line.get("agentType", "")
                    if at:
                        line_agent = "@" + at
                result = {
                    "type": "focus_timeline",
                    "directive": True,
                    "session_id": session_id or line.get("sessionId", ""),
                    "host": host,
                    "username": username,
                    "agent_name": line_agent,
                    "entity_type": et_m.group(1),
                    "entity_id": eid_m.group(1),
                    "focus": focus_m.group(1) if focus_m else "",
                    "timestamp": first_ts or line.get("timestamp", ""),
                }
                break
    except Exception:
        result = ""  # unreadable transcript → no directive

    # Cache both hits and clean misses so the daemon stops re-scanning. An
    # exception path leaves result="" but we still cache it: a transcript that
    # raises once will keep raising, and re-scanning it every poll is wasteful.
    _directive_cache[path] = result
    return result


# ---------------------------------------------------------------------------
# Scraper
# ---------------------------------------------------------------------------

class Scraper:
    def __init__(self, *, telemetry_url: str, host: str, username: str,
                 session_glob: str, cursor_path: str, dry_run: bool,
                 data_dir: str):
        self.telemetry_url = telemetry_url
        self.host = host
        self.username = username
        self.session_glob = session_glob
        self.cursor_path = cursor_path
        self.dry_run = dry_run
        self.data_dir = data_dir
        # cursors: {filepath: {"offset": int}}
        self.cursors: dict[str, dict] = {}
        self._http_timeout = 5
        # Derive focus-timeline URL from telemetry URL base.
        base = telemetry_url
        if base.endswith("/telemetry"):
            base = base[:-len("/telemetry")]
        self._focus_url = base + "/focus-timeline"
        # Transcript cache directory.
        self._cache_dir = os.path.join(data_dir, "transcript-cache")
        # Killswitch path: touch this file to disable enhanced extraction.
        self._killswitch_path = os.path.join(data_dir, ".no-sweep")
        # Track paths whose brief directive has already been emitted this
        # process lifetime, avoiding redundant POSTs on subsequent polls.
        self._directives_emitted: set = set()

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
    # Killswitch
    # ------------------------------------------------------------------

    def _killswitch_active(self) -> bool:
        """Check if the .no-sweep killswitch file exists."""
        return os.path.exists(self._killswitch_path)

    # ------------------------------------------------------------------
    # Transcript cache
    # ------------------------------------------------------------------

    def _cache_transcript(self, path: str) -> None:
        """Copy a transcript file to the local cache directory.

        Uses the session directory name as the cache key. Skips the copy when
        the cache is already at least as fresh as the source (mtime check).
        If the copy fails (permissions, disk full), log and continue.
        """
        try:
            session_dir = os.path.basename(os.path.dirname(path))
            if not session_dir or session_dir == ".":
                session_dir = os.path.basename(path)
            cache_path = os.path.join(self._cache_dir, session_dir + ".jsonl")
            try:
                src_stat = os.stat(path)
                dst_stat = os.stat(cache_path)
                if dst_stat.st_mtime >= src_stat.st_mtime:
                    return
            except OSError:
                pass  # cache doesn't exist yet, proceed with copy
            os.makedirs(self._cache_dir, exist_ok=True)
            shutil.copy2(path, cache_path)
        except Exception as exc:
            _log_error("transcript cache failed", path=path, error=str(exc))
            logging.warning("transcript cache failed path=%s: %s", path, exc)

    def _cached_path_for(self, path: str) -> str | None:
        """Return the cached transcript path if it exists, else None."""
        session_dir = os.path.basename(os.path.dirname(path))
        if not session_dir or session_dir == ".":
            session_dir = os.path.basename(path)
        cache_path = os.path.join(self._cache_dir, session_dir + ".jsonl")
        if os.path.exists(cache_path):
            return cache_path
        return None

    # ------------------------------------------------------------------
    # Poll
    # ------------------------------------------------------------------

    def poll(self, stop_event=None) -> None:
        killswitch = self._killswitch_active()
        if killswitch:
            logging.info("killswitch active (.no-sweep exists), "
                         "skipping enhanced extraction")

        all_focus: list[dict] = []
        paths = sorted(glob.glob(self.session_glob))
        seen_cursor_keys = set()

        for path in paths:
            if stop_event is not None and stop_event.is_set():
                break
            seen_cursor_keys.add(path)

            if not killswitch:
                self._cache_transcript(path)

            all_focus.extend(
                self._poll_one(path, path, killswitch, stop_event))

        # Cache recovery: process transcripts whose originals were deleted
        # between scraper polls. Only runs when killswitch is off (caching
        # is an enhanced feature). Check cursor keys we already know about
        # that didn't appear in the glob — the original is gone.
        if not killswitch:
            for cursor_key in list(self.cursors):
                if stop_event is not None and stop_event.is_set():
                    break
                if cursor_key in seen_cursor_keys:
                    continue
                cached = self._cached_path_for(cursor_key)
                if not cached:
                    continue
                logging.warning(
                    "transcript deleted, processing from cache "
                    "path=%s cache=%s", cursor_key, cached)
                all_focus.extend(
                    self._poll_one(cached, cursor_key, killswitch,
                                   stop_event))

        if all_focus:
            self._post_focus_events(all_focus)

        if not self.dry_run:
            self.save_cursors()

    def _poll_one(self, effective_path: str, cursor_key: str,
                  killswitch: bool, stop_event) -> list[dict]:
        """Process a single session transcript and return focus events."""
        focus: list[dict] = []

        agent_name = _agent_name_from_transcript(effective_path)
        try:
            file_focus = self._process_file(
                effective_path, cursor_key, agent_name)
            if not killswitch:
                focus.extend(file_focus)
        except Exception as exc:
            _log_error("process_file error", path=cursor_key,
                       error=str(exc))
            logging.error("process file error path=%s: %s",
                          cursor_key, exc)

        # Subagent walking always runs (token telemetry is basic), but
        # focus events from subagents are only collected when killswitch
        # is off.
        sub_focus = self._process_subagents(effective_path, stop_event)
        if not killswitch:
            focus.extend(sub_focus)

        if not killswitch:
            if effective_path not in self._directives_emitted:
                try:
                    directive = _extract_brief_directive(
                        effective_path, agent_name, self.host,
                        self.username)
                    if directive:
                        focus.append(directive)
                        self._directives_emitted.add(effective_path)
                except Exception as exc:
                    _log_error("brief_directive error", path=cursor_key,
                               error=str(exc))
                    logging.error("brief directive error path=%s: %s",
                                  cursor_key, exc)

        return focus

    # ------------------------------------------------------------------
    # Subagent walking
    # ------------------------------------------------------------------

    def _process_subagents(self, main_path: str,
                           stop_event=None) -> list[dict]:
        """Walk the subagents/ directory next to a main session file."""
        focus_events: list[dict] = []
        base = main_path[:-6] if main_path.endswith(".jsonl") else main_path
        sub_dir = os.path.join(base, "subagents")
        pattern = os.path.join(sub_dir, "agent-*.jsonl")
        try:
            entries = sorted(glob.glob(pattern))
        except Exception as exc:
            logging.debug("subagent glob error dir=%s: %s", sub_dir, exc)
            return focus_events
        for sub in entries:
            if stop_event is not None and stop_event.is_set():
                return focus_events
            agent_name = self._agent_name_for(sub)
            try:
                focus = self._process_file(sub, sub, agent_name)
                focus_events.extend(focus)
            except Exception as exc:
                _log_error("process_subagent error", path=sub, error=str(exc))
                logging.error("process subagent file error path=%s: %s", sub, exc)
        return focus_events

    def _agent_name_for(self, sub_path: str) -> str:
        """Read agentType from sibling .meta.json; return '@'+agentType or ''."""
        meta_path = (sub_path[:-6] if sub_path.endswith(".jsonl")
                     else sub_path) + ".meta.json"
        try:
            with open(meta_path) as f:
                meta = json.load(f)
        except FileNotFoundError:
            logging.debug("subagent meta missing path=%s", meta_path)
            return ""
        except Exception as exc:
            logging.debug("subagent meta malformed path=%s: %s", meta_path, exc)
            return ""
        agent_type = meta.get("agentType", "")
        if not agent_type:
            return ""
        return "@" + agent_type

    # ------------------------------------------------------------------
    # File processing — faithful port of Go processFile()
    # ------------------------------------------------------------------

    def _process_file(self, path: str, cursor_key: str,
                       agent_name: str) -> list[dict]:
        """Ingest one session JSONL file from its current cursor offset.

        path: the file to actually read (may be a cache copy).
        cursor_key: the key to track in self.cursors (always the original path).
        Returns any focus events extracted from new bytes.
        """
        try:
            file_size = os.path.getsize(path)
        except OSError:
            return []  # file disappeared between glob and stat

        cursor = self.cursors.setdefault(cursor_key, {"offset": 0})

        # Reset if file was truncated/rotated.
        if file_size < cursor["offset"]:
            cursor["offset"] = 0

        if file_size == cursor["offset"]:
            return []

        try:
            f = open(path, "rb")
        except OSError:
            return []

        try:
            return self._read_file(f, path, agent_name, cursor)
        finally:
            f.close()

    def _read_file(self, f, path: str, agent_name: str,
                   cursor: dict) -> list[dict]:
        """Inner read loop — mirrors the Go scanner loop in processFile().

        Cursor safety contract (matches Go exactly):
        - new_offset tracks the byte offset of the end of the last *successfully
          flushed* group.
        - group_start is the byte offset of the start of the *current open* group.
        - On flush success: new_offset = group_start (start of the group just flushed),
          then after advancing past EOF new_offset = pos.
        - On flush / POST failure: stop iterating; cursor.offset = new_offset
          (last safely committed position).

        Returns focus events extracted from the new bytes.
        """
        if cursor["offset"] > 0:
            f.seek(cursor["offset"])

        new_offset = cursor["offset"]
        post_failed = False
        focus_events: list[dict] = []

        # The lead's own transcript is the only one _configured_model()'s
        # "[1m]" setting can be safely attributed to — a subagent may be
        # dispatched on a different model entirely (see
        # teamster-context-bug.md), so this is scoped to agent_name == ""
        # (main session file) only.
        long_context_base = ""
        if not agent_name:
            cfg_model = _configured_model()
            if cfg_model.endswith(_LONG_CONTEXT_SUFFIX):
                long_context_base = cfg_model[:-len(_LONG_CONTEXT_SUFFIX)]

        # Derive session_id from filename: <session_id>.jsonl or
        # subagents/agent-<id>.jsonl — for subagents, walk up to the main
        # session dir name.
        session_id = ""

        cur: dict | None = None   # open group's running max-usage representative
        cur_key: str = ""         # dedup key of the open group
        group_start = cursor["offset"]
        pos = cursor["offset"]    # byte offset of line currently being read

        def flush() -> bool:
            nonlocal cur, new_offset, post_failed
            if cur is None:
                return True
            if not self._emit(cur, agent_name):
                post_failed = True
                return False
            cur = None
            new_offset = group_start  # advance past the just-flushed group
            return True

        while True:
            line_start = f.tell()
            raw = f.readline()
            if not raw:
                break  # EOF

            # readline() returns bytes including the newline (or without at EOF).
            # len(raw) gives us the exact byte advance.
            line_len = len(raw)
            pos = line_start + line_len

            raw = raw.rstrip(b"\n\r")
            if not raw:
                # Blank line — flush current group, advance.
                if not flush():
                    break
                new_offset = pos
                continue

            try:
                line = json.loads(raw)
            except Exception as exc:
                logging.debug("skipping malformed line path=%s: %s", path, exc)
                if not flush():
                    break
                new_offset = pos
                continue

            if not isinstance(line, dict):
                if not flush():
                    break
                new_offset = pos
                continue

            # Grab session_id from the first line that has one.
            if not session_id:
                session_id = line.get("sessionId", "")

            # Focus extraction: runs on every assistant line regardless of
            # usage, matching Go appendFocusEvents() which is independent
            # of the token-usage dedup path.
            if (line.get("type") == "assistant"
                    and b"setFocus" in (raw if isinstance(raw, bytes)
                                        else raw.encode("utf-8", "replace"))):
                try:
                    evs = _extract_focus_events(
                        line, session_id or line.get("sessionId", ""),
                        agent_name, self.host, self.username)
                    focus_events.extend(evs)
                except Exception as exc:
                    logging.debug("focus extraction error path=%s: %s",
                                  path, exc)

            if (line.get("type") != "assistant"
                    or not line.get("sessionId")
                    or not line.get("message")):
                if not flush():
                    break
                new_offset = pos
                continue

            msg = line.get("message") or {}
            usage = msg.get("usage") or {}
            if int(usage.get("input_tokens") or 0) + int(usage.get("output_tokens") or 0) == 0:
                if not flush():
                    break
                new_offset = pos
                continue

            u = _usage_from_line(line)
            if long_context_base and u["model"] == long_context_base:
                u["model"] = u["model"] + _LONG_CONTEXT_SUFFIX
            key = u["message_id"]  # == dedupKey(line)

            if cur is not None and key == cur_key:
                _merge_usage(cur, u)  # same request, another content block
                continue

            # New group: flush the previous one, open this.
            if not flush():
                break
            cur = u
            cur_key = key
            group_start = line_start

        # EOF reached cleanly (no scanner error equivalent in Python readline):
        # the trailing group is complete, flush it too.
        if not post_failed:
            if flush():
                new_offset = pos

        cursor["offset"] = new_offset
        return focus_events

    # ------------------------------------------------------------------
    # Emit / POST
    # ------------------------------------------------------------------

    def _emit(self, u: dict, agent_name: str) -> bool:
        """Price and send one deduplicated request row. Returns False on POST failure."""
        cost_usd = compute_cost(
            u["model"], u["input_tokens"], u["output_tokens"],
            u["cache_read_tokens"], u["cache_write_tokens"])

        if self.dry_run:
            logging.info(
                "dry-run session_id=%s message_id=%s agent_name=%r model=%s "
                "cost_usd=%.6f total_input=%d",
                u["session_id"], u["message_id"], agent_name,
                u["model"], cost_usd, u["total_input"])
            return True

        # Normalize timestamp to RFC3339Nano UTC (matches Go time.RFC3339Nano).
        ts = u.get("timestamp", "")
        if ts:
            try:
                # Claude Code timestamps look like: 2025-09-29T12:34:56.789Z
                # or RFC3339 with offset.  Normalise to UTC RFC3339.
                if ts.endswith("Z"):
                    ts = ts[:-1] + "+00:00"
                dt = datetime.fromisoformat(ts)
                ts = dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f000Z")
            except Exception:
                pass  # leave as-is

        row = {
            "message_id":        u["message_id"],
            "session_id":        u["session_id"],
            "agent_name":        agent_name,
            "host":              self.host,
            "username":          self.username,
            "model":             u["model"],
            "input_tokens":      u["input_tokens"],
            "output_tokens":     u["output_tokens"],
            "cache_read_tokens": u["cache_read_tokens"],
            "cache_write_tokens": u["cache_write_tokens"],
            "cache_write_1h":    u["cache_write_1h"],
            "cache_write_5m":    u["cache_write_5m"],
            "n_text":            u["n_text"],
            "n_tool_use":        u["n_tool_use"],
            "n_thinking":        u["n_thinking"],
            "total_input":       u["total_input"],
            "stop_reason":       u["stop_reason"],
            "service_tier":      u["service_tier"],
            "speed":             u["speed"],
            "cost_usd":          cost_usd,
            "timestamp":         ts,
        }

        ok = self._post_telemetry(row)
        if not ok:
            logging.error("telemetry POST failed session_id=%s message_id=%s",
                          u["session_id"], u["message_id"])
        return ok

    def _post_telemetry(self, row: dict) -> bool:
        """POST a single row to the telemetry endpoint. Returns True on 202."""
        try:
            body = json.dumps(row).encode("utf-8")
            req = urllib.request.Request(
                self.telemetry_url, data=body, method="POST",
                headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=self._http_timeout) as resp:
                _ = resp.read()
                if resp.status != 202:
                    _log_error("telemetry POST non-202",
                               status=resp.status,
                               session_id=row.get("session_id"),
                               message_id=row.get("message_id"))
                    return False
            return True
        except urllib.error.HTTPError as exc:
            _log_error("telemetry POST HTTPError",
                       status=exc.code,
                       session_id=row.get("session_id"),
                       message_id=row.get("message_id"),
                       error=str(exc))
            return False
        except Exception as exc:
            _log_error("telemetry POST error",
                       session_id=row.get("session_id"),
                       message_id=row.get("message_id"),
                       error=str(exc))
            return False

    def _post_focus_events(self, events: list[dict]) -> None:
        """Batch-POST focus events to hookd /focus-timeline. Swallows errors."""
        if not events:
            return
        if self.dry_run:
            logging.info("dry-run focus_events count=%d", len(events))
            for ev in events:
                logging.info("  focus%s: session=%s agent=%s entity=%s/%s focus=%s",
                             " (brief_directive)" if ev.get("directive") else "",
                             ev.get("session_id"), ev.get("agent_name"),
                             ev.get("entity_type"), ev.get("entity_id"),
                             ev.get("focus"))
            return
        try:
            body = json.dumps(events).encode("utf-8")
            req = urllib.request.Request(
                self._focus_url, data=body, method="POST",
                headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=self._http_timeout) as resp:
                _ = resp.read()
                if resp.status not in (200, 202):
                    _log_error("focus POST non-2xx",
                               status=resp.status, count=len(events))
            logging.info("posted %d focus events", len(events))
        except urllib.error.HTTPError as exc:
            _log_error("focus POST HTTPError",
                       status=exc.code, count=len(events),
                       error=str(exc))
        except Exception as exc:
            _log_error("focus POST error", count=len(events),
                       error=str(exc))


# ---------------------------------------------------------------------------
# Config helpers
# ---------------------------------------------------------------------------

def _data_dir() -> str:
    """Resolve ~/teamster/var with the same logic as config.Load().

    Checks ~/teamster/var and /usr/local/teamster/var; falls back to
    ~/.local/share/teamster for legacy installs. Creates the directory.
    """
    home = os.path.expanduser("~")
    # TEAMSTER_BASEDIR override.
    basedir = os.environ.get("TEAMSTER_BASEDIR", "")
    if basedir:
        d = os.path.join(basedir, "var")
        os.makedirs(d, exist_ok=True)
        return d
    # Walk candidate base dirs (same order as config.go).
    for candidate in (os.path.join(home, "teamster"),
                      "/usr/local/teamster"):
        var_dir = os.path.join(candidate, "var")
        if os.path.isdir(var_dir):
            return var_dir
    # Legacy fallback.
    legacy = os.path.join(home, ".local", "share", "teamster")
    if os.path.isdir(legacy):
        return legacy
    # Create default.
    default = os.path.join(home, "teamster", "var")
    os.makedirs(default, exist_ok=True)
    return default


def _telemetry_url() -> str:
    """Derive telemetry URL from environment (matches Go main())."""
    direct = os.environ.get("TEAMSTER_TELEMETRY_URL", "")
    if direct:
        return direct
    hook_url = os.environ.get("TEAMSTER_HOOK_SERVER_URL", "http://localhost:9125/event")
    base = hook_url
    if base.endswith("/event"):
        base = base[:-len("/event")]
    return base + "/telemetry"


# ---------------------------------------------------------------------------
# Signal / daemon helpers
# ---------------------------------------------------------------------------

class _StopEvent:
    """Simple threading-free stop flag."""
    def __init__(self):
        self._set = False

    def set(self):
        self._set = True

    def is_set(self) -> bool:
        return self._set


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main() -> int:
    parser = argparse.ArgumentParser(
        description="token-scraper: ingest Claude Code session JSONL token usage")
    parser.add_argument("--daemon", action="store_true",
                        help="run as continuous poll daemon (default: single poll + exit)")
    parser.add_argument("--version", "-v", action="store_true",
                        help="print version and exit")
    args = parser.parse_args()

    if args.version:
        print(f"token-scraper {__version__}")
        return 0

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
        stream=sys.stderr)

    # Config from environment.
    poll_interval = 30
    if v := os.environ.get("SCRAPER_POLL_INTERVAL", ""):
        try:
            n = int(v)
            if n > 0:
                poll_interval = n
        except ValueError:
            pass

    # On macOS, Claude Code also uses ~/.claude/projects/ (same path as Linux).
    # If a future macOS Claude Code release moves transcripts under
    # ~/Library/Application Support/, this glob would need a fallback for that path.
    session_glob = os.path.join(
        os.path.expanduser("~"), ".claude", "projects", "*", "*.jsonl")
    if v := os.environ.get("SCRAPER_SESSION_GLOB", ""):
        session_glob = v

    dry_run = os.environ.get("SCRAPER_DRY_RUN", "") in ("true", "1")

    telemetry_url = _telemetry_url()
    data_dir = _data_dir()
    cursor_path = os.path.join(data_dir, "scraper-cursors.json")

    host = os.environ.get("TEAMSTER_HOST", "")
    if not host:
        try:
            host = socket.gethostname().split(".")[0]
        except Exception:
            host = "localhost"

    username = os.environ.get("TEAMSTER_USER", "")
    if not username:
        try:
            import getpass
            username = getpass.getuser()
        except Exception:
            username = ""

    logging.info("token-scraper %s starting daemon=%s poll_interval=%ds "
                 "glob=%s telemetry_url=%s dry_run=%s",
                 __version__, args.daemon, poll_interval,
                 session_glob, telemetry_url, dry_run)

    scraper = Scraper(
        telemetry_url=telemetry_url,
        host=host,
        username=username,
        session_glob=session_glob,
        cursor_path=cursor_path,
        dry_run=dry_run,
        data_dir=data_dir,
    )
    scraper.load_cursors()

    stop = _StopEvent()

    def _handle_signal(signum, frame):
        logging.info("shutting down (signal %d)", signum)
        stop.set()

    signal.signal(signal.SIGINT, _handle_signal)
    signal.signal(signal.SIGTERM, _handle_signal)

    if not args.daemon:
        # Single-poll mode: process new bytes, save cursors, exit 0.
        try:
            scraper.poll(stop_event=stop)
        except Exception as exc:
            _log_error("poll error", error=str(exc))
            logging.error("poll error: %s", exc)
        return 0

    # Daemon mode: poll loop until signal.
    while not stop.is_set():
        try:
            scraper.poll(stop_event=stop)
        except Exception as exc:
            _log_error("poll error", error=str(exc))
            logging.error("poll error: %s", exc)

        # Sleep in small increments so SIGTERM is handled promptly.
        deadline = time.monotonic() + poll_interval
        while not stop.is_set() and time.monotonic() < deadline:
            time.sleep(min(1.0, deadline - time.monotonic()))

    logging.info("token-scraper stopped")
    return 0


if __name__ == "__main__":
    sys.exit(main())

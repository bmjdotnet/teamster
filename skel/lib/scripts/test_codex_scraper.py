#!/usr/bin/env python3
"""Unit tests for codex-scraper.py.

Pure-stdlib (unittest), synthetic fixtures only — never raw rollout files
(they carry operator content and codex base_instructions). Ports the test
ASSERTIONS of src/cmd/codex-scraper/scraper_test.go, the Go reference
implementation's acceptance tests (LESSONS.md §2/§3 are the binding spec).

Run with: python3 test_codex_scraper.py
"""
from __future__ import annotations

import json
import os
import shutil
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

import importlib.util

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "codex_scraper", os.path.join(_HERE, "codex-scraper.py"))
codex_scraper = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(codex_scraper)


# ---------------------------------------------------------------------------
# Capture server — stands in for hookd's /telemetry and /session endpoints,
# recording every row POSTed so a test can assert what the tailer derived.
# ---------------------------------------------------------------------------

class _CaptureServer:
    def __init__(self):
        self.telemetry_rows = []
        self.session_calls = []
        self.rates_payload = None  # dict served at GET /rates; None -> 500
        self.rates_requests = []
        handler = self._make_handler()
        self.httpd = HTTPServer(("127.0.0.1", 0), handler)
        self.port = self.httpd.server_port
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()

    def _make_handler(self):
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass  # silence

            def do_GET(self):
                if self.path.split("?")[0] != "/rates":
                    self.send_response(404)
                    self.end_headers()
                    return
                outer.rates_requests.append(self.path)
                if outer.rates_payload is None:
                    self.send_response(500)
                    self.end_headers()
                    return
                body = json.dumps(outer.rates_payload).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                raw = self.rfile.read(length) if length else b""
                try:
                    row = json.loads(raw) if raw else {}
                except Exception:
                    self.send_response(400)
                    self.end_headers()
                    return
                if self.path == "/telemetry":
                    outer.telemetry_rows.append(row)
                    self.send_response(202)
                elif self.path == "/session":
                    outer.session_calls.append(row)
                    self.send_response(200)
                else:
                    self.send_response(404)
                self.end_headers()

        return Handler

    @property
    def telemetry_url(self):
        return f"http://127.0.0.1:{self.port}/telemetry"

    @property
    def rates_url(self):
        return f"http://127.0.0.1:{self.port}/rates?runtime=codex"

    @property
    def session_url(self):
        return f"http://127.0.0.1:{self.port}/session"

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


class _FailingCaptureServer(_CaptureServer):
    """Like _CaptureServer, but /telemetry always 500s (simulates a POST
    failure so tests can assert the cursor does not advance past it)."""

    def _make_handler(self):
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                self.rfile.read(length) if length else b""
                self.send_response(500)
                self.end_headers()

        return Handler


def _new_test_scraper(server, dry_run=False, rates=None):
    tmpdir = tempfile.mkdtemp()
    cursor_path = os.path.join(tmpdir, "cursors.json")
    scraper = codex_scraper.Scraper(
        telemetry_url=server.telemetry_url,
        session_url=server.session_url,
        host="testhost",
        username="testuser",
        roots=[],
        cursor_path=cursor_path,
        dry_run=dry_run,
        rates=rates,
    )
    return scraper, tmpdir


# ---------------------------------------------------------------------------
# Fixture builders — hand-written, zero model tokens. Mirror the Go test
# file's writeLines/sessionMetaLine/subagentSessionMetaLine/turnContextLine/
# tokenCountLine builders exactly.
# ---------------------------------------------------------------------------

def write_lines(path, lines):
    with open(path, "w") as f:
        for line in lines:
            f.write(line + "\n")


def session_meta_line(id_, cwd, originator, cli_version):
    """Top-level (non-subagent) session_meta — deliberately with NO
    session_id field, matching both a real 0.137.0 file (never has one) and
    being harmless on 0.142.x (where a top-level file's session_id equals
    its own id, so the id-fallback produces the same result)."""
    return json.dumps({
        "timestamp": "2026-07-07T00:00:00.000Z",
        "type": "session_meta",
        "payload": {
            "id": id_,
            "cwd": cwd,
            "originator": originator,
            "cli_version": cli_version,
        },
    })


def subagent_session_meta_line(id_, session_id, parent_thread_id, cwd,
                                originator, cli_version, role, nickname):
    """A thread_spawn subagent's session_meta: id is the file's OWN thread
    id; session_id/parent_thread_id are the parent's id (both set — matches
    real evidence); role/nickname are the subagent's identity."""
    return json.dumps({
        "timestamp": "2026-07-07T00:00:00.000Z",
        "type": "session_meta",
        "payload": {
            "id": id_,
            "session_id": session_id,
            "parent_thread_id": parent_thread_id,
            "cwd": cwd,
            "originator": originator,
            "cli_version": cli_version,
            "agent_role": role,
            "agent_nickname": nickname,
        },
    })


def turn_context_line(model):
    return json.dumps({
        "timestamp": "2026-07-07T00:00:01.000Z",
        "type": "turn_context",
        "payload": {"model": model},
    })


def token_count_line(input_, output, cached_input, reasoning_output):
    """cached_input/reasoning_output are SUBSETS of input/output (matching
    real Codex semantics), not additional tokens on top."""
    def usage(mult):
        return {
            "input_tokens": input_ * mult,
            "output_tokens": output * mult,
            "cached_input_tokens": cached_input * mult,
            "reasoning_output_tokens": reasoning_output * mult,
            "total_tokens": (input_ + output) * mult,
        }

    return json.dumps({
        "timestamp": "2026-07-07T00:00:02.000Z",
        "type": "event_msg",
        "payload": {
            "type": "token_count",
            "info": {
                "total_token_usage": usage(100),  # cumulative, always ignored
                "last_token_usage": usage(1),
            },
        },
    })


def token_usage_record_line(input_, output, cached_input, reasoning_output,
                            timestamp="2026-10-01T19:47:36.748Z"):
    """Codex 0.159.x top-level token_usage_record. payload.usage has the same
    shape as last_token_usage (plus cache_write_input_tokens); the
    turn/thread cumulative blocks are present but must always be ignored."""
    def usage(mult):
        return {
            "input_tokens": input_ * mult,
            "cached_input_tokens": cached_input * mult,
            "cache_write_input_tokens": 0,
            "output_tokens": output * mult,
            "reasoning_output_tokens": reasoning_output * mult,
            "total_tokens": (input_ + output) * mult,
        }

    return json.dumps({
        "timestamp": timestamp,
        "type": "token_usage_record",
        "payload": {
            "thread_id": "thread-synthetic",
            "turn_id": "turn-synthetic",
            "session_id": "session-synthetic",
            "usage": usage(1),
            "turn_token_usage": usage(10),  # cumulative, always ignored
            "thread_token_usage": usage(100),  # cumulative, always ignored
        },
    })


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

class TestSubagentThreadBooksUnderParentSession(unittest.TestCase):
    """Port of TestProcessFile_SubagentThreadBooksUnderParentSession: the
    regression test for the subagent cost-attribution bug. A thread_spawn
    subagent's rollout file has session_meta.id == its own thread id, but
    session_meta.session_id == the PARENT's id. Ledger rows and the sessions
    upsert must use the PARENT id so rollup's temporal_join can bridge
    subagent spend to the parent's focus intervals."""

    def setUp(self):
        self.server = _CaptureServer()
        self.scraper, self.tmpdir = _new_test_scraper(self.server)

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_thread_to_parent_booking(self):
        parent_id = "019f3ed6-1354-7731-b35e-5356dd9af6d4"
        thread_id = "019f3ed8-17a7-7dc3-b11a-6cca251d9c86"
        path = os.path.join(self.tmpdir, "rollout-subagent.jsonl")
        write_lines(path, [
            subagent_session_meta_line(thread_id, parent_id, parent_id, "/tmp",
                                       "codex_exec", "0.142.5", "explorer", "Mencius"),
            turn_context_line("gpt-5.4"),
            token_count_line(100, 10, 0, 0),
        ])

        self.scraper.process_file(path)

        rows = self.server.telemetry_rows
        self.assertEqual(1, len(rows))
        row = rows[0]
        self.assertEqual(parent_id, row["session_id"],
                         "ledger session_id must be the parent id, not the thread's own id")
        self.assertEqual(f"codex:{thread_id}:000000", row["message_id"],
                         "message_id must be keyed by thread id, not session_id")
        self.assertEqual("@Mencius", row["agent_name"],
                         "agent_name must be nickname-first (@Mencius), not role (@explorer)")

        calls = self.server.session_calls
        self.assertEqual(1, len(calls))
        self.assertEqual(parent_id, calls[0]["session_id"])
        self.assertEqual("@Mencius", calls[0]["agent_name"])
        self.assertEqual("subagent", calls[0]["relationship"])
        self.assertEqual(thread_id, calls[0]["agent_id"])


# Verbatim first line of a real Codex 0.160.0 subagent rollout (base_instructions
# and creator ids redacted) — DESIGN.md D2 fixture session_meta_01a108e6.json.
_REAL_SUBAGENT_META_0160 = (
    '{"timestamp":"2026-10-04T21:51:32.368Z","type":"session_meta","payload":'
    '{"creator_user_id":"user-REDACTED","creator_account_id":"REDACTED",'
    '"session_id":"01a108dd-60b4-7722-b2d1-36ee22189a03",'
    '"id":"01a108e6-9810-7cd1-a1a2-2d7d1eee6a0b",'
    '"parent_thread_id":"01a108dd-60b4-7722-b2d1-36ee22189a03",'
    '"timestamp":"2026-10-04T21:51:32.368Z","cwd":"/home/claude/teamster",'
    '"runtime_workspace_roots":["/home/claude/teamster"],"originator":"codex-tui",'
    '"cli_version":"0.160.0","source":{"subagent":{"thread_spawn":'
    '{"parent_thread_id":"01a108dd-60b4-7722-b2d1-36ee22189a03","depth":1,'
    '"agent_path":"/root/review_data_analysis","agent_nickname":"Avicenna",'
    '"agent_role":null}}},"thread_source":"subagent","agent_nickname":"Avicenna",'
    '"agent_path":"/root/review_data_analysis","model_provider":"openai",'
    '"base_instructions":"<redacted>","history_mode":"paginated",'
    '"multi_agent_version":"v2","context_window":'
    '{"window_id":"01a108e6-9810-7cd1-a1a2-2d80da665f51"}}}')


class TestSubagentIdentityD2(unittest.TestCase):
    """DESIGN.md D1/D2/D4 (caller side): nickname-first agent_name, whitespace
    collapsed, relationship/agent_id sent for subagent files only."""

    PARENT = "01a108dd-60b4-7722-b2d1-36ee22189a03"
    THREAD = "01a108e6-9810-7cd1-a1a2-2d7d1eee6a0b"

    def setUp(self):
        self.server = _CaptureServer()
        self.scraper, self.tmpdir = _new_test_scraper(self.server)

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _run(self, meta_line):
        path = os.path.join(self.tmpdir, "rollout-x.jsonl")
        write_lines(path, [meta_line, turn_context_line("gpt-5.4"),
                           token_count_line(100, 10, 0, 0)])
        self.scraper.process_file(path)
        self.assertEqual(1, len(self.server.session_calls))
        return self.server.session_calls[0]

    def _meta(self, payload):
        return json.dumps({"timestamp": "2026-10-04T00:00:00.000Z",
                           "type": "session_meta", "payload": payload})

    def test_real_0160_fixture(self):
        call = self._run(_REAL_SUBAGENT_META_0160)
        self.assertEqual(self.PARENT, call["session_id"])
        self.assertEqual("@Avicenna", call["agent_name"])
        self.assertEqual("subagent", call["relationship"])
        self.assertEqual(self.THREAD, call["agent_id"])
        row = self.server.telemetry_rows[0]
        self.assertEqual(self.PARENT, row["session_id"])
        self.assertEqual(f"codex:{self.THREAD}:000000", row["message_id"])
        self.assertEqual("@Avicenna", row["agent_name"])

    def _sub(self, **extra):
        p = {"id": self.THREAD, "session_id": self.PARENT,
             "parent_thread_id": self.PARENT, "cwd": "/tmp",
             "originator": "codex-tui", "cli_version": "0.160.0",
             "thread_source": "subagent"}
        p.update(extra)
        return self._meta(p)

    def test_nickname_beats_role(self):
        call = self._run(self._sub(agent_role="worker", agent_nickname="Dirac"))
        self.assertEqual("@Dirac", call["agent_name"])

    def test_whitespace_collapsed(self):
        call = self._run(self._sub(agent_nickname="Avicenna the 2nd"))
        self.assertEqual("@Avicenna-the-2nd", call["agent_name"])

    def test_subagent_name_ascii_whitespace_rule(self):
        f = codex_scraper._subagent_name
        tid = self.THREAD
        self.assertEqual("@" + tid[:8], f(" ", None, tid))
        self.assertEqual("@rev", f(" ", "rev", tid))
        self.assertEqual("@\x1cA\x1cB", f("\x1cA\x1cB", None, tid))
        self.assertEqual("@A\u00a0B", f("A\u00a0B", None, tid))
        self.assertEqual("@A-B", f("A  B", None, tid))
        self.assertEqual("@A-B", f("A\t\tB", None, tid))
        self.assertEqual("@Avicenna-the-2nd", f("Avicenna the 2nd", None, tid))

    def test_role_when_no_nickname(self):
        call = self._run(self._sub(agent_role="reviewer"))
        self.assertEqual("@reviewer", call["agent_name"])

    def test_id_prefix_when_neither(self):
        call = self._run(self._sub())
        self.assertEqual("@" + self.THREAD[:8], call["agent_name"])
        self.assertEqual("subagent", call["relationship"])

    def test_thread_source_alone_marks_subagent(self):
        p = {"id": self.THREAD, "session_id": self.PARENT, "cwd": "/tmp",
             "originator": "codex-tui", "cli_version": "0.160.0",
             "thread_source": "subagent", "agent_nickname": "Solo"}
        call = self._run(self._meta(p))
        self.assertEqual("@Solo", call["agent_name"])
        self.assertEqual("subagent", call["relationship"])

    def test_root_thread_has_no_identity_fields(self):
        p = {"id": self.PARENT, "session_id": self.PARENT, "cwd": "/tmp",
             "originator": "codex-tui", "cli_version": "0.160.0",
             "thread_source": "user"}
        call = self._run(self._meta(p))
        self.assertEqual("", call["agent_name"])
        self.assertNotIn("relationship", call)
        self.assertNotIn("agent_id", call)


class TestPreThreadSpawnSessionMetaFallsBackToID(unittest.TestCase):
    """Port of TestProcessFile_PreThreadSpawnSessionMetaFallsBackToID:
    0.137.0-shaped rollout files have session_meta with NO session_id field
    at all. session_id must fall back to the file's own id."""

    def setUp(self):
        self.server = _CaptureServer()
        self.scraper, self.tmpdir = _new_test_scraper(self.server)

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_no_session_id_fallback(self):
        id_ = "019f0000-0000-7000-8000-000000000001"
        path = os.path.join(self.tmpdir, "rollout-pre-0142.jsonl")
        write_lines(path, [
            session_meta_line(id_, "/tmp", "codex_exec", "0.137.0"),
            turn_context_line("gpt-5.5"),
            token_count_line(100, 10, 0, 0),
        ])

        self.scraper.process_file(path)

        rows = self.server.telemetry_rows
        self.assertEqual(1, len(rows))
        self.assertEqual(id_, rows[0]["session_id"], "session_id must fall back to file id")
        self.assertEqual(f"codex:{id_}:000000", rows[0]["message_id"])
        self.assertEqual("", rows[0]["agent_name"],
                         "agent_name must be empty (no agent_role on a non-subagent file)")

        calls = self.server.session_calls
        self.assertEqual("", calls[0]["agent_name"])
        self.assertNotIn("relationship", calls[0])
        self.assertNotIn("agent_id", calls[0])


class TestParentAndSubagentNoMessageIDCollision(unittest.TestCase):
    """Port of TestProcessFile_ParentAndSubagentNoMessageIDCollision: a
    parent file and its subagent file share the same session_id (the
    parent's id) but each has its own independent seq counter. If message_id
    were derived from session_id instead of thread_id, both seq-0 rows would
    collide onto the same key and hookd's uq_message upsert would silently
    swallow one of them."""

    def setUp(self):
        self.server = _CaptureServer()
        self.scraper, self.tmpdir = _new_test_scraper(self.server)

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_no_collision(self):
        parent_id = "019f3ed6-1354-7731-b35e-5356dd9af6d4"
        thread_id = "019f3ed8-1843-7d61-992b-f7a012bfa313"

        parent_path = os.path.join(self.tmpdir, "rollout-parent.jsonl")
        write_lines(parent_path, [
            session_meta_line(parent_id, "/tmp/test-workspace", "codex-tui", "0.142.5"),
            turn_context_line("gpt-5.4"),
            token_count_line(100, 10, 0, 0),
        ])
        subagent_path = os.path.join(self.tmpdir, "rollout-subagent.jsonl")
        write_lines(subagent_path, [
            subagent_session_meta_line(thread_id, parent_id, parent_id, "/tmp/test-workspace",
                                       "codex_exec", "0.142.5", "worker", "Dirac"),
            turn_context_line("gpt-5.4"),
            token_count_line(200, 20, 0, 0),
        ])

        self.scraper.process_file(parent_path)
        self.scraper.process_file(subagent_path)

        rows = self.server.telemetry_rows
        self.assertEqual(2, len(rows))
        seen = set()
        for row in rows:
            self.assertEqual(parent_id, row["session_id"])
            self.assertNotIn(row["message_id"], seen, "message_id collision across files")
            seen.add(row["message_id"])

        calls = self.server.session_calls
        self.assertEqual(2, len(calls))
        by_agent = {c["agent_name"]: c for c in calls}
        self.assertIn("", by_agent)
        self.assertEqual(parent_id, by_agent[""]["session_id"])
        self.assertIn("@Dirac", by_agent)
        self.assertEqual(parent_id, by_agent["@Dirac"]["session_id"])
        self.assertNotIn("relationship", by_agent[""])
        self.assertNotIn("agent_id", by_agent[""])


class TestEmitLedgerRowCachedAndReasoningAreSubsets(unittest.TestCase):
    """Port of TestEmitLedgerRow_CachedAndReasoningAreSubsets: the
    regression test for the double-counting bug where cached_input_tokens
    and reasoning_output_tokens were wrongly treated as additional tokens on
    top of input_tokens/output_tokens instead of subsets already counted
    inside them."""

    def setUp(self):
        self.server = _CaptureServer()
        self.scraper, self.tmpdir = _new_test_scraper(self.server)

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_subset_semantics(self):
        # input=1000, cached_input=400 (subset), output=200,
        # reasoning_output=50 (subset). total_tokens = 1000+200=1200.
        path = os.path.join(self.tmpdir, "rollout-subset.jsonl")
        write_lines(path, [
            session_meta_line("sess-subset", "/tmp", "codex_exec", "0.137.0"),
            turn_context_line("gpt-5.5"),
            token_count_line(1000, 200, 400, 50),
        ])

        self.scraper.process_file(path)

        rows = self.server.telemetry_rows
        self.assertEqual(1, len(rows))
        row = rows[0]
        self.assertEqual(600, row["input_tokens"], "input_tokens - cached_input_tokens")
        self.assertEqual(400, row["cache_read_tokens"])
        self.assertEqual(200, row["output_tokens"], "output as-is, NOT +reasoning")
        self.assertEqual(50, row["reasoning_output_tokens"])


class TestIgnoresCodexAutoReviewModelSentinel(unittest.TestCase):
    """Port of TestProcessLine_IgnoresCodexAutoReviewModelSentinel (upstream
    bug openai/codex#20981): the sentinel model string must not overwrite
    the last real model seen."""

    def setUp(self):
        self.server = _CaptureServer()
        self.scraper, self.tmpdir = _new_test_scraper(self.server)

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_sentinel_ignored(self):
        path = os.path.join(self.tmpdir, "rollout-sentinel.jsonl")
        write_lines(path, [
            session_meta_line("sess-sentinel", "/tmp", "codex_exec", "0.137.0"),
            turn_context_line("gpt-5.5"),
            token_count_line(100, 10, 0, 0),
            turn_context_line("codex-auto-review"),  # must be ignored
            token_count_line(50, 5, 0, 0),
        ])

        self.scraper.process_file(path)

        rows = self.server.telemetry_rows
        self.assertEqual(2, len(rows))
        for row in rows:
            self.assertEqual("gpt-5.5", row["model"],
                             "codex-auto-review sentinel must not overwrite the model")


class TestResumedRolloutDedup(unittest.TestCase):
    """Synthetic equivalent of TestProcessFile_ResumedRollout (the Go test
    uses a raw rollout fixture we may not port verbatim — operator content).
    Verifies: multiple token_count events in one file each derive from their
    own last_token_usage (never cumulative total_token_usage), get distinct
    message_ids, and the file's session identity is upserted exactly once."""

    def setUp(self):
        self.server = _CaptureServer()
        self.scraper, self.tmpdir = _new_test_scraper(self.server)

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_multiple_turns_one_file(self):
        session_id = "019f3b4a-3808-7fa3-bc1d-e99cdc0f1f4e"
        path = os.path.join(self.tmpdir, "rollout-resumed.jsonl")
        write_lines(path, [
            session_meta_line(session_id, "/tmp/test-workspace", "codex_exec", "0.142.5"),
            turn_context_line("gpt-5.5"),
            token_count_line(23215, 36, 2432, 0),
            token_count_line(23300, 5, 22912, 0),
            token_count_line(23318, 5, 22912, 0),
        ])

        self.scraper.process_file(path)

        rows = self.server.telemetry_rows
        self.assertEqual(3, len(rows), "one ledger row per token_count event")
        wants = [
            (23215 - 2432, 36, 2432),
            (23300 - 22912, 5, 22912),
            (23318 - 22912, 5, 22912),
        ]
        seen_ids = set()
        for i, row in enumerate(rows):
            self.assertEqual(session_id, row["session_id"])
            self.assertEqual("codex", row["runtime"])
            self.assertEqual("gpt-5.5", row["model"])
            want_in, want_out, want_cr = wants[i]
            self.assertEqual(want_in, row["input_tokens"])
            self.assertEqual(want_out, row["output_tokens"])
            self.assertEqual(want_cr, row["cache_read_tokens"])
            self.assertNotIn(row["message_id"], seen_ids, "duplicate message_id")
            seen_ids.add(row["message_id"])

        self.assertEqual(1, len(self.server.session_calls),
                         "exactly one session upsert per file scan")
        sess = self.server.session_calls[0]
        self.assertEqual(session_id, sess["session_id"])
        self.assertEqual("/tmp/test-workspace", sess["cwd"])
        self.assertEqual("codex_exec", sess["originator"])
        self.assertEqual("codex", sess["runtime"])
        self.assertEqual("gpt-5.5", sess["model"])


class TestArchiveRescanIdempotent(unittest.TestCase):
    """Port of TestProcessFile_ArchiveRescanIdempotent: simulates `codex
    archive` moving a rollout file to a new path (losing the path-keyed
    cursor, forcing a full re-scan from byte 0). Derived message_ids must be
    identical to a first-time scan of the same content."""

    def test_rescan_reproduces_message_ids(self):
        content_lines = [
            session_meta_line("sess-archive", "/tmp", "codex_exec", "0.137.0"),
            turn_context_line("gpt-5.5"),
            token_count_line(100, 10, 5, 0),
        ]

        server1 = _CaptureServer()
        scraper1, tmpdir1 = _new_test_scraper(server1)
        path1 = os.path.join(tmpdir1, "rollout.jsonl")
        write_lines(path1, content_lines)
        scraper1.process_file(path1)

        server2 = _CaptureServer()
        scraper2, tmpdir2 = _new_test_scraper(server2)
        path2 = os.path.join(tmpdir2, "rollout.jsonl")  # different path, same content
        write_lines(path2, content_lines)
        scraper2.process_file(path2)

        try:
            self.assertEqual(len(server1.telemetry_rows), len(server2.telemetry_rows))
            for r1, r2 in zip(server1.telemetry_rows, server2.telemetry_rows):
                self.assertEqual(r1["message_id"], r2["message_id"],
                                 "a post-archive rescan must reproduce identical message_ids")
        finally:
            server1.close()
            server2.close()
            shutil.rmtree(tmpdir1, ignore_errors=True)
            shutil.rmtree(tmpdir2, ignore_errors=True)


class TestTruncated(unittest.TestCase):
    """Port of TestProcessFile_Truncated: if the file on disk is smaller than
    the persisted cursor offset, the cursor resets to zero rather than
    seeking past EOF."""

    def setUp(self):
        self.server = _CaptureServer()
        self.scraper, self.tmpdir = _new_test_scraper(self.server)

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_truncation_resets_cursor(self):
        path = os.path.join(self.tmpdir, "rollout-trunc.jsonl")
        write_lines(path, [
            session_meta_line("sess-trunc", "/tmp", "codex_exec", "0.137.0"),
            turn_context_line("gpt-5.5"),
            token_count_line(100, 10, 5, 0),
        ])
        self.scraper.process_file(path)
        self.assertEqual(1, len(self.server.telemetry_rows))

        self.server.telemetry_rows.clear()
        write_lines(path, [
            session_meta_line("sess-trunc-2", "/tmp2", "codex_exec", "0.137.0"),
            turn_context_line("gpt-5.5"),
            token_count_line(50, 5, 0, 0),
        ])
        self.scraper.process_file(path)

        self.assertEqual(1, len(self.server.telemetry_rows))
        self.assertEqual("sess-trunc-2", self.server.telemetry_rows[0]["session_id"],
                         "cursor did not reset on truncation")


class TestVanished(unittest.TestCase):
    """Port of TestProcessFile_Vanished: a missing file is a silent no-op,
    not an error."""

    def test_vanished_file_is_noop(self):
        server = _CaptureServer()
        scraper, tmpdir = _new_test_scraper(server)
        try:
            scraper.process_file("/nonexistent/path/rollout.jsonl")  # must not raise
        finally:
            server.close()
            shutil.rmtree(tmpdir, ignore_errors=True)


class TestPartialTrailingLine(unittest.TestCase):
    """Port of TestProcessFile_PartialTrailingLine: the tailer never commits
    a line with no trailing newline yet (Codex may still be mid-write) — the
    cursor must not advance past it."""

    def setUp(self):
        self.server = _CaptureServer()
        self.scraper, self.tmpdir = _new_test_scraper(self.server)

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def test_partial_line_not_committed(self):
        path = os.path.join(self.tmpdir, "rollout-partial.jsonl")
        full = (session_meta_line("sess-partial", "/tmp", "codex_exec", "0.137.0") + "\n"
                + turn_context_line("gpt-5.5") + "\n"
                + token_count_line(10, 1, 0, 0))  # no trailing newline

        with open(path, "w") as f:
            f.write(full)
        self.scraper.process_file(path)
        self.assertEqual(0, len(self.server.telemetry_rows),
                         "must not emit while the token_count line lacks a trailing newline")

        with open(path, "w") as f:
            f.write(full + "\n")
        self.scraper.process_file(path)
        self.assertEqual(1, len(self.server.telemetry_rows),
                         "must pick up the previously-uncommitted line once complete")


class TestPostFailureDoesNotAdvanceCursor(unittest.TestCase):
    """No direct Go analogue by name, but exercises the same contract
    errPostFailed protects: a telemetry POST failure must not advance the
    cursor past the unsent row, so it is retried on the next poll."""

    def test_failed_post_retried(self):
        failing = _FailingCaptureServer()
        scraper, tmpdir = _new_test_scraper(failing)
        path = os.path.join(tmpdir, "rollout.jsonl")
        write_lines(path, [
            session_meta_line("sess-fail", "/tmp", "codex_exec", "0.137.0"),
            turn_context_line("gpt-5.5"),
            token_count_line(100, 10, 0, 0),
        ])
        try:
            scraper.process_file(path)  # POST fails; must not raise out of process_file
            cursor = scraper.cursors[path]
            self.assertEqual(0, cursor["seq"], "seq must not be consumed for an unsent row")
        finally:
            failing.close()
            shutil.rmtree(tmpdir, ignore_errors=True)


class _ScraperTestCase(unittest.TestCase):
    def setUp(self):
        self.server = _CaptureServer()
        self.scraper, self.tmpdir = _new_test_scraper(self.server)

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)


class TestTokenUsageRecordOnly(_ScraperTestCase):
    """A file with ONLY token_usage_record events (a future Codex that drops
    the legacy token_count) must ledger one row per record."""

    def test_record_only_file(self):
        thread_id = "019f0000-0000-7000-8000-0000000000aa"
        path = os.path.join(self.tmpdir, "rollout-record-only.jsonl")
        write_lines(path, [
            session_meta_line(thread_id, "/tmp", "codex_exec", "0.159.3"),
            turn_context_line("gpt-6-luna"),
            token_usage_record_line(20389, 166, 11008, 59),
            token_usage_record_line(23731, 73, 20224, 7),
        ])

        self.scraper.process_file(path)

        rows = self.server.telemetry_rows
        self.assertEqual(2, len(rows))
        first = rows[0]
        self.assertEqual(f"codex:{thread_id}:000000", first["message_id"])
        self.assertEqual(f"codex:{thread_id}:000001", rows[1]["message_id"])
        self.assertEqual(thread_id, first["session_id"])
        self.assertEqual("codex", first["runtime"])
        self.assertEqual("gpt-6-luna", first["model"])
        self.assertEqual(20389 - 11008, first["input_tokens"],
                         "input_tokens - cached_input_tokens")
        self.assertEqual(11008, first["cache_read_tokens"])
        self.assertEqual(166, first["output_tokens"], "output as-is, NOT +reasoning")
        self.assertEqual(59, first["reasoning_output_tokens"])
        self.assertEqual(0, first["cache_write_tokens"])
        self.assertEqual("2026-10-01T19:47:36.748000000Z", first["timestamp"],
                         "timestamp must be the record's own event time")
        want_cost = (20389 - 11008) * 0.0000001 + 166 * 0.0000005 + 11008 * 0.00000001
        self.assertGreater(first["cost_usd"], 0.0, "gpt-6-luna must not price at $0")
        self.assertAlmostEqual(want_cost, first["cost_usd"])

    def test_record_without_usage_is_skipped(self):
        path = os.path.join(self.tmpdir, "rollout-empty-record.jsonl")
        empty = json.dumps({"timestamp": "2026-10-01T19:47:36.748Z",
                            "type": "token_usage_record", "payload": {}})
        write_lines(path, [
            session_meta_line("sess-empty", "/tmp", "codex_exec", "0.159.3"),
            turn_context_line("gpt-6-luna"),
            empty,
            token_count_line(100, 10, 0, 0),
        ])

        self.scraper.process_file(path)

        self.assertEqual(1, len(self.server.telemetry_rows),
                         "an empty record is not usage; the token_count must still be ledgered")
        self.assertFalse(self.scraper.cursors[path]["usage_record_seen"])


class TestTokenUsageRecordDedup(_ScraperTestCase):
    """Codex 0.159.3 writes BOTH token_usage_record and event_msg:token_count
    for each turn. Exactly one ledger row per turn must result."""

    def test_both_formats_ledger_once_per_turn(self):
        thread_id = "019f0000-0000-7000-8000-0000000000bb"
        path = os.path.join(self.tmpdir, "rollout-both.jsonl")
        write_lines(path, [
            session_meta_line(thread_id, "/tmp", "codex_exec", "0.159.3"),
            turn_context_line("gpt-6-luna"),
            token_usage_record_line(20389, 166, 11008, 59),
            token_count_line(20389, 166, 11008, 59),
            token_usage_record_line(23731, 73, 20224, 7),
            token_count_line(23731, 73, 20224, 7),
        ])

        self.scraper.process_file(path)

        rows = self.server.telemetry_rows
        self.assertEqual(2, len(rows), "4 usage events for 2 turns must ledger 2 rows")
        self.assertEqual([f"codex:{thread_id}:000000", f"codex:{thread_id}:000001"],
                         [r["message_id"] for r in rows])
        self.assertEqual([166, 73], [r["output_tokens"] for r in rows])

    def test_legacy_only_file_unchanged(self):
        path = os.path.join(self.tmpdir, "rollout-legacy.jsonl")
        write_lines(path, [
            session_meta_line("sess-legacy", "/tmp", "codex_exec", "0.142.5"),
            turn_context_line("gpt-5.5"),
            token_count_line(100, 10, 0, 0),
            token_count_line(200, 20, 0, 0),
        ])

        self.scraper.process_file(path)

        self.assertEqual(2, len(self.server.telemetry_rows))
        self.assertFalse(self.scraper.cursors[path]["usage_record_seen"])

    def test_resumed_file_old_turns_then_new_format(self):
        # A rollout started on an old Codex, resumed on 0.159.x: the old turns
        # are token_count-only and must all be kept; only the new-format
        # turns' token_counts are duplicates.
        path = os.path.join(self.tmpdir, "rollout-resumed-upgrade.jsonl")
        write_lines(path, [
            session_meta_line("sess-upgrade", "/tmp", "codex_exec", "0.142.5"),
            turn_context_line("gpt-6-luna"),
            token_count_line(100, 10, 0, 0),
            token_count_line(200, 20, 0, 0),
            token_usage_record_line(300, 30, 0, 0),
            token_count_line(300, 30, 0, 0),
        ])

        self.scraper.process_file(path)

        rows = self.server.telemetry_rows
        self.assertEqual(3, len(rows))
        self.assertEqual([10, 20, 30], [r["output_tokens"] for r in rows])

    def test_poll_boundary_between_record_and_its_token_count(self):
        # Real rollouts show the paired token_count landing minutes after its
        # record, so a poll can read the record in one run and the
        # token_count in the next. Each cron run is a fresh process: carry
        # the state through the persisted cursor file, not in-memory.
        thread_id = "019f0000-0000-7000-8000-0000000000cc"
        path = os.path.join(self.tmpdir, "rollout-straddle.jsonl")
        write_lines(path, [
            session_meta_line(thread_id, "/tmp", "codex_exec", "0.159.3"),
            turn_context_line("gpt-6-luna"),
            token_usage_record_line(20389, 166, 11008, 59),
        ])
        self.scraper.roots = [self.tmpdir]
        self.scraper.poll()
        self.assertEqual(1, len(self.server.telemetry_rows))

        with open(path, "a") as f:
            f.write(token_count_line(20389, 166, 11008, 59) + "\n")
            f.write(token_usage_record_line(23731, 73, 20224, 7) + "\n")
            f.write(token_count_line(23731, 73, 20224, 7) + "\n")

        second_run = codex_scraper.Scraper(
            telemetry_url=self.server.telemetry_url,
            session_url=self.server.session_url,
            host="testhost", username="testuser", roots=[self.tmpdir],
            cursor_path=self.scraper.cursor_path, dry_run=False)
        second_run.load_cursors()
        second_run.poll()

        rows = self.server.telemetry_rows
        self.assertEqual(2, len(rows),
                         "the straddling token_count must not be ledgered a second time")
        self.assertEqual([f"codex:{thread_id}:000000", f"codex:{thread_id}:000001"],
                         [r["message_id"] for r in rows])

    def test_flag_cleared_when_cursor_resets_on_truncation(self):
        path = os.path.join(self.tmpdir, "rollout-trunc-flag.jsonl")
        write_lines(path, [
            session_meta_line("sess-a", "/tmp", "codex_exec", "0.159.3"),
            turn_context_line("gpt-6-luna"),
            token_usage_record_line(100, 10, 0, 0),
            token_count_line(100, 10, 0, 0),
        ])
        self.scraper.process_file(path)
        self.assertTrue(self.scraper.cursors[path]["usage_record_seen"])

        self.server.telemetry_rows.clear()
        write_lines(path, [
            session_meta_line("sess-b", "/tmp", "codex_exec", "0.142.5"),
            turn_context_line("gpt-5.5"),
            token_count_line(50, 5, 0, 0),
        ])
        self.scraper.process_file(path)

        self.assertEqual(1, len(self.server.telemetry_rows),
                         "a reset cursor must forget the old file's format")


class TestMcpCallOK(unittest.TestCase):
    """Port of TestMcpCallOK."""

    def test_success(self):
        ok, matched = codex_scraper._mcp_call_ok(
            '{"Ok":{"content":[{"type":"text","text":"58 open outcomes"}]}}')
        self.assertTrue(ok)
        self.assertTrue(matched)

    def test_cancelled_or_denied(self):
        ok, matched = codex_scraper._mcp_call_ok('{"Err":"user cancelled MCP tool call"}')
        self.assertFalse(ok)
        self.assertTrue(matched)

    def test_empty(self):
        ok, matched = codex_scraper._mcp_call_ok("")
        self.assertFalse(ok)
        self.assertFalse(matched)

    def test_malformed(self):
        ok, matched = codex_scraper._mcp_call_ok("not json")
        self.assertFalse(ok)
        self.assertFalse(matched)


class TestDiscoverFiles(unittest.TestCase):
    """Port of TestDiscoverFiles / TestDiscoverFiles_MissingRoot."""

    def test_missing_root_skipped(self):
        tmpdir = tempfile.mkdtemp()
        try:
            existing = os.path.join(tmpdir, "sessions")
            os.makedirs(existing)
            write_lines(os.path.join(existing, "rollout-a.jsonl"),
                       [session_meta_line("a", "/tmp", "codex_exec", "0.137.0")])
            missing = os.path.join(tmpdir, "archived_sessions")  # never created

            server = _CaptureServer()
            scraper, _ = _new_test_scraper(server)
            scraper.roots = [existing, missing]
            try:
                files = scraper.discover_files()
                self.assertEqual(1, len(files))
            finally:
                server.close()
        finally:
            shutil.rmtree(tmpdir, ignore_errors=True)

    def test_discovers_both_roots_ignores_non_jsonl(self):
        tmpdir = tempfile.mkdtemp()
        try:
            sessions_dir = os.path.join(tmpdir, "sessions", "2026", "07", "07")
            archived_dir = os.path.join(tmpdir, "archived_sessions")
            os.makedirs(sessions_dir)
            os.makedirs(archived_dir)
            write_lines(os.path.join(sessions_dir, "rollout-a.jsonl"),
                       [session_meta_line("a", "/tmp", "codex_exec", "0.137.0")])
            write_lines(os.path.join(archived_dir, "rollout-b.jsonl"),
                       [session_meta_line("b", "/tmp", "codex_exec", "0.137.0")])
            with open(os.path.join(sessions_dir, "notes.txt"), "w") as f:
                f.write("hi")

            server = _CaptureServer()
            scraper, _ = _new_test_scraper(server)
            scraper.roots = [os.path.join(tmpdir, "sessions"), archived_dir]
            try:
                files = scraper.discover_files()
                self.assertEqual(2, len(files))
            finally:
                server.close()
        finally:
            shutil.rmtree(tmpdir, ignore_errors=True)


class TestPricing(unittest.TestCase):
    """Sanity checks for the codex-model pricing table (mirrors pricing.go)."""

    def test_known_model_exact(self):
        cost = codex_scraper.compute_cost("gpt-5.5", 1_000_000, 1_000_000, 0, 0)
        self.assertAlmostEqual(0.000005 * 1_000_000 + 0.00003 * 1_000_000, cost)

    def test_unknown_model_is_zero(self):
        self.assertEqual(0.0, codex_scraper.compute_cost("totally-unknown-model", 100, 100, 0, 0))

    def test_gpt_6_luna_priced(self):
        # $0.10 in / $0.50 out / $0.01 cache-read per million tokens.
        cost = codex_scraper.compute_cost("gpt-6-luna", 1_000_000, 1_000_000, 1_000_000, 0)
        self.assertAlmostEqual(0.10 + 0.50 + 0.01, cost)

    def test_gpt_6_luna_dated_variant_prefix_match(self):
        cost = codex_scraper.compute_cost("gpt-6-luna-2026-10-01", 1_000_000, 0, 0, 0)
        self.assertAlmostEqual(0.10, cost)

    def test_prefix_match(self):
        # A dated/suffixed variant of a known family should resolve via prefix match.
        cost = codex_scraper.compute_cost("gpt-5.4-mini-2026-01-01", 1000, 1000, 0, 0)
        self.assertGreater(cost, 0.0)


def _rate(id_, kind, key, inp, out, cr, cw5, variant="base", valid_to=None,
          runtime="codex", valid_from="1970-01-01T00:00:00Z"):
    return {"id": id_, "runtime": runtime, "match_kind": kind, "model_key": key,
            "variant": variant, "input_per_mtok": inp, "output_per_mtok": out,
            "cache_read_per_mtok": cr, "cache_write_5m_per_mtok": cw5,
            "cache_write_1h_per_mtok": "0.000000",
            "valid_from": valid_from, "valid_to": valid_to}


class TestHTTPRates(unittest.TestCase):
    def setUp(self):
        self.server = _CaptureServer()
        self.tmpdir = tempfile.mkdtemp()

    def tearDown(self):
        self.server.close()
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _rollout(self, model):
        path = os.path.join(self.tmpdir, "rollout-rates.jsonl")
        write_lines(path, [
            session_meta_line("sess-rates", "/tmp", "codex_exec", "0.137.0"),
            turn_context_line(model),
            token_count_line(1000, 200, 400, 50),
        ])
        return path

    def test_fetch_success_prices_from_rates_and_sends_rate_id(self):
        self.server.rates_payload = {"count": 1, "rates": [
            _rate(42, "exact", "gpt-5.5", "10.000000", "20.000000", "1.000000", "5.000000")]}
        rates = codex_scraper._fetch_rates(self.server.rates_url)
        self.assertEqual(1, len(rates))
        self.assertEqual(["/rates?runtime=codex"], self.server.rates_requests)
        scraper, _ = _new_test_scraper(self.server, rates=rates)
        scraper.process_file(self._rollout("gpt-5.5"))
        row = self.server.telemetry_rows[0]
        self.assertEqual(42, row["rate_id"])
        # uncached 600 in, 200 out, 400 cache read
        self.assertAlmostEqual((600 * 10 + 200 * 20 + 400 * 1) / 1e6, row["cost_usd"])

    def test_fetch_failure_falls_back_to_known_without_rate_id(self):
        self.server.rates_payload = None
        rates = codex_scraper._fetch_rates(self.server.rates_url)
        self.assertIsNone(rates)
        scraper, _ = _new_test_scraper(self.server, rates=rates)
        scraper.process_file(self._rollout("gpt-5.5"))
        row = self.server.telemetry_rows[0]
        self.assertEqual(-1, row["rate_id"])
        self.assertAlmostEqual(
            codex_scraper.compute_cost("gpt-5.5", 600, 200, 400, 0), row["cost_usd"])

    def test_unreachable_server_returns_none(self):
        self.assertIsNone(codex_scraper._fetch_rates("http://127.0.0.1:1/rates"))

    def test_unmatched_model_falls_back_to_known(self):
        rates = [_rate(7, "exact", "other-model", "1", "1", "1", "1")]
        scraper, _ = _new_test_scraper(self.server, rates=rates)
        scraper.process_file(self._rollout("gpt-5.5"))
        row = self.server.telemetry_rows[0]
        self.assertEqual(-1, row["rate_id"])
        self.assertGreater(row["cost_usd"], 0.0)

    def test_exact_beats_prefix_and_longest_prefix_wins(self):
        rates = [
            _rate(1, "prefix", "gpt-5", "1", "1", "1", "1"),
            _rate(2, "prefix", "gpt-5.5", "1", "1", "1", "1"),
            _rate(3, "exact", "gpt-5.5-x", "1", "1", "1", "1"),
        ]
        self.assertEqual(3, codex_scraper._resolve_rate(rates, "gpt-5.5-x")["id"])
        self.assertEqual(2, codex_scraper._resolve_rate(rates, "gpt-5.5-y")["id"])
        self.assertEqual(1, codex_scraper._resolve_rate(rates, "gpt-5.1")["id"])

    def test_resolver_skips_closed_variant_class_and_other_runtime(self):
        rates = [
            _rate(1, "exact", "m", "1", "1", "1", "1", valid_to="2026-01-01T00:00:00Z"),
            _rate(2, "exact", "m", "1", "1", "1", "1", variant="fast"),
            _rate(3, "class", "m", "1", "1", "1", "1"),
            _rate(4, "exact", "m", "1", "1", "1", "1", runtime="claude_code"),
        ]
        self.assertIsNone(codex_scraper._resolve_rate(rates, "m"))

    def test_future_valid_from_skipped(self):
        rates = [_rate(1, "exact", "m", "1", "1", "1", "1",
                       valid_from="2999-01-01T00:00:00Z")]
        self.assertIsNone(codex_scraper._resolve_rate(rates, "m"))

    def test_latest_valid_from_wins_among_open_rows(self):
        rates = [
            _rate(5, "exact", "m", "1", "1", "1", "1", valid_from="2026-01-01T00:00:00Z"),
            _rate(3, "exact", "m", "1", "1", "1", "1", valid_from="2026-06-01T00:00:00.123456Z"),
        ]
        self.assertEqual(3, codex_scraper._resolve_rate(rates, "m")["id"])

    def test_cache_write_priced_at_5m_rate(self):
        rate = _rate(9, "exact", "m", "0", "0", "0", "8.000000")
        cost, rid = codex_scraper._cost_from_rate(rate, 0, 0, 0, 1_000_000)
        self.assertAlmostEqual(8.0, cost)
        self.assertEqual(9, rid)


if __name__ == "__main__":
    unittest.main()

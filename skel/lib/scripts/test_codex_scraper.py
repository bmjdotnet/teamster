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


def _new_test_scraper(server, dry_run=False):
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
        self.assertEqual("@explorer", row["agent_name"],
                         "agent_name must be role-based (@explorer), not nickname (@Mencius)")

        calls = self.server.session_calls
        self.assertEqual(1, len(calls))
        self.assertEqual(parent_id, calls[0]["session_id"])
        self.assertEqual("@explorer", calls[0]["agent_name"])


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
        self.assertIn("@worker", by_agent)
        self.assertEqual(parent_id, by_agent["@worker"]["session_id"])


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

    def test_prefix_match(self):
        # A dated/suffixed variant of a known family should resolve via prefix match.
        cost = codex_scraper.compute_cost("gpt-5.4-mini-2026-01-01", 1000, 1000, 0, 0)
        self.assertGreater(cost, 0.0)


if __name__ == "__main__":
    unittest.main()

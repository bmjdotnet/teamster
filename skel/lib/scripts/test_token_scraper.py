#!/usr/bin/env python3
"""Unit tests for token-scraper.py's "[1m]" long-context suffix handling
(wu-1m-suffix).

Claude Code's API response (transcript message.model) never echoes back a
"[1m]" long-context-beta annotation — it only lives in
~/.claude/settings.json's "model" field. These tests prove the Go scraper's
fix (cmd/token-scraper/main.go, longcontext_test.go) is ported faithfully here
for remote/client-mode installs.

Run with: python3 test_token_scraper.py
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
    "token_scraper", os.path.join(_HERE, "token-scraper.py"))
token_scraper = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(token_scraper)


class _CaptureServer:
    """Stands in for hookd's /telemetry endpoint, recording every row POSTed."""

    def __init__(self):
        self.rows = []
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
                row = json.loads(raw) if raw else {}
                outer.rows.append(row)
                self.send_response(202)
                self.end_headers()

        return Handler

    def stop(self):
        self.httpd.shutdown()
        self.thread.join(timeout=2)


def _asst_line(uuid, msg_id, req_id, model, in_tok, out_tok):
    return {
        "type": "assistant",
        "uuid": uuid,
        "requestId": req_id,
        "sessionId": "sess-1",
        "timestamp": "2026-06-09T19:03:27.386Z",
        "message": {
            "id": msg_id,
            "model": model,
            "stop_reason": "tool_use",
            "content": [{"type": "text"}],
            "usage": {
                "input_tokens": in_tok,
                "output_tokens": out_tok,
                "cache_read_input_tokens": 0,
                "cache_creation_input_tokens": 0,
                "service_tier": "standard",
            },
        },
    }


class LongContextSuffixTest(unittest.TestCase):
    def setUp(self):
        self.cap = _CaptureServer()
        self.tmp = tempfile.mkdtemp()
        self.home = tempfile.mkdtemp()
        self._orig_home = os.environ.get("HOME")
        os.environ["HOME"] = self.home
        # Per-process caches in the module must not leak across tests.
        token_scraper._agentname_cache.clear()
        token_scraper._directive_cache.clear()

    def tearDown(self):
        self.cap.stop()
        shutil.rmtree(self.tmp, ignore_errors=True)
        shutil.rmtree(self.home, ignore_errors=True)
        if self._orig_home is None:
            os.environ.pop("HOME", None)
        else:
            os.environ["HOME"] = self._orig_home

    def _write_settings_model(self, model: str) -> None:
        d = os.path.join(self.home, ".claude")
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "settings.json"), "w") as f:
            json.dump({"model": model}, f)

    def _write_transcript(self, name: str, lines: list[dict]) -> str:
        path = os.path.join(self.tmp, name)
        with open(path, "w") as f:
            for line in lines:
                f.write(json.dumps(line) + "\n")
        return path

    def _scraper(self) -> token_scraper.Scraper:
        return token_scraper.Scraper(
            telemetry_url=f"http://127.0.0.1:{self.cap.port}/telemetry",
            host="testhost",
            username="claude",
            session_glob="",
            cursor_path=os.path.join(self.tmp, "cursors.json"),
            dry_run=False,
            data_dir=self.tmp,
        )

    def test_suffix_applied_to_main_session(self):
        self._write_settings_model("claude-fable-5[1m]")
        path = self._write_transcript("sess-1.jsonl", [
            _asst_line("u1", "msg_A", "req_A", "claude-fable-5", 100, 10),
        ])
        s = self._scraper()
        s._process_file(path, path, "")
        self.assertEqual(len(self.cap.rows), 1)
        self.assertEqual(self.cap.rows[0]["model"], "claude-fable-5[1m]")

    def test_suffix_not_applied_to_subagent(self):
        self._write_settings_model("claude-fable-5[1m]")
        path = self._write_transcript("sess-1.jsonl", [
            _asst_line("u1", "msg_A", "req_A", "claude-fable-5", 100, 10),
        ])
        s = self._scraper()
        s._process_file(path, path, "@teammate")
        self.assertEqual(len(self.cap.rows), 1)
        self.assertEqual(self.cap.rows[0]["model"], "claude-fable-5")

    def test_suffix_requires_base_match(self):
        self._write_settings_model("claude-fable-5[1m]")
        path = self._write_transcript("sess-1.jsonl", [
            _asst_line("u1", "msg_A", "req_A", "claude-opus-4-8", 100, 10),
        ])
        s = self._scraper()
        s._process_file(path, path, "")
        self.assertEqual(len(self.cap.rows), 1)
        self.assertEqual(self.cap.rows[0]["model"], "claude-opus-4-8")

    def test_no_suffix_without_long_context_config(self):
        self._write_settings_model("claude-opus-4-8")
        path = self._write_transcript("sess-1.jsonl", [
            _asst_line("u1", "msg_A", "req_A", "claude-opus-4-8", 100, 10),
        ])
        s = self._scraper()
        s._process_file(path, path, "")
        self.assertEqual(len(self.cap.rows), 1)
        self.assertEqual(self.cap.rows[0]["model"], "claude-opus-4-8")


if __name__ == "__main__":
    unittest.main()

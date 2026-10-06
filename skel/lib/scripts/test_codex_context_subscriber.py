#!/usr/bin/env python3
"""Unit tests for codex-context-subscriber.py (pure stdlib, unittest).

A fake WebSocket-over-UDS app-server and a fake hookd /context endpoint stand
in for the real peers; synthetic data only.

Run with: python3 test_codex_context_subscriber.py
"""
from __future__ import annotations

import base64
import hashlib
import importlib.util
import json
import os
import select
import shutil
import socket
import struct
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "codex_context_subscriber", os.path.join(_HERE, "codex-context-subscriber.py"))
sub = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(sub)

_GUID = b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def _usage(last_total, window, thread="t1"):
    return {"threadId": thread, "turnId": "u1", "tokenUsage": {
        "total": {"totalTokens": last_total * 3},
        "last": {"totalTokens": last_total, "inputTokens": last_total},
        "modelContextWindow": window}}


class _Capture:
    def __init__(self):
        self.rows = []       # /context bodies
        self.sessions = []   # /session bodies
        outer = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                n = int(self.headers.get("Content-Length", 0))
                body = json.loads(self.rfile.read(n))
                (outer.sessions if self.path == "/session" else outer.rows).append(
                    (self.path, body))
                self.send_response(200)
                self.end_headers()

        self.httpd = HTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    @property
    def url(self):
        return f"http://127.0.0.1:{self.httpd.server_port}/context"

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


class _FakeServer:
    """Fake codex app-server. `script` is a per-connection list of notification
    params (dicts) pushed after the thread/resume of thread t1; connections are
    served in order, each closed by the server after pushing (unless last
    connection and keep_open)."""

    def __init__(self, path, usages_per_conn, thread=None, pad=0, chunk=0,
                 threads=None, list_ids=None, after_list=None, hook=None,
                 close_after_list=False):
        self.path = path
        self.threads = threads or {}          # tid -> thread object (default: self.thread)
        self.list_ids = list_ids if list_ids is not None else ["t1", "bad"]
        self.after_list = after_list or []    # messages pushed right after the list reply
        self.hook = hook                      # hook(fake, conn, msg) after default handling
        self.close_after_list = close_after_list
        self.pad = pad      # bytes of filler in the resume result (real servers send full history)
        self.chunk = chunk  # if >0, write every frame in chunk-sized pieces
        self.usages_per_conn = usages_per_conn
        self.thread = thread or {"id": "t1", "sessionId": "t1", "agentRole": None,
                                 "model": "gpt-test"}
        self.received = []       # decoded client JSON messages
        self.pongs = 0
        self.conns = 0
        self.srv = socket.socket(socket.AF_UNIX)
        self.srv.bind(path)
        self.srv.listen(4)
        self.t = threading.Thread(target=self._serve, daemon=True)
        self.t.start()

    def close(self):
        self.srv.close()

    @staticmethod
    def _frame(op, data):
        n = len(data)
        hdr = bytes([0x80 | op])
        if n < 126:
            hdr += bytes([n])
        elif n < 65536:
            hdr += bytes([126]) + struct.pack(">H", n)
        else:
            hdr += bytes([127]) + struct.pack(">Q", n)
        return hdr + data

    def _send(self, c, obj):
        data = self._frame(1, json.dumps(obj).encode())
        if not self.chunk:
            c.sendall(data)
            return
        for i in range(0, len(data), self.chunk):
            c.sendall(data[i:i + self.chunk])
            time.sleep(0.002)

    def _read_frames(self, c, buf):
        out = []
        while len(buf) >= 2:
            b0, b1 = buf[0], buf[1]
            n = b1 & 0x7F
            off = 2
            if n == 126:
                if len(buf) < 4:
                    break
                n = struct.unpack(">H", buf[2:4])[0]
                off = 4
            if len(buf) < off + 4 + n:
                break
            m = buf[off:off + 4]
            p = bytes(b ^ m[i % 4] for i, b in enumerate(buf[off + 4:off + 4 + n]))
            out.append((b0 & 0xF, p))
            buf = buf[off + 4 + n:]
        return out, buf

    def _serve(self):
        idx = 0
        while idx < len(self.usages_per_conn):
            try:
                c, _ = self.srv.accept()
            except OSError:
                return
            self.conns += 1
            usages = self.usages_per_conn[idx]
            idx += 1
            try:
                self._handle(c, usages, last=idx == len(self.usages_per_conn))
            except OSError:
                pass
            finally:
                c.close()

    def _handle(self, c, usages, last):
        req = b""
        while b"\r\n\r\n" not in req:
            req += c.recv(4096)
        key = [l.split(b":", 1)[1].strip() for l in req.split(b"\r\n")
               if l.lower().startswith(b"sec-websocket-key")][0]
        acc = base64.b64encode(hashlib.sha1(key + _GUID).digest())
        c.sendall(b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"
                  b"Connection: Upgrade\r\nSec-WebSocket-Accept: " + acc + b"\r\n\r\n")
        buf = req.split(b"\r\n\r\n", 1)[1]
        deadline = time.time() + 8
        while time.time() < deadline:
            r, _, _ = select.select([c], [], [], 0.1)
            if r:
                d = c.recv(65536)
                if not d:
                    return
                buf += d
            frames, buf = self._read_frames(c, buf)
            for op, p in frames:
                if op == 0xA:
                    self.pongs += 1
                    continue
                msg = json.loads(p)
                self.received.append(msg)
                m = msg.get("method")
                if m == "initialize":
                    self._send(c, {"id": "initialize", "result": {"userAgent": "fake"}})
                elif m == "thread/loaded/list":
                    self._send(c, {"id": msg["id"], "result": {"data": self.list_ids}})
                    if self.close_after_list:
                        c.sendall(self._frame(8, b""))
                    for extra in self.after_list:
                        self._send(c, extra)
                elif m == "thread/unsubscribe":
                    self._send(c, {"id": msg["id"], "result": {"status": "unsubscribed"}})
                elif m == "thread/resume":
                    tid = msg["params"]["threadId"]
                    if tid == "bad":
                        self._send(c, {"id": msg["id"], "error": {
                            "code": -32600, "message": "invalid session id: bad"}})
                    else:
                        thread = dict(self.threads.get(tid) or self.thread)
                        if self.pad:
                            thread["turns"] = [{"filler": "x" * self.pad}]
                        self._send(c, {"id": msg["id"], "result": {
                            "thread": thread, "model": thread.get("model", "gpt-test")}})
                        # server request (must NOT be answered), ping, then usages
                        self._send(c, {"id": 99, "method": "item/commandExecution/requestApproval",
                                       "params": {}})
                        c.sendall(self._frame(9, b"hi"))
                        for u in usages:
                            self._send(c, {"method": "thread/tokenUsage/updated",
                                           "params": dict(u, threadId=tid)})
                        if not last:
                            time.sleep(0.2)
                            return  # drop the connection
                if self.hook:
                    self.hook(self, c, msg)

    def methods(self, tid=None):
        return [m["method"] for m in self.received if m.get("method") and
                (tid is None or (m.get("params") or {}).get("threadId") == tid)]


class SubscriberTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="ccs-")
        self.sock = os.path.join(self.tmp, "s.sock")
        self.cap = _Capture()
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.addCleanup(self.cap.close)

    def _fake(self, *a, **kw):
        f = _FakeServer(self.sock, *a, **kw)
        self.addCleanup(f.close)
        return f

    def _sub(self, **kw):
        return sub.Subscriber(sock_path=self.sock, context_url=self.cap.url,
                              host="hosty", once_quiet=0.5, **kw)

    def test_payload_and_passivity(self):
        fake = self._fake([[_usage(25840, 258400)]])
        s = self._sub()
        self.assertEqual(s.run(once=True), 0)
        self.assertEqual(len(self.cap.rows), 1)
        path, body = self.cap.rows[0]
        self.assertEqual(path, "/context")
        params = _usage(25840, 258400)
        self.assertEqual(body, {
            "session_id": "t1", "agent_name": "", "host": "hosty",
            "context_window_size": 258400, "used_percentage": 10.0,
            "total_input_tokens": 25840, "runtime": "codex",
            "context_source": "codex_appserver", "model": "gpt-test",
            "statusline_json": json.dumps(params)})
        # the server request (id 99) was never answered; ping was ponged
        self.assertFalse([m for m in fake.received if m.get("id") == 99])
        self.assertGreaterEqual(fake.pongs, 1)
        # handshake shape
        init = fake.received[0]
        self.assertEqual(init["params"]["clientInfo"]["name"], "teamster-codex-context")
        self.assertNotIn("jsonrpc", init)
        resumes = [m for m in fake.received if m.get("method") == "thread/resume"]
        # excludeTurns would suppress the replayed usage on the real server
        self.assertEqual(resumes[0]["params"], {"threadId": "t1"})
        self.assertIn("thread/unsubscribe", fake.methods("t1"))

    def _roundtrip(self, **kw):
        self._fake([[_usage(25840, 258400)]], **kw)
        self._sub().run(once=True)
        self.assertEqual([b["total_input_tokens"] for _, b in self.cap.rows], [25840])

    def test_large_frame_16bit_length(self):
        self._roundtrip(pad=1000)

    def test_large_frame_64bit_length(self):
        self._roundtrip(pad=70000)

    def test_frames_split_across_recv(self):
        self._roundtrip(pad=3000, chunk=7)

    def test_real_shaped_flow(self):
        """list -> resume -> replay with the shapes seen live on codex 0.160.0."""
        thread = {"id": "t1", "sessionId": "t1", "parentThreadId": None,
                  "agentRole": None, "agentNickname": None, "model": "gpt-6.1-sol",
                  "source": "vscode", "threadSource": "user", "status": {"type": "idle"}}
        params = {"threadId": "t1", "turnId": "u", "tokenUsage": {
            "total": {"totalTokens": 900000, "inputTokens": 800000,
                      "cachedInputTokens": 700000, "cacheWriteInputTokens": 0,
                      "outputTokens": 100000, "reasoningOutputTokens": 5000},
            "last": {"totalTokens": 119026, "inputTokens": 118000,
                     "cachedInputTokens": 100000, "cacheWriteInputTokens": 0,
                     "outputTokens": 1026, "reasoningOutputTokens": 100},
            "modelContextWindow": 258400}}
        self._fake([[params]], thread=thread, pad=70000)
        self._sub().run(once=True)
        (_, body), = self.cap.rows
        self.assertEqual(body["used_percentage"], 46.1)
        self.assertEqual(body["total_input_tokens"], 119026)
        self.assertEqual(body["model"], "gpt-6.1-sol")
        self.assertEqual(body["agent_name"], "")

    def test_subagent_identity_matches_scraper(self):
        thread = {"id": "child", "sessionId": "parent", "parentThreadId": "parent",
                  "agentRole": "worker", "model": "m"}
        s = self._sub()
        s._learn_thread(thread)
        body = s.build_payload(_usage(10, 100, thread="child"))
        self.assertEqual(body["session_id"], "parent")
        self.assertEqual(body["agent_name"], "@worker")

    def test_subagent_nickname_naming(self):
        thread = {"id": "01a108e6xyz", "sessionId": "01a108dd", "parentThreadId": "01a108dd",
                  "agentRole": None, "agentNickname": "Curie", "model": "m"}
        s = self._sub()
        s._learn_thread(thread)
        body = s.build_payload(_usage(10, 100, thread="01a108e6xyz"))
        self.assertEqual(body["session_id"], "01a108dd")
        self.assertEqual(body["agent_name"], "@Curie")
        thread2 = dict(thread, id="abcdef123456", agentNickname=None)
        s._learn_thread(thread2)
        self.assertEqual(s.build_payload(_usage(10, 100, thread="abcdef123456"))["agent_name"],
                         "@abcdef12")

    def test_percentage_clamped(self):
        s = self._sub()
        body = s.build_payload(_usage(300, 100))
        self.assertEqual(body["used_percentage"], 100.0)
        self.assertEqual(body["total_input_tokens"], 300)

    def test_close_frame_raises(self):
        self._fake([[]], close_after_list=True)
        with self.assertRaises(sub.WSClosed):
            self._sub().session(once=True)

    def test_oversize_frame_closes(self):
        old = sub._MAX_FRAME
        sub._MAX_FRAME = 100
        self.addCleanup(setattr, sub, "_MAX_FRAME", old)
        self._fake([[_usage(10, 100)]], pad=500)
        with self.assertRaises(sub.WSClosed):
            self._sub().session(once=True)

    def test_startup_resume_replay_unsubscribe_order(self):
        fake = self._fake([[_usage(10, 100)]], list_ids=["t1"])
        self._sub().run(once=True)
        self.assertEqual(fake.methods(),
                         ["initialize", "initialized", "thread/loaded/list",
                          "thread/resume", "thread/unsubscribe"])
        self.assertEqual(len(self.cap.rows), 1)

    def test_thread_started_new_thread(self):
        t2 = {"id": "t2", "sessionId": "t2", "agentRole": None, "model": "gpt-test"}
        fake = self._fake([[_usage(10, 100)]], list_ids=[], threads={"t2": t2},
                          after_list=[{"method": "thread/started", "params": {"thread": t2}}])
        self._sub().run(once=True)
        self.assertEqual(fake.methods("t2"), ["thread/resume", "thread/unsubscribe"])
        self.assertEqual([b["session_id"] for _, b in self.cap.rows], ["t2"])

    def test_ephemeral_thread_skipped(self):
        eph = {"id": "t1", "sessionId": "t1", "ephemeral": True, "model": "m"}
        fake = self._fake([[_usage(10, 100)]], list_ids=["t1"], threads={"t1": eph})
        self._sub().run(once=True)
        self.assertEqual(self.cap.rows, [])
        self.assertIn("thread/unsubscribe", fake.methods("t1"))
        t2 = {"id": "t2", "sessionId": "t2", "ephemeral": True}
        s = self._sub()
        s._learn_thread(t2)
        self.assertIsNone(s.build_payload(_usage(10, 100, thread="t2")))

    def test_status_active_resumes_idle_unsubscribes(self):
        def status(t, typ):
            return {"method": "thread/status/changed",
                    "params": {"threadId": t, "status": {"type": typ}}}

        def hook(f, c, msg):
            m = msg.get("method")
            if m == "thread/unsubscribe" and f.methods("t1").count("thread/unsubscribe") == 1:
                f._send(c, status("t1", "active"))
            if m == "thread/resume" and f.methods("t1").count("thread/resume") == 2:
                time.sleep(0.05)
                f._send(c, status("t1", "idle"))

        fake = self._fake([[_usage(10, 100)]], list_ids=["t1"], hook=hook)
        stop = threading.Event()
        s = self._sub(stop=stop)
        t = threading.Thread(target=s.run, daemon=True)
        t.start()
        deadline = time.time() + 10
        while time.time() < deadline and fake.methods("t1").count("thread/unsubscribe") < 2:
            time.sleep(0.05)
        stop.set()
        t.join(5)
        self.assertEqual(fake.methods("t1"),
                         ["thread/resume", "thread/unsubscribe",
                          "thread/resume", "thread/unsubscribe"])

    def test_thread_closed_prunes_state(self):
        s = self._sub()
        s._learn_thread({"id": "t9", "sessionId": "t9"})

        class _WS:
            def send_json(self, o):
                pass
        s._on_closed(_WS(), "t9")
        self.assertNotIn("t9", s.threads)

    def test_post_worker_coalesces_latest(self):
        seen, gate, entered = [], threading.Event(), threading.Event()

        def send(body):
            entered.set()
            gate.wait(5)
            seen.append(body["n"])

        w = sub.PostWorker(send)
        w.submit("k", {"n": 1})
        self.assertTrue(entered.wait(5))
        w.submit("k", {"n": 2})
        w.submit("k", {"n": 3})
        gate.set()
        w.flush()
        self.assertEqual(seen, [1, 3])

    def test_initialize_error_is_failure(self):
        fake = self._fake([[]])
        orig = fake._send

        def bad(c, obj):
            if obj.get("id") == "initialize":
                obj = {"id": "initialize", "error": {"code": -1, "message": "nope"}}
            orig(c, obj)
        fake._send = bad
        with self.assertRaises(sub.WSClosed):
            self._sub().session(once=True)

    def test_fill_math_rounding(self):
        s = self._sub()
        self.assertEqual(s.build_payload(_usage(1, 3))["used_percentage"], 33.3)
        self.assertNotIn("model", s.build_payload(_usage(1, 3)))

    def test_zero_or_null_window_skipped(self):
        fake = self._fake([[_usage(5, 0), _usage(5, None), _usage(50, 100)]])
        s = self._sub()
        s.run(once=True)
        self.assertEqual([b["total_input_tokens"] for _, b in self.cap.rows], [50])
        s2 = self._sub()
        self.assertIsNone(s2.build_payload(_usage(5, 0)))
        self.assertIsNone(s2.build_payload(_usage(5, None)))
        self.assertEqual(len(s2._warned_nowindow), 1)  # logged once

    def test_reconnect_after_server_close(self):
        fake = self._fake([[_usage(10, 100)], [_usage(20, 100)]])
        stop = threading.Event()
        s = self._sub(stop=stop)
        s._sleep = lambda secs: stop.wait(0.1)
        t = threading.Thread(target=s.run, daemon=True)
        t.start()
        deadline = time.time() + 10
        while time.time() < deadline and len(self.cap.rows) < 2:
            time.sleep(0.05)
        stop.set()
        t.join(5)
        self.assertEqual([b["total_input_tokens"] for _, b in self.cap.rows], [10, 20])
        self.assertGreaterEqual(fake.conns, 2)

    def test_dry_run_prints_no_post(self):
        self._fake([[_usage(10, 100)]])
        lines = []
        s = self._sub(dry_run=True, printer=lines.append)
        s.run(once=True)
        self.assertEqual(self.cap.rows, [])
        bodies = [json.loads(l) for l in lines]
        ctx = [b for b in bodies if "used_percentage" in b]
        ses = [b for b in bodies if "relationship" in b]
        self.assertEqual(len(ctx), 1)
        self.assertEqual(ctx[0]["used_percentage"], 10.0)
        self.assertEqual(len(ses), 1)  # status absent = unknown -> registered

    def test_missing_socket_once_exits_cleanly(self):
        self.assertEqual(self._sub().run(once=True), 0)

    def test_post_failure_does_not_raise(self):
        s = sub.Subscriber(sock_path=self.sock, context_url="http://127.0.0.1:1/context",
                           host="h")
        s.post_context({"session_id": "x", "agent_name": "", "used_percentage": 1})
        self.assertEqual(s.posted, 0)


_FIXTURE_JSON = '''{"id":"01a108e6-9810-7cd1-a1a2-2d7d1eee6a0b","sessionId":"01a108dd-60b4-7722-b2d1-36ee22189a03","parentThreadId":"01a108dd-60b4-7722-b2d1-36ee22189a03","forkedFromId":null,"ephemeral":false,"historyMode":"paginated","modelProvider":"openai","model":"gpt-6.1-sol","reasoningEffort":"high","cwd":"/home/claude/teamster","cliVersion":"0.160.0","originator":"codex-tui","source":{"subagent":{"thread_spawn":{"parent_thread_id":"01a108dd-60b4-7722-b2d1-36ee22189a03","depth":1,"agent_path":"/root/review_data_analysis","agent_nickname":"Avicenna","agent_role":null}}},"threadSource":"subagent","agentNickname":"Avicenna","agentRole":null,"name":null,"status":{"type":"active"},"turns":[]}'''

_FIXTURE = json.loads(_FIXTURE_JSON)
_AVICENNA = _FIXTURE


class IdentityTest(unittest.TestCase):
    def _s(self, **kw):
        return sub.Subscriber(sock_path="/nonexistent", context_url="http://h:1/context",
                              host="hosty", username="u", **kw)

    def test_fixture_identity(self):
        s = self._s()
        s._learn_thread(_AVICENNA)
        info = s.threads[_AVICENNA["id"]]
        self.assertEqual((info["session_id"], info["agent_name"]),
                         ("01a108dd-60b4-7722-b2d1-36ee22189a03", "@Avicenna"))
        self.assertEqual(info["relationship"], "subagent")
        self.assertEqual(info["thread_id"], "01a108e6-9810-7cd1-a1a2-2d7d1eee6a0b")

    def test_fixture_json_identity(self):
        thread = json.loads(_FIXTURE_JSON)
        s = self._s()
        s._learn_thread(thread)
        info = s.threads[thread["id"]]
        self.assertEqual((info["session_id"], info["agent_name"]),
                         ("01a108dd-60b4-7722-b2d1-36ee22189a03", "@Avicenna"))

    def test_whitespace_naming_parity(self):
        cases = [
            (" ", None, "@" + _AVICENNA["id"][:8]),
            (" ", "rev", "@rev"),
            ("\x1cA\x1cB", None, "@\x1cA\x1cB"),
            ("A B", None, "@A-B"),
            ("A\u00a0B", None, "@A\u00a0B"),
            ("A\t\tB", None, "@A-B"),
        ]
        for nick, role, want in cases:
            s = self._s()
            s._learn_thread(dict(_AVICENNA, agentNickname=nick, agentRole=role))
            self.assertEqual(s.threads[_AVICENNA["id"]]["agent_name"], want, repr(nick))

    def test_reset_clears_session_stamp(self):
        s = self._s()
        s._session_stamp["x"] = 1.0
        s._reset_session_state()
        self.assertEqual(s._session_stamp, {})

    def test_resumed_idle_does_not_post_session(self):
        lines = []
        s = self._s(dry_run=True, printer=lines.append)

        class _WS:
            def send_json(self, o):
                pass
        ws = _WS()
        idle = dict(_AVICENNA, status={"type": "idle"})
        s._on_resumed(ws, idle["id"], {"thread": idle})
        s._session_worker.flush()
        self.assertEqual(lines, [])
        s2 = self._s(dry_run=True, printer=lines.append)
        s2._on_resumed(ws, _AVICENNA["id"], {"thread": _AVICENNA})
        s2._session_worker.flush()
        self.assertEqual(len(lines), 1)

    def test_resumed_status_variants(self):
        class _WS:
            def send_json(self, o):
                pass
        base = {k: v for k, v in _AVICENNA.items() if k != "status"}
        for status, want in ((None, 1), ({"type": "idle"}, 0), ({"type": "active"}, 1)):
            lines = []
            s = self._s(dry_run=True, printer=lines.append)
            th = dict(base) if status is None else dict(base, status=status)
            s._on_resumed(_WS(), th["id"], {"thread": th})
            s._session_worker.flush()
            self.assertEqual(len(lines), want, repr(status))

    def test_status_active_posts_session_rate_limited(self):
        class _WS:
            def send_json(self, o):
                pass
        lines = []
        s = self._s(dry_run=True, printer=lines.append)
        s._learn_thread(dict(_AVICENNA, status={"type": "idle"}))
        tid = _AVICENNA["id"]
        s._on_status(_WS(), tid, {"type": "active"})
        s._session_worker.flush()
        self.assertEqual(len(lines), 1)
        s._on_status(_WS(), tid, {"type": "active"})
        s._session_worker.flush()
        self.assertEqual(len(lines), 1)
        s._on_resumed(_WS(), tid, {"thread": dict(_AVICENNA, status={"type": "active"})})
        s._session_worker.flush()
        self.assertEqual(len(lines), 1)

    def test_non_string_nickname_role(self):
        for nick, role in ((42, None), (None, ["x"])):
            s = self._s()
            s._learn_thread(dict(_AVICENNA, agentNickname=nick, agentRole=role))
            self.assertEqual(s.threads[_AVICENNA["id"]]["agent_name"],
                             "@" + _AVICENNA["id"][:8])

    def test_nickname_beats_role(self):
        s = self._s()
        s._learn_thread(dict(_AVICENNA, agentRole="reviewer"))
        self.assertEqual(s.threads[_AVICENNA["id"]]["agent_name"], "@Avicenna")
        s._learn_thread(dict(_AVICENNA, agentNickname=None, agentRole="reviewer"))
        self.assertEqual(s.threads[_AVICENNA["id"]]["agent_name"], "@reviewer")

    def test_whitespace_sanitized(self):
        s = self._s()
        s._learn_thread(dict(_AVICENNA, agentNickname="Avicenna the 2nd"))
        self.assertEqual(s.threads[_AVICENNA["id"]]["agent_name"], "@Avicenna-the-2nd")

    def test_thread_source_alone_marks_subagent(self):
        s = self._s()
        s._learn_thread(dict(_AVICENNA, parentThreadId=None))
        self.assertEqual(s.threads[_AVICENNA["id"]]["relationship"], "subagent")

    def test_root_thread_is_lead(self):
        s = self._s()
        s._learn_thread({"id": "root1", "sessionId": "root1", "parentThreadId": None,
                         "agentRole": "worker", "agentNickname": "X", "threadSource": "user"})
        self.assertEqual(s.threads["root1"]["agent_name"], "")
        self.assertEqual(s.threads["root1"]["relationship"], "lead")

    def test_session_url_derivation(self):
        self.assertEqual(sub._session_url("http://h:9125/context"), "http://h:9125/session")
        self.assertEqual(self._s().session_url, "http://h:1/session")
        self.assertEqual(self._s(session_url="http://x/session").session_url, "http://x/session")

    def test_session_body_shape(self):
        s = self._s()
        s._learn_thread(_AVICENNA)
        self.assertEqual(s.build_session(_AVICENNA["id"]), {
            "session_id": "01a108dd-60b4-7722-b2d1-36ee22189a03", "agent_name": "@Avicenna",
            "host": "hosty", "username": "u", "runtime": "codex",
            "cwd": "/home/claude/teamster", "model": "gpt-6.1-sol", "originator": "codex-tui",
            "cli_version": "0.160.0", "relationship": "subagent",
            "agent_id": "01a108e6-9810-7cd1-a1a2-2d7d1eee6a0b"})

    def test_ephemeral_has_no_session_body(self):
        s = self._s()
        s._learn_thread(dict(_AVICENNA, ephemeral=True))
        self.assertIsNone(s.build_session(_AVICENNA["id"]))

    def test_started_posts_session_once_and_heartbeat_throttled(self):
        lines = []
        s = self._s(dry_run=True, printer=lines.append)
        clock = [1000.0]
        orig = sub.time.monotonic
        sub.time.monotonic = lambda: clock[0]
        self.addCleanup(setattr, sub.time, "monotonic", orig)

        class _WS:
            def send_json(self, o):
                pass
        ws, tid = _WS(), _AVICENNA["id"]
        s._on_started(ws, _AVICENNA)  # status active -> live
        s._session_worker.flush()
        self.assertEqual(len(lines), 1)
        self.assertEqual(json.loads(lines[0])["relationship"], "subagent")
        msg = {"method": "thread/tokenUsage/updated", "params": _usage(10, 100, thread=tid)}

        def beat():
            s._handle(ws, msg)
            s._worker.flush()
            s._session_worker.flush()
            return len([l for l in lines if "relationship" in json.loads(l)])
        clock[0] += 30
        self.assertEqual(beat(), 1)   # <60s since learn: no re-post
        clock[0] += 31
        self.assertEqual(beat(), 2)   # >=60s: heartbeat
        clock[0] += 10
        self.assertEqual(beat(), 2)   # throttled again
        clock[0] += 60
        self.assertEqual(beat(), 3)
        s._on_closed(ws, tid)
        self.assertNotIn(tid, s._session_stamp)

    def test_no_heartbeat_for_non_live_thread(self):
        lines = []
        s = self._s(dry_run=True, printer=lines.append)
        s._learn_thread(_AVICENNA)
        s._handle(None, {"method": "thread/tokenUsage/updated",
                         "params": _usage(10, 100, thread=_AVICENNA["id"])})
        s._worker.flush()
        s._session_worker.flush()
        self.assertEqual([l for l in lines if "relationship" in json.loads(l)], [])

    def test_end_to_end_session_post(self):
        tmp = tempfile.mkdtemp(prefix="ccs-")
        self.addCleanup(shutil.rmtree, tmp, True)
        cap = _Capture()
        self.addCleanup(cap.close)
        fake = _FakeServer(os.path.join(tmp, "s.sock"), [[_usage(10, 100, thread=_AVICENNA["id"])]],
                           threads={_AVICENNA["id"]: _AVICENNA}, list_ids=[_AVICENNA["id"]])
        self.addCleanup(fake.close)
        s = sub.Subscriber(sock_path=os.path.join(tmp, "s.sock"), context_url=cap.url,
                           host="hosty", once_quiet=0.5, username="u")
        s.run(once=True)
        self.assertGreaterEqual(len(cap.sessions), 1)
        path, body = cap.sessions[0]
        self.assertEqual(path, "/session")
        self.assertEqual(body["agent_name"], "@Avicenna")
        self.assertEqual(body["relationship"], "subagent")
        self.assertEqual(body["agent_id"], _AVICENNA["id"])
        self.assertEqual([b["agent_name"] for _, b in cap.rows], ["@Avicenna"])


if __name__ == "__main__":
    unittest.main()

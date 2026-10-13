#!/usr/bin/env python3
"""codex-context-subscriber: report Codex thread context-window fill to hookd.

Codex runs a shared per-user app-server daemon whose control socket speaks
WebSocket (HTTP Upgrade GET /rpc) over a unix socket, one JSON-RPC message per
text frame. This daemon attaches as a passive listener and forwards each
thread/tokenUsage/updated notification to hookd's POST /context — the same
endpoint and body shape teamster-statusline.sh uses for Claude Code gauges.
It holds a subscription (thread/resume) only while a thread is Active, since a
resume pins the thread in the daemon: at startup and on thread/started it takes
a one-shot snapshot (resume, read the replayed usage, unsubscribe), and it
unsubscribes when a thread goes idle or closes.

Context fill = last.totalTokens / modelContextWindow.

Passive by design: server->client requests (approvals etc.) are NEVER answered
(first responder wins; a silent listener must not be able to answer an
operator's prompt). Only WebSocket pings are ponged.

Pure stdlib. Runs as a long-lived daemon (systemd Type=simple); --once
snapshots all loaded threads, forwards the replayed usages, and exits.
"""
from __future__ import annotations

import argparse
import base64
import getpass
import json
import logging
import os
import re
import select
import signal
import socket
import struct
import sys
import threading
import time
import urllib.request

__version__ = "dev"
_HTTP_TIMEOUT = 3  # seconds
_BACKOFF_MAX = 30.0
# thread/resume results embed the full turn history (multi-MB in practice);
# 64 MiB leaves ample headroom while bounding memory against a corrupt length.
_MAX_FRAME = 64 * 1024 * 1024
_REPLAY_WAIT = 1.5  # seconds to wait for a replayed tokenUsage after resume
_CONTEXT_SOURCE = "codex_appserver"
_SESSION_HEARTBEAT_S = 60.0  # min seconds between /session re-posts per active thread
_DEBUG = os.environ.get("CODEX_CTX_DEBUG", "") in ("1", "true")


# ---------------------------------------------------------------------------
# Config discovery — mirrors codex-scraper.py (_codex_home, _hub_base_url,
# TEAMSTER_HOST/gethostname host derivation).
# ---------------------------------------------------------------------------

def _codex_home() -> str:
    return os.environ.get("CODEX_HOME") or os.path.join(os.path.expanduser("~"), ".codex")


def _hub_base_url() -> str:
    hook_url = os.environ.get("TEAMSTER_HOOK_SERVER_URL", "http://localhost:9125/event")
    base = hook_url
    if base.endswith("/event"):
        base = base[:-len("/event")]
    return base


def _context_url() -> str:
    direct = os.environ.get("TEAMSTER_CONTEXT_URL", "")
    if direct:
        return direct
    return _hub_base_url() + "/context"


def _session_url(context_url: str) -> str:
    if context_url.endswith("/context"):
        return context_url[:-len("/context")] + "/session"
    return _hub_base_url() + "/session"


def _username() -> str:
    username = os.environ.get("TEAMSTER_USER", "")
    if not username:
        try:
            username = getpass.getuser()
        except Exception:
            username = ""
    return username


def _host() -> str:
    host = os.environ.get("TEAMSTER_HOST", "")
    if not host:
        try:
            host = socket.gethostname().split(".")[0]
        except Exception:
            host = "localhost"
    return host


def _socket_path() -> str:
    return os.environ.get("CODEX_APP_SERVER_SOCK") or os.path.join(
        _codex_home(), "app-server-control", "app-server-control.sock")


# ---------------------------------------------------------------------------
# Minimal WebSocket-over-UDS client (RFC 6455 client side).
# ---------------------------------------------------------------------------

class WSClosed(Exception):
    pass


class WSClient:
    def __init__(self, sock_path: str, timeout: float = 10.0):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(timeout)
        self.sock.connect(sock_path)
        self.buf = bytearray()
        self._frag = bytearray()
        try:
            self._handshake()
        except BaseException:
            self.close()
            raise

    def _handshake(self) -> None:
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall((
            "GET /rpc HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\n"
            "Upgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"
            f"Sec-WebSocket-Key: {key}\r\n\r\n").encode())
        while b"\r\n\r\n" not in self.buf:
            d = self.sock.recv(4096)
            if not d:
                raise WSClosed("EOF during upgrade")
            self.buf += d
        idx = self.buf.index(b"\r\n\r\n")
        head = bytes(self.buf[:idx])
        del self.buf[:idx + 4]
        status = head.split(b"\r\n", 1)[0]
        if b" 101" not in status:
            raise WSClosed("upgrade refused: %s" % status.decode(errors="replace"))

    def _send_frame(self, opcode: int, data: bytes) -> None:
        mask = os.urandom(4)
        n = len(data)
        hdr = bytes([0x80 | opcode])
        if n < 126:
            hdr += bytes([0x80 | n])
        elif n < 65536:
            hdr += bytes([0x80 | 126]) + struct.pack(">H", n)
        else:
            hdr += bytes([0x80 | 127]) + struct.pack(">Q", n)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(data))
        self.sock.sendall(hdr + mask + masked)

    def send_json(self, obj) -> None:
        if _DEBUG:
            logging.info("DEBUG send %s", json.dumps(obj)[:200])
        self._send_frame(0x1, json.dumps(obj).encode())

    def _parse_one(self):
        """Parse one frame from buf; return (fin, opcode, payload) or None."""
        buf = self.buf
        if len(buf) < 2:
            return None
        b0, b1 = buf[0], buf[1]
        n = b1 & 0x7F
        off = 2
        if n == 126:
            if len(buf) < 4:
                return None
            n = struct.unpack(">H", buf[2:4])[0]
            off = 4
        elif n == 127:
            if len(buf) < 10:
                return None
            n = struct.unpack(">Q", buf[2:10])[0]
            off = 10
        if n > _MAX_FRAME:
            raise WSClosed("frame too large: %d bytes (cap %d)" % (n, _MAX_FRAME))
        masked = bool(b1 & 0x80)
        if masked:
            off += 4
        if len(buf) < off + n:
            return None
        payload = bytes(buf[off:off + n])
        if masked:
            m = bytes(buf[off - 4:off])
            payload = bytes(b ^ m[i % 4] for i, b in enumerate(payload))
        del buf[:off + n]
        return bool(b0 & 0x80), b0 & 0x0F, payload

    def recv_messages(self, timeout: float):
        """Return a list of decoded JSON-RPC dicts ([] on timeout). Pings are
        ponged here. Raises WSClosed on EOF or a close frame."""
        out = []
        end = time.monotonic() + timeout
        while True:
            while True:
                fr = self._parse_one()
                if fr is None:
                    break
                fin, op, payload = fr
                if _DEBUG:
                    logging.info("DEBUG recv op=%s fin=%s len=%d head=%r", op, fin,
                                 len(payload), payload[:200])
                if op == 0x9:
                    self._send_frame(0xA, payload)
                elif op == 0xA:
                    pass
                elif op == 0x8:
                    raise WSClosed("close frame")
                elif op in (0x1, 0x0, 0x2):
                    self._frag += payload
                    if len(self._frag) > _MAX_FRAME:
                        raise WSClosed("fragmented message too large")
                    if fin:
                        data, self._frag = bytes(self._frag), bytearray()
                        try:
                            msg = json.loads(data.decode("utf-8", errors="replace"))
                        except Exception:
                            logging.warning("bad frame (unparseable JSON), ignoring")
                            continue
                        if isinstance(msg, dict):
                            out.append(msg)
            if out:
                return out
            rem = end - time.monotonic()
            if rem <= 0:
                return []
            r, _, _ = select.select([self.sock], [], [], rem)
            if not r:
                return []
            d = self.sock.recv(65536)
            if not d:
                raise WSClosed("EOF")
            self.buf += d

    def close(self) -> None:
        try:
            self.sock.close()
        except Exception:
            pass


# ---------------------------------------------------------------------------
# Subscriber
# ---------------------------------------------------------------------------

def _int(v) -> int:
    try:
        return int(v)
    except (TypeError, ValueError):
        return 0


def _status_type(status) -> str:
    if isinstance(status, dict):
        return str(status.get("type") or "").lower()
    return str(status or "").lower()


class PostWorker:
    """Sends context bodies off the read loop, coalescing to the latest body per
    key so a slow hookd can never stall the socket or build an unbounded queue."""

    def __init__(self, send):
        self._send = send
        self._cv = threading.Condition()
        self._latest: dict = {}
        self._busy = False
        self._thread: threading.Thread | None = None

    def submit(self, key, body: dict) -> None:
        with self._cv:
            self._latest[key] = body
            if self._thread is None:
                self._thread = threading.Thread(target=self._run, daemon=True)
                self._thread.start()
            self._cv.notify_all()

    def _run(self) -> None:
        while True:
            with self._cv:
                while not self._latest:
                    self._cv.wait()
                key = next(iter(self._latest))
                body = self._latest.pop(key)
                self._busy = True
            try:
                self._send(body)
            except Exception as exc:
                logging.error("post worker: %s", exc)
            finally:
                with self._cv:
                    self._busy = False
                    self._cv.notify_all()

    def flush(self, timeout: float = 5.0) -> None:
        end = time.monotonic() + timeout
        with self._cv:
            while self._latest or self._busy:
                rem = end - time.monotonic()
                if rem <= 0:
                    return
                self._cv.wait(rem)


_IDENTITY_WS = " \t\n\r\f\v"
_IDENTITY_WS_RUN = re.compile("[" + re.escape(_IDENTITY_WS) + "]+")


class Subscriber:
    def __init__(self, *, sock_path: str, context_url: str, host: str,
                 dry_run: bool = False, stop: threading.Event | None = None,
                 once_quiet: float = 1.0, printer=print, session_url: str = "",
                 username: str | None = None):
        self.sock_path = sock_path
        self.context_url = context_url
        self.session_url = session_url or _session_url(context_url)
        self.username = _username() if username is None else username
        self.host = host
        self.dry_run = dry_run
        self.stop = stop or threading.Event()
        self.once_quiet = once_quiet
        self.printer = printer
        self.threads: dict[str, dict] = {}   # threadId -> {session_id, agent_name, model, ephemeral}
        self._warned_nowindow: set[str] = set()
        self._next_id = 10
        self._pending: dict = {}             # request id -> ("list",) | ("resume", tid) | ("unsub", tid)
        self.posted = 0
        self._backoff = 1.0
        self._worker = PostWorker(self.post_context)
        self._session_worker = PostWorker(self.post_session)
        self._session_stamp: dict[str, float] = {}  # tid -> monotonic time of last /session post
        self._reset_session_state()

    def _reset_session_state(self) -> None:
        self._session_stamp = {}
        self._pending = {}
        self._subscribed: set[str] = set()   # we hold a subscription on the server
        self._resuming: set[str] = set()     # thread/resume in flight
        self._live: set[str] = set()         # Active: keep subscribed until idle
        self._snapshot: dict[str, float] = {}  # tid -> deadline; unsubscribe after replay

    # -- thread identity (matches codex-scraper's sessions row) -------------

    def _learn_thread(self, thread: dict, model: str = "") -> None:
        tid = thread.get("id") or ""
        if not tid:
            return
        # DESIGN D2 (same rule as codex-scraper): session_id = sessionId else id
        # (a subagent carries its PARENT's session id); a subagent is named
        # "@" + nickname, else role, else id[:8]; root threads are the lead ("").
        sid = thread.get("sessionId") or tid
        is_sub = bool(thread.get("parentThreadId")) or thread.get("threadSource") == "subagent"
        info = self.threads.setdefault(tid, {})
        info["session_id"] = sid
        info["thread_id"] = tid
        if is_sub:
            nick, role = thread.get("agentNickname"), thread.get("agentRole")
            name = ((nick if isinstance(nick, str) else "").strip(_IDENTITY_WS)
                    or (role if isinstance(role, str) else "").strip(_IDENTITY_WS) or tid[:8])
            info["agent_name"] = "@" + _IDENTITY_WS_RUN.sub("-", name)
            info["relationship"] = "subagent"
        else:
            info["agent_name"] = ""
            info["relationship"] = "lead"
        info["ephemeral"] = bool(thread.get("ephemeral"))
        for key, field in (("cwd", "cwd"), ("originator", "originator"),
                           ("cli_version", "cliVersion")):
            if thread.get(field):
                info[key] = thread[field]
        m = model or thread.get("model") or ""
        if m:
            info["model"] = m

    # -- POST ---------------------------------------------------------------

    def build_payload(self, params: dict):
        tid = params.get("threadId") or ""
        usage = params.get("tokenUsage") or {}
        window = _int(usage.get("modelContextWindow"))
        last = usage.get("last") or {}
        if not tid:
            return None
        info = self.threads.get(tid, {})
        if info.get("ephemeral"):
            return None
        if window <= 0:
            if tid not in self._warned_nowindow:
                self._warned_nowindow.add(tid)
                logging.warning("skip thread=%s: no modelContextWindow", tid)
            return None
        used = _int(last.get("totalTokens"))
        # last.totalTokens can exceed the window at peak pressure; hookd 400s above 100
        pct = min(100.0, max(0.0, round(100 * used / window, 1)))
        body = {
            "session_id": info.get("session_id", tid),
            "agent_name": info.get("agent_name", ""),
            "host": self.host,
            "context_window_size": window,
            "used_percentage": pct,
            "total_input_tokens": used,
            "runtime": "codex",
            "context_source": _CONTEXT_SOURCE,
            "statusline_json": json.dumps(params),
        }
        if info.get("model"):
            body["model"] = info["model"]
        return body

    def build_session(self, tid: str):
        info = self.threads.get(tid)
        if not info or info.get("ephemeral"):
            return None
        body = {
            "session_id": info.get("session_id", tid),
            "agent_name": info.get("agent_name", ""),
            "host": self.host,
            "username": self.username,
            "runtime": "codex",
            "cwd": info.get("cwd", ""),
            "model": info.get("model", ""),
            "originator": info.get("originator", ""),
            "cli_version": info.get("cli_version", ""),
            "relationship": info.get("relationship", "lead"),
            "agent_id": info.get("thread_id", tid),
        }
        return body

    def _submit_session(self, tid: str) -> None:
        body = self.build_session(tid)
        if body:
            self._session_stamp[tid] = time.monotonic()
            self._session_worker.submit((body["session_id"], body["agent_name"]), body)

    def post_session(self, body: dict) -> None:
        if self.dry_run:
            self.printer(json.dumps(body))
            self.posted += 1
            return
        try:
            req = urllib.request.Request(
                self.session_url, data=json.dumps(body).encode(), method="POST",
                headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=_HTTP_TIMEOUT) as resp:
                resp.read()
            self.posted += 1
            logging.info("session posted session_id=%s agent=%r rel=%s", body["session_id"],
                         body["agent_name"], body["relationship"])
        except Exception as exc:
            logging.error("session POST failed session_id=%s: %s", body["session_id"], exc)

    def post_context(self, body: dict) -> None:
        if self.dry_run:
            self.printer(json.dumps(body))
            self.posted += 1
            return
        try:
            req = urllib.request.Request(
                self.context_url, data=json.dumps(body).encode(), method="POST",
                headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=_HTTP_TIMEOUT) as resp:
                resp.read()
            self.posted += 1
            logging.info("posted session_id=%s agent=%r pct=%s", body["session_id"],
                         body["agent_name"], body["used_percentage"])
        except Exception as exc:
            logging.error("context POST failed session_id=%s: %s", body["session_id"], exc)

    # -- protocol -----------------------------------------------------------

    def _request(self, ws: WSClient, method: str, params: dict, tag) -> None:
        rid = self._next_id
        self._next_id += 1
        self._pending[rid] = tag
        ws.send_json({"id": rid, "method": method, "params": params})

    def _resume(self, ws: WSClient, tid: str) -> None:
        if tid in self._subscribed or tid in self._resuming:
            return
        self._resuming.add(tid)
        self._request(ws, "thread/resume", {"threadId": tid}, ("resume", tid))

    def _unsubscribe(self, ws: WSClient, tid: str) -> None:
        if tid not in self._subscribed:
            return
        self._subscribed.discard(tid)
        self._request(ws, "thread/unsubscribe", {"threadId": tid}, ("unsub", tid))

    def _on_status(self, ws: WSClient, tid: str, status) -> None:
        if tid in self.threads and self.threads[tid].get("ephemeral"):
            return
        if _status_type(status) == "active":
            self._live.add(tid)
            self._snapshot.pop(tid, None)
            info = self.threads.get(tid)
            if (info and not info.get("ephemeral") and
                    time.monotonic() - self._session_stamp.get(tid, float("-inf"))
                    >= _SESSION_HEARTBEAT_S):
                self._submit_session(tid)
            self._resume(ws, tid)
        else:
            self._live.discard(tid)
            if tid not in self._snapshot:
                self._unsubscribe(ws, tid)

    def _on_closed(self, ws: WSClient, tid: str) -> None:
        self._unsubscribe(ws, tid)
        self._live.discard(tid)
        self._snapshot.pop(tid, None)
        self._resuming.discard(tid)
        self.threads.pop(tid, None)
        self._warned_nowindow.discard(tid)
        self._session_stamp.pop(tid, None)

    def _on_started(self, ws: WSClient, thread: dict) -> None:
        tid = thread.get("id")
        if not tid:
            return
        self._learn_thread(thread)
        if thread.get("ephemeral"):
            return
        self._submit_session(tid)
        if _status_type(thread.get("status")) == "active":
            self._live.add(tid)
        self._resume(ws, tid)

    def _on_resumed(self, ws: WSClient, tid: str, result: dict) -> None:
        self._resuming.discard(tid)
        thread = result.get("thread") or {}
        if thread.get("id"):
            self._learn_thread(thread, result.get("model") or "")
        self._subscribed.add(tid)
        if self.threads.get(tid, {}).get("ephemeral"):
            self._unsubscribe(ws, tid)
            return
        st = _status_type(thread.get("status"))
        if ((st in ("", "active") or tid in self._live)
                and time.monotonic() - self._session_stamp.get(tid, float("-inf"))
                >= _SESSION_HEARTBEAT_S):
            self._submit_session(tid)
        if st == "active" or tid in self._live:
            self._live.add(tid)
        else:
            self._snapshot[tid] = time.monotonic() + _REPLAY_WAIT

    def _expire_snapshots(self, ws: WSClient) -> None:
        now = time.monotonic()
        for tid in [t for t, d in self._snapshot.items() if d <= now]:
            del self._snapshot[tid]
            self._unsubscribe(ws, tid)

    def _handle(self, ws: WSClient, msg: dict) -> None:
        method = msg.get("method")
        mid = msg.get("id")
        if method is not None:
            if mid is not None:
                return  # server request: deliberately never answered
            params = msg.get("params") or {}
            if method == "thread/tokenUsage/updated":
                body = self.build_payload(params)
                if body:
                    self._worker.submit((body["session_id"], body["agent_name"]), body)
                tid = params.get("threadId") or ""
                if (tid in self._live and
                        time.monotonic() - self._session_stamp.get(tid, float("-inf"))
                        >= _SESSION_HEARTBEAT_S):
                    self._submit_session(tid)
                if tid in self._snapshot:
                    del self._snapshot[tid]
                    self._unsubscribe(ws, tid)
            elif method == "thread/started":
                self._on_started(ws, params.get("thread") or {})
            elif method == "thread/status/changed":
                if params.get("threadId"):
                    self._on_status(ws, params["threadId"], params.get("status"))
            elif method == "thread/closed":
                if params.get("threadId"):
                    self._on_closed(ws, params["threadId"])
            return
        if mid in self._pending:
            tag = self._pending.pop(mid)
            if "error" in msg:
                logging.warning("request failed tag=%s: %s", tag,
                                (msg["error"] or {}).get("message"))
                if tag[0] == "resume":
                    self._resuming.discard(tag[1])
                return
            result = msg.get("result") or {}
            if tag[0] == "list":
                for tid in result.get("data") or []:
                    if isinstance(tid, str):
                        self._resume(ws, tid)
            elif tag[0] == "resume":
                self._on_resumed(ws, tag[1], result)

    def session(self, once: bool = False) -> None:
        """One connection's lifetime. Raises WSClosed/OSError on disconnect."""
        ws = WSClient(self.sock_path)
        self._reset_session_state()
        try:
            ws.send_json({"id": "initialize", "method": "initialize", "params": {
                "clientInfo": {"name": "teamster-codex-context", "title": None,
                               "version": __version__},
                "capabilities": {"experimentalApi": False}}})
            deadline = time.monotonic() + 10
            got_init = False
            while not got_init:
                if time.monotonic() > deadline:
                    raise WSClosed("initialize timeout")
                for msg in ws.recv_messages(1.0):
                    if msg.get("id") == "initialize":
                        if "error" in msg:
                            raise WSClosed("initialize failed: %s" %
                                           ((msg["error"] or {}).get("message")))
                        got_init = True
                    else:
                        self._handle(ws, msg)
            self._backoff = 1.0
            ws.send_json({"method": "initialized"})
            self._request(ws, "thread/loaded/list", {}, ("list",))
            logging.info("attached sock=%s", self.sock_path)
            last_activity = time.monotonic()
            started = last_activity
            while not self.stop.is_set():
                msgs = ws.recv_messages(1.0)
                if msgs:
                    last_activity = time.monotonic()
                for msg in msgs:
                    self._handle(ws, msg)
                self._expire_snapshots(ws)
                if once:
                    now = time.monotonic()
                    idle = not self._pending and not self._snapshot
                    if idle and now - last_activity >= self.once_quiet:
                        return
                    if now - started > 15:
                        return
        finally:
            ws.close()
            self._worker.flush()
            self._session_worker.flush()

    def run(self, once: bool = False) -> int:
        while not self.stop.is_set():
            if not os.path.exists(self.sock_path):
                if once:
                    logging.info("no app-server socket at %s", self.sock_path)
                    return 0
                self._sleep(self._backoff)
                self._backoff = min(self._backoff * 2, _BACKOFF_MAX)
                continue
            try:
                self.session(once=once)
                if once:
                    return 0
            except (WSClosed, OSError) as exc:
                logging.warning("connection ended: %s", exc)
                if once:
                    return 0
            except Exception as exc:  # never crash the daemon
                logging.error("unexpected error: %s", exc)
                if once:
                    return 0
            self._sleep(self._backoff)
            self._backoff = min(self._backoff * 2, _BACKOFF_MAX)
        return 0

    def _sleep(self, secs: float) -> None:
        self.stop.wait(secs)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        description="codex-context-subscriber: forward Codex context fill to hookd /context")
    parser.add_argument("--once", action="store_true",
                        help="resume loaded threads, forward replayed usage, exit")
    parser.add_argument("--dry-run", action="store_true",
                        help="print payloads instead of POSTing")
    parser.add_argument("--session-url", default="",
                        help="hookd /session URL (default: context URL with /context -> /session)")
    args = parser.parse_args(argv)

    logging.basicConfig(level=logging.INFO, stream=sys.stderr,
                        format="%(asctime)s %(levelname)s codex-context-subscriber %(message)s")

    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())

    sub = Subscriber(sock_path=_socket_path(), context_url=_context_url(),
                     host=_host(), dry_run=args.dry_run, stop=stop,
                     session_url=args.session_url)
    logging.info("starting sock=%s context_url=%s session_url=%s dry_run=%s once=%s",
                 sub.sock_path, sub.context_url, sub.session_url, args.dry_run, args.once)
    try:
        return sub.run(once=args.once)
    except Exception as exc:
        logging.error("fatal: %s", exc)
        return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        sys.exit(0)

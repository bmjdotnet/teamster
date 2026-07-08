#!/usr/bin/env python3
"""remote-codex-setup: the Python port of internal/codexconfig for a Go-less
Codex remote. Invoked by remote-setup.sh (WP-R4) after ~/teamster/ has been
staged, so it wires ~/.codex/config.toml, ~/.codex/skills/, and AGENTS.md the
same way cmd/teamster-install's wireCodex() does on the hub — same marker
scheme, same backup-then-doctor-gate discipline, same hook trust-hash
algorithm — reproduced here because a remote has no Go toolchain (operator
directive: remote components are pure-stdlib Python, never Go).

WP-R1 transport ruling (research/wp-r1-transport/README.md): codex speaks
MCP over HTTP natively (`codex mcp add --url` / bare `url = "..."` form,
wire-verified at 0.137.0 and 0.142.5). So unlike the hub's stdio
`[mcp_servers.*]` tables (command + args + env pointing at local Go
binaries), remote registrations are direct-HTTP: `url = "http://<hub>/mcp/
<id>"` pointed straight at hookd — no local MCP process, no proxy. Identity
still flows via `x-codex-turn-metadata` + clientInfo, unchanged by transport.

Every config.toml write here follows codexconfig's contract exactly:
marker-bounded section (`# >>> teamster:<name> >>>` / `# <<< ... <<<`),
backup before write (.pre-teamster once + a timestamped .bak every run),
`codex --strict-config doctor --json` gated on checks["config.load"].status
(NEVER overallStatus — other checks flap for reasons unrelated to whether
the write is well-formed), and a restore-to-backup + hard failure if the
gate rejects it. See internal/codexconfig/{sectionpatch,doctor,installbackup
(ported inline below), hooktrust}.go for the reference implementation this
mirrors — TrustedHash/HookStateKey in particular are byte-verified against
that Go code in test_remote_codex_setup.py, not just structurally similar.

Reliability posture: unlike codex-hook.py/codex-scraper.py (continuously
invoked, exit-0-always, never block), THIS is a one-shot install step run
once per `teamster install-remote` — like the Go installer it mirrors, it
exits nonzero and prints a clear error on any real failure (doctor gate
rejects a write, staged assets missing) so remote-setup.sh's `|| die`
wrapper catches it. The one case that is NOT a failure: codex simply isn't
installed on this remote and --codex-mode is unset (auto-detect) — that
exits 0 with CODEX_STATUS=skipped, mirroring the hub's own auto-detect
posture (probeCodex + wireCodex's "not detected — skipped" summary).

Prints exactly one `CODEX_STATUS=<skipped|wired>` line to stdout so
remote-setup.sh can decide whether to also schedule codex-scraper.py.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import time


# ---------------------------------------------------------------------------
# installbackup port (internal/installbackup/backup.go)
# ---------------------------------------------------------------------------

def backup(path: str) -> str:
    """Preserve path's current content before an overwrite. Returns the
    timestamped backup path, or "" if path didn't exist (nothing to
    preserve — callers pass "" to restore() to mean "roll back to
    file-did-not-exist"). First-ever call also makes path+".pre-teamster",
    never touched again."""
    if not os.path.exists(path):
        return ""

    pre_teamster = path + ".pre-teamster"
    if not os.path.exists(pre_teamster):
        _copy_file(path, pre_teamster)

    ts = path + "." + time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + ".bak"
    _copy_file(path, ts)
    return ts


def restore(backup_path: str, path: str) -> None:
    """Roll path back to backup_path's content (used when the doctor gate
    rejects a write). Empty backup_path means Backup found nothing to
    preserve, so path is removed rather than left with rejected content."""
    if not backup_path:
        if os.path.exists(path):
            os.remove(path)
        return
    _copy_file(backup_path, path)


def _copy_file(src: str, dst: str) -> None:
    with open(src, "rb") as f:
        data = f.read()
    mode = os.stat(src).st_mode
    with open(dst, "wb") as f:
        f.write(data)
    os.chmod(dst, mode & 0o777)


# ---------------------------------------------------------------------------
# sectionpatch port (internal/codexconfig/sectionpatch.go)
# ---------------------------------------------------------------------------

SKIP_IF_PRESENT = "skip"
ALWAYS_UPSERT = "always"

_BLANK_RUN_RE = re.compile(r"\n{3,}")


def _marker_lines(name: str):
    return f"# >>> teamster:{name} >>>", f"# <<< teamster:{name} <<<"


def _section_re(name: str):
    start, end = _marker_lines(name)
    return re.compile(re.escape(start) + r"(?:.*?)" + re.escape(end) + r"\n?", re.DOTALL)


def _contains_line(content: str, line: str) -> bool:
    return any(l.strip() == line for l in content.split("\n"))


def _collapse_blank_runs(content: str) -> str:
    return _BLANK_RUN_RE.sub("\n\n", content)


def upsert_section(content: str, name: str, body: str, literal_header: str, policy: str):
    """Idempotently write a Teamster-owned, marker-bounded block named `name`
    into content. Returns (new_content, result) where result is a dict with
    changed/skipped_existing/unmarked_collision booleans — mirrors
    UpsertSection's UpsertResult exactly."""
    re_ = _section_re(name)
    has_marker = re_.search(content) is not None

    if not has_marker and literal_header and _contains_line(content, literal_header):
        return content, {"changed": False, "skipped_existing": False, "unmarked_collision": True}

    if has_marker and policy == SKIP_IF_PRESENT:
        return content, {"changed": False, "skipped_existing": True, "unmarked_collision": False}

    stripped = re_.sub("", content)
    stripped = _collapse_blank_runs(stripped.rstrip("\n"))

    start, end = _marker_lines(name)
    block = start + "\n" + body + end + "\n"

    if stripped.strip() == "":
        return block, {"changed": True, "skipped_existing": False, "unmarked_collision": False}
    return stripped + "\n\n" + block, {"changed": True, "skipped_existing": False, "unmarked_collision": False}


def remove_section(content: str, name: str) -> str:
    """Delete a previously-written marker span for name; no-op if absent.
    Uninstall-recipe helper, mirrors RemoveSection."""
    re_ = _section_re(name)
    if re_.search(content) is None:
        return content
    stripped = re_.sub("", content)
    return _collapse_blank_runs(stripped.rstrip("\n")) + "\n"


def _quote_toml_string(s: str) -> str:
    """Render s as a TOML basic string. Mirrors quoteTOMLString's use of Go's
    strconv.Quote for the value set this module ever renders (absolute
    paths, URLs, hostnames, mode names) — not a general TOML encoder."""
    out = ['"']
    for ch in s:
        if ch == "\\":
            out.append("\\\\")
        elif ch == '"':
            out.append('\\"')
        elif ch == "\n":
            out.append("\\n")
        elif ch == "\t":
            out.append("\\t")
        elif ch == "\r":
            out.append("\\r")
        elif ord(ch) < 0x20:
            out.append("\\u%04x" % ord(ch))
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


# ---------------------------------------------------------------------------
# doctor gate port (internal/codexconfig/doctor.go)
# ---------------------------------------------------------------------------

DOCTOR_OK = "ok"
DOCTOR_FAILED = "failed"
DOCTOR_SKIPPED = "skipped"


class DoctorResult:
    def __init__(self, status: str, summary: str = "", remediation: str = ""):
        self.status = status
        self.summary = summary
        self.remediation = remediation


def run_doctor_gate() -> DoctorResult:
    """Runs `codex --strict-config doctor --json`, gated on
    checks["config.load"].status ALONE — never overallStatus (which flaps
    "fail" for unrelated reasons, e.g. auth.credentials on a host not logged
    into Codex, per doctor.go's doc comment). codex not on PATH is
    DOCTOR_SKIPPED, not a failure — nothing to gate on a codex-less host."""
    codex_path = shutil.which("codex")
    if not codex_path:
        return DoctorResult(DOCTOR_SKIPPED, "codex not found in PATH")

    try:
        proc = subprocess.run(
            [codex_path, "--strict-config", "doctor", "--json"],
            capture_output=True, timeout=30)
    except Exception as exc:
        raise RuntimeError(f"run {codex_path} --strict-config doctor --json: {exc}")

    out = proc.stdout
    if not out and proc.returncode != 0:
        raise RuntimeError(
            f"run {codex_path} --strict-config doctor --json: exit {proc.returncode}: {proc.stderr!r}")

    try:
        report = json.loads(out)
    except Exception as exc:
        raise RuntimeError(f"parse codex doctor --json output: {exc}")

    checks = report.get("checks") or {}
    check = checks.get("config.load")
    if check is None:
        raise RuntimeError('codex doctor --json output missing checks["config.load"]')
    if check.get("status") != "ok":
        return DoctorResult(DOCTOR_FAILED, check.get("summary", ""), check.get("remediation", ""))
    return DoctorResult(DOCTOR_OK, check.get("summary", ""))


def _write_gated(path: str, content: str, what: str) -> None:
    """Shared write-then-doctor-gate-then-rollback-on-failure sequence used
    by every config.toml writer below — mirrors WriteMCPServers/
    WriteOtelConfig/WriteHooks's identical tail in the Go package."""
    backup_path = backup(path)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(content)

    result = run_doctor_gate()
    if result.status == DOCTOR_FAILED:
        restore(backup_path, path)
        raise RuntimeError(
            f"codex config.toml {what} write rejected by doctor gate, rolled back: {result.summary}")


# ---------------------------------------------------------------------------
# MCP servers — WP-R1 ruling: direct-HTTP url form, SkipIfPresent
# ---------------------------------------------------------------------------

def _mcp_server_section_name(server_id: str) -> str:
    return "mcp_servers." + server_id


def _mcp_server_literal_header(server_id: str) -> str:
    return f"[mcp_servers.{server_id}]"


def _render_remote_mcp_server(server_id: str, url: str) -> str:
    lines = [f"[mcp_servers.{server_id}]"]
    lines.append(f"url = {_quote_toml_string(url)}")
    # Verified fix for `codex exec`'s silent-cancel-without-a-TTY behavior
    # (CODEX-INSTALL.md "MCP servers"); applies to the URL-transport form
    # exactly like the hub's stdio form — approval mode is orthogonal to
    # transport.
    lines.append('default_tools_approval_mode = "approve"')
    return "\n".join(lines) + "\n"


def write_mcp_servers(config_path: str, hub_server: str) -> bool:
    """Upserts [mcp_servers.activity] / [mcp_servers.wms] as direct-HTTP url
    registrations pointed at hookd (http://<hub_server>/mcp/<id>). Returns
    True iff anything changed (config.toml was rewritten + doctor-gated)."""
    try:
        with open(config_path) as f:
            content = f.read()
    except FileNotFoundError:
        content = ""

    changed = False
    for server_id in ("activity", "wms"):
        url = f"http://{hub_server}/mcp/{server_id}"
        body = _render_remote_mcp_server(server_id, url)
        content, result = upsert_section(
            content, _mcp_server_section_name(server_id), body,
            _mcp_server_literal_header(server_id), SKIP_IF_PRESENT)
        if result["unmarked_collision"]:
            print(f"WARNING: [mcp_servers.{server_id}] already defined outside Teamster's "
                  f"markers — left untouched, not wired to the hub", file=sys.stderr)
        if result["changed"]:
            changed = True

    if changed:
        _write_gated(config_path, content, "mcp_servers")
    return changed


# ---------------------------------------------------------------------------
# OTEL — remote codex in scope (operator decision); inline-table form ONLY
# ---------------------------------------------------------------------------

def _render_otel(environment: str, endpoint: str) -> str:
    lines = ["[otel]"]
    lines.append(f"environment = {_quote_toml_string(environment)}")
    lines.append("log_user_prompt = false")
    lines.append('exporter = "none"')
    lines.append(
        "metrics_exporter = { otlp-http = { endpoint = %s, protocol = %s } }"
        % (_quote_toml_string(endpoint), _quote_toml_string("binary")))
    lines.append('trace_exporter = "none"')
    return "\n".join(lines) + "\n"


def write_otel_config(config_path: str, environment: str, endpoint: str) -> bool:
    """AlwaysUpsert (a stale prior write must be replaced every run, never
    preserved — codexconfig/otel.go's OtelSpec doc comment). literalHeader
    guard leaves an operator's own unmarked [otel] table untouched."""
    try:
        with open(config_path) as f:
            content = f.read()
    except FileNotFoundError:
        content = ""

    body = _render_otel(environment, endpoint)
    content, result = upsert_section(content, "otel", body, "[otel]", ALWAYS_UPSERT)
    if result["unmarked_collision"]:
        print("WARNING: [otel] already defined outside Teamster's markers — "
              "left untouched, OTEL export is NOT configured on this remote", file=sys.stderr)
        return False
    if not result["changed"]:
        return False

    _write_gated(config_path, content, "[otel]")
    return True


# ---------------------------------------------------------------------------
# hooktrust port (internal/codexconfig/hooktrust.go) — BYTE-VERIFIED against
# the Go implementation in test_remote_codex_setup.py, not just structurally
# ported. See hooktrust.go's own doc comment for the full derivation and the
# codex-rs source references (rust-v0.137.0, hooks/src/engine/discovery.rs +
# config/src/fingerprint.rs).
# ---------------------------------------------------------------------------

_HOOK_EVENT_SNAKE = {
    "PreToolUse": "pre_tool_use",
    "PermissionRequest": "permission_request",
    "PostToolUse": "post_tool_use",
    "PreCompact": "pre_compact",
    "PostCompact": "post_compact",
    "SessionStart": "session_start",
    "UserPromptSubmit": "user_prompt_submit",
    "SubagentStart": "subagent_start",
    "SubagentStop": "subagent_stop",
    "Stop": "stop",
}


def hook_event_snake(event: str) -> str:
    try:
        return _HOOK_EVENT_SNAKE[event]
    except KeyError:
        raise ValueError(f"codexconfig: unknown hook event {event!r}")


def trusted_hash(event: str, matcher: str, command: str, timeout_sec: int) -> str:
    """Computes the sha256 trust hash Codex derives for one hook handler —
    see hooktrust.go's TrustedHash doc comment for the full derivation this
    ports byte-for-byte: a single-handler NormalizedHookIdentity, JSON keys
    in alphabetical order (event_name, hooks, matcher — matcher omitted
    when empty), each handler serialized as
    {"async":false,"command":...,"timeout":...,"type":"command"} (also
    alphabetical), compact (no spaces), no HTML-escaping, no trailing
    newline, then sha256'd and hex-encoded with a "sha256:" prefix.
    timeout is normalized to 600 if 0, then clamped to >= 1."""
    import hashlib

    snake = hook_event_snake(event)

    timeout = timeout_sec
    if timeout == 0:
        timeout = 600
    if timeout < 1:
        timeout = 1

    handler = {
        "async": False,
        "command": command,
        "timeout": timeout,
        "type": "command",
    }
    identity = {"event_name": snake, "hooks": [handler]}
    if matcher != "":
        identity["matcher"] = matcher

    encoded = json.dumps(identity, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    digest = hashlib.sha256(encoded.encode("utf-8")).hexdigest()
    return "sha256:" + digest


def hook_state_key(config_path: str, event: str, group_index: int, handler_index: int) -> str:
    """Builds the [hooks.state."..."] table name — the absolute config path
    embedded verbatim (never canonicalized/symlink-resolved), matching
    HookStateKey exactly. config_path must be the FINAL install-time path."""
    snake = hook_event_snake(event)
    return f"{config_path}:{snake}:{group_index}:{handler_index}"


DEFAULT_HOOK_TIMEOUT_SEC = 10


def _render_hook_registration(event: str, matcher: str, command: str, timeout_sec: int) -> str:
    lines = [f"[[hooks.{event}]]"]
    if matcher:
        lines.append(f"matcher = {_quote_toml_string(matcher)}")
    lines.append(f"[[hooks.{event}.hooks]]")
    lines.append('type = "command"')
    lines.append(f"command = {_quote_toml_string(command)}")
    lines.append(f"timeout = {timeout_sec}")
    return "\n".join(lines) + "\n"


def teamster_hook_specs(teamster_dir: str, hub_server: str, host: str,
                         timeout_sec: int = DEFAULT_HOOK_TIMEOUT_SEC):
    """Builds the three hooks Teamster registers on a remote — SessionStart,
    PreToolUse, PostToolUse — pointed at codex-hook.py via an explicit
    `python3 <path>` invocation (matches the hub's TeamsterHookSpecs, down to
    the explicit-interpreter rationale: removes any dependency on the
    installer preserving the executable bit).

    WP-R8 fix: codex's hook-handler TOML schema has NO `env` field (unlike
    `[mcp_servers.*]`, which carries a real inline env table) — codex-hook.py
    can only see whatever ambient process environment `codex` itself
    inherited when it spawned the hook. An interactive shell sources
    ~/.bashrc/~/.zshrc (remote-setup.sh's Step 5 puts
    TEAMSTER_HOOK_SERVER_URL/TEAMSTER_HOST there), so it worked in every
    manual test — but a non-interactive invocation (cron, `ssh host 'codex
    exec ...'`, CI) never sources that file, so codex-hook.py's own fallback
    (`_resolve_host_and_url` in codex-hook.py) silently resolves to the
    REMOTE's own hostname:9125 instead of the hub — the feed channel just
    goes silent, no operator-visible error (cost/sessions/WMS are
    unaffected; this is feed-only).

    Fix: make the hub URL and host EXPLICIT at install time instead of
    ambient at hook-fire time, by prefixing the command with `/usr/bin/env
    VAR=value ...` — wire-verified empirically (real codex 0.137.0, isolated
    CODEX_HOME, a live `codex exec` run) that codex's hook command parsing
    tokenizes `command` by whitespace and executes argv[0] directly (no
    shell, no `$VAR`/`${VAR}` expansion of its own) — so `env` is the
    correct standard mechanism, not a guess: it's a real executable that
    sets the given vars and executes the remaining argv, and this was
    confirmed to actually reach the spawned script's os.environ, byte for
    byte, for values shaped exactly like a real hub address
    (`http://host.example.com:9125/event`) and a real short hostname
    (`remote-box-01`). This also means values must never contain whitespace
    (nothing here does by default: hub_server is `host:port`, host is a
    short hostname, teamster_dir defaults to ~/teamster) — a bare
    `env`-prefixed command has no quoting mechanism for that, unlike a
    shell; a space anywhere in hub_server/host/teamster_dir warns rather
    than silently producing a hook whose argv splits wrong.

    No second copy of config to drift: hub_server/host are the same values
    already threaded through main() from --server/--host (or the same
    hostname-resolution fallback socket.gethostname().split(".")[0] uses
    elsewhere in this file) — nothing new is introduced that could disagree
    with what write_mcp_servers/AGENTS.md already use.
    """
    if (any(c.isspace() for c in hub_server) or any(c.isspace() for c in host)
            or any(c.isspace() for c in teamster_dir)):
        print(f"WARNING: --server, --host, or --teamster-dir contains "
              f"whitespace — codex tokenizes the hook command by "
              f"whitespace (no shell), so this will not parse correctly: "
              f"server={hub_server!r} host={host!r} teamster_dir={teamster_dir!r}",
              file=sys.stderr)

    command = (
        "/usr/bin/env "
        f"TEAMSTER_HOOK_SERVER_URL=http://{hub_server}/event "
        f"TEAMSTER_HOST={host} "
        "python3 " + os.path.join(teamster_dir, "lib", "hook", "codex-hook.py")
    )
    return [
        {"event": event, "matcher": ".*", "command": command, "timeout_sec": timeout_sec}
        for event in ("SessionStart", "PreToolUse", "PostToolUse")
    ]


def write_hooks(config_path: str, specs) -> bool:
    """Upserts hook registrations AND their trust-state blocks, both
    AlwaysUpsert (WP8/hooks.go's rationale: Codex silently invalidates trust
    the instant a hook definition changes, so re-deriving + re-writing both
    every run is what makes an upgrade self-heal instead of leaving hooks
    silently inert)."""
    try:
        with open(config_path) as f:
            content = f.read()
    except FileNotFoundError:
        content = ""

    hooks_body = []
    state_body = ["[hooks.state]", ""]
    for spec in specs:
        hooks_body.append(_render_hook_registration(
            spec["event"], spec["matcher"], spec["command"], spec["timeout_sec"]))
        hooks_body.append("")

        key = hook_state_key(config_path, spec["event"], 0, 0)
        h = trusted_hash(spec["event"], spec["matcher"], spec["command"], spec["timeout_sec"])
        state_body.append(f"[hooks.state.{_quote_toml_string(key)}]")
        state_body.append(f"trusted_hash = {_quote_toml_string(h)}")
        state_body.append("")

    content, hooks_result = upsert_section(content, "hooks", "\n".join(hooks_body), "", ALWAYS_UPSERT)
    content, state_result = upsert_section(content, "hooks-state", "\n".join(state_body), "", ALWAYS_UPSERT)

    if not hooks_result["changed"] and not state_result["changed"]:
        return False

    _write_gated(config_path, content, "hooks")
    return True


# ---------------------------------------------------------------------------
# Skills — full remove-then-copy per directory (skills.go's InstallSkills)
# ---------------------------------------------------------------------------

def _remove_all(path: str) -> None:
    """Mirrors Go's os.RemoveAll: removes path whether it's a symlink, a
    plain file, or a directory tree; a no-op if it doesn't exist."""
    if os.path.islink(path) or os.path.isfile(path):
        os.remove(path)
    elif os.path.isdir(path):
        shutil.rmtree(path)


def install_skills(src_skills_dir: str, codex_home: str):
    """Copies each skill directory under src_skills_dir into
    <codex_home>/skills/<name>/, overwriting any previous Teamster-owned
    copy — no marker merge, skills are pure generated content (no
    operator-hand-edit case worth preserving), matches InstallSkills."""
    dest_root = os.path.join(codex_home, "skills")
    os.makedirs(dest_root, exist_ok=True)

    installed = []
    for name in sorted(os.listdir(src_skills_dir)):
        src = os.path.join(src_skills_dir, name)
        if not os.path.isdir(src):
            continue
        dst = os.path.join(dest_root, name)
        _remove_all(dst)
        shutil.copytree(src, dst, symlinks=True)
        installed.append(name)
    return installed


# ---------------------------------------------------------------------------
# AGENTS.md merge — reads the SAME shared data file the Go installer reads
# (LESSONS.md §1: single-sourced, never a second copy of this text).
# ---------------------------------------------------------------------------

CODEX_AGENTS_MARKER = "## Getting Started with Teamster (Codex)"


def merge_codex_agents_md(teamster_dir: str, codex_home: str) -> None:
    protocol_path = os.path.join(teamster_dir, "lib", "codex-plugin", "agents-protocol.md")
    with open(protocol_path) as f:
        protocol = f.read()

    target = os.path.join(codex_home, "AGENTS.md")
    override_path = os.path.join(codex_home, "AGENTS.override.md")
    using_override = False
    if os.path.exists(override_path):
        target = override_path
        using_override = True

    existing = ""
    if os.path.exists(target):
        with open(target) as f:
            existing = f.read()

    if CODEX_AGENTS_MARKER in existing:
        return  # already merged — idempotent no-op

    if using_override:
        print(f"Note: {override_path} exists and fully overrides AGENTS.md on this "
              f"Codex install -- merging Teamster's protocol there instead so it "
              f"actually takes effect.")

    content = existing
    if content:
        content = content.rstrip("\n") + "\n"
    content += protocol

    os.makedirs(os.path.dirname(target), exist_ok=True)
    backup(target)
    with open(target, "w") as f:
        f.write(content)


# ---------------------------------------------------------------------------
# CODEX_HOME resolution — mirrors DefaultConfigPath (codexconfig/mcpserver.go)
# ---------------------------------------------------------------------------

def default_config_path(home: str) -> str:
    codex_home = os.environ.get("CODEX_HOME", "")
    if codex_home:
        return os.path.join(codex_home, "config.toml")
    return os.path.join(home, ".codex", "config.toml")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main() -> int:
    parser = argparse.ArgumentParser(
        description="remote-codex-setup: wire Codex CLI config on a Teamster remote")
    parser.add_argument("--server", required=True, help="hub address host:port")
    parser.add_argument("--teamster-dir", default=os.path.join(os.path.expanduser("~"), "teamster"))
    parser.add_argument("--codex-mode", choices=("auto", "install", "none"), default="auto")
    parser.add_argument("--otel-codex-port", type=int, default=0)
    parser.add_argument("--otel-environment", default="production")
    # WP-R8: the short hostname this remote's events should report under —
    # baked into the hook command explicitly (see teamster_hook_specs) rather
    # than left for codex-hook.py to resolve ambiently at hook-fire time.
    # Falls back to the same short-hostname convention teamster.py/
    # token-scraper.py already use when the caller doesn't pass one.
    parser.add_argument("--host", default="")
    args = parser.parse_args()

    if args.codex_mode == "none":
        print("Codex wiring skipped (--codex-mode=none)")
        print("CODEX_STATUS=skipped")
        return 0

    codex_path = shutil.which("codex")
    if not codex_path:
        if args.codex_mode == "install":
            print("ERROR: --codex-mode=install requires codex on PATH, but it was not found",
                  file=sys.stderr)
            return 1
        print("codex not detected on PATH — skipping Codex wiring (auto-detect)")
        print("CODEX_STATUS=skipped")
        return 0

    version = "unknown"
    try:
        out = subprocess.run([codex_path, "--version"], capture_output=True, timeout=10)
        fields = out.stdout.decode("utf-8", "replace").split()
        if fields:
            version = fields[-1]
    except Exception:
        pass

    home = os.path.expanduser("~")
    config_path = default_config_path(home)
    codex_home = os.path.dirname(config_path)
    teamster_dir = args.teamster_dir
    host = args.host or socket.gethostname().split(".")[0]

    try:
        write_mcp_servers(config_path, args.server)

        if args.otel_codex_port:
            endpoint = f"http://{args.server.split(':')[0]}:{args.otel_codex_port}/"
            write_otel_config(config_path, args.otel_environment, endpoint)

        skills_src = os.path.join(teamster_dir, "lib", "codex-plugin", "skills")
        if os.path.isdir(skills_src):
            install_skills(skills_src, codex_home)
        else:
            print(f"WARNING: {skills_src} not found — skipping Codex skills install", file=sys.stderr)

        merge_codex_agents_md(teamster_dir, codex_home)

        specs = teamster_hook_specs(teamster_dir, args.server, host)
        write_hooks(config_path, specs)
    except Exception as exc:
        print(f"ERROR: Codex wiring failed: {exc}", file=sys.stderr)
        return 1

    print(f"Codex wiring complete (codex {version})")
    print("CODEX_STATUS=wired")
    return 0


if __name__ == "__main__":
    sys.exit(main())

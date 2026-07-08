#!/usr/bin/env python3
"""Unit tests for remote-codex-setup.py — the Python port of
internal/codexconfig. Pure-stdlib (unittest), synthetic fixtures only.

The TrustedHash/HookStateKey tests below are BYTE-VERIFIED against reference
values generated directly from the Go implementation (internal/codexconfig's
TrustedHash + HookStateKey), not just structurally similar — see the
docstring on TestTrustedHash.MatchesGoReference for how the golden values
were produced.

Run with: python3 test_remote_codex_setup.py
"""
from __future__ import annotations

import importlib.util
import os
import shutil
import tempfile
import unittest

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "remote_codex_setup", os.path.join(_HERE, "remote-codex-setup.py"))
rcs = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(rcs)


def _read(path):
    with open(path) as f:
        return f.read()


class TempDirCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.mkdtemp(prefix="rcs-test-")

    def tearDown(self):
        shutil.rmtree(self._tmp, ignore_errors=True)

    def path(self, *parts):
        return os.path.join(self._tmp, *parts)


# ---------------------------------------------------------------------------
# upsert_section / remove_section (sectionpatch.go port)
# ---------------------------------------------------------------------------

class TestUpsertSection(unittest.TestCase):
    def test_operator_comments_survive_three_runs(self):
        content = "# operator's own comment\nmodel = \"gpt-5.5\"\n"
        for _ in range(3):
            content, result = rcs.upsert_section(
                content, "mcp_servers.wms", "[mcp_servers.wms]\nurl = \"http://hub:9125/mcp/wms\"\n",
                "[mcp_servers.wms]", rcs.SKIP_IF_PRESENT)
        self.assertIn("# operator's own comment", content)
        self.assertEqual(content.count("[mcp_servers.wms]"), 1)

    def test_skip_if_present_does_not_touch_existing_marked_block(self):
        first, _ = rcs.upsert_section("", "otel", "[otel]\nenvironment = \"production\"\n", "[otel]", rcs.ALWAYS_UPSERT)
        # Operator hand-edits inside the marker span.
        hand_edited = first.replace('"production"', '"staging"')
        second, result = rcs.upsert_section(hand_edited, "otel", "[otel]\nenvironment = \"production\"\n", "[otel]", rcs.SKIP_IF_PRESENT)
        self.assertTrue(result["skipped_existing"])
        self.assertIn('"staging"', second)

    def test_always_upsert_replaces_in_place(self):
        first, _ = rcs.upsert_section("", "otel", "[otel]\nenvironment = \"production\"\n", "[otel]", rcs.ALWAYS_UPSERT)
        second, result = rcs.upsert_section(first, "otel", "[otel]\nenvironment = \"staging\"\n", "[otel]", rcs.ALWAYS_UPSERT)
        self.assertTrue(result["changed"])
        self.assertIn('"staging"', second)
        self.assertNotIn('"production"', second)

    def test_unmarked_collision_never_touches_foreign_content(self):
        content = "[mcp_servers.wms]\ncommand = \"my-own-server\"\n"
        new_content, result = rcs.upsert_section(
            content, "mcp_servers.wms", "[mcp_servers.wms]\nurl = \"http://hub:9125/mcp/wms\"\n",
            "[mcp_servers.wms]", rcs.SKIP_IF_PRESENT)
        self.assertTrue(result["unmarked_collision"])
        self.assertEqual(content, new_content)

    def test_remove_section_restores_byte_for_byte(self):
        original = "model = \"gpt-5.5\"\n"
        with_block, _ = rcs.upsert_section(original, "otel", "[otel]\nenvironment = \"production\"\n", "[otel]", rcs.ALWAYS_UPSERT)
        removed = rcs.remove_section(with_block, "otel")
        self.assertEqual(removed.strip("\n"), original.strip("\n"))

    def test_remove_section_no_op_when_absent(self):
        content = "model = \"gpt-5.5\"\n"
        self.assertEqual(rcs.remove_section(content, "otel"), content)

    def test_collapse_blank_runs(self):
        self.assertEqual(rcs._collapse_blank_runs("a\n\n\n\nb"), "a\n\nb")

    def test_contains_line_full_line_match_only(self):
        content = "# this comment mentions [mcp_servers.wms] in prose\nx = 1\n"
        self.assertFalse(rcs._contains_line(content, "[mcp_servers.wms]"))
        self.assertTrue(rcs._contains_line("[mcp_servers.wms]\nurl = \"x\"\n", "[mcp_servers.wms]"))


class TestQuoteTOMLString(unittest.TestCase):
    def test_escapes_backslash_and_quote(self):
        self.assertEqual(rcs._quote_toml_string('a"b\\c'), '"a\\"b\\\\c"')

    def test_plain_path_unquoted_inside(self):
        s = "/home/alice/teamster/lib/hook/codex-hook.py"
        self.assertEqual(rcs._quote_toml_string(s), f'"{s}"')


# ---------------------------------------------------------------------------
# MCP servers (WP-R1 direct-HTTP url form)
# ---------------------------------------------------------------------------

class TestRenderRemoteMcpServer(unittest.TestCase):
    def test_url_form_and_approval_mode(self):
        body = rcs._render_remote_mcp_server("wms", "http://hub.example.com:9125/mcp/wms")
        self.assertIn("[mcp_servers.wms]", body)
        self.assertIn('url = "http://hub.example.com:9125/mcp/wms"', body)
        self.assertIn('default_tools_approval_mode = "approve"', body)
        self.assertNotIn("command", body)


class TestWriteMcpServers(TempDirCase):
    def test_fresh_file_writes_both_servers(self):
        config_path = self.path("config.toml")
        changed = rcs.write_mcp_servers(config_path, "hub.example.com:9125")
        self.assertTrue(changed)
        content = _read(config_path)
        self.assertIn('url = "http://hub.example.com:9125/mcp/activity"', content)
        self.assertIn('url = "http://hub.example.com:9125/mcp/wms"', content)

    def test_rerun_is_idempotent_and_preserves_operator_content(self):
        config_path = self.path("config.toml")
        with open(config_path, "w") as f:
            f.write("# my own notes\n")
        rcs.write_mcp_servers(config_path, "hub.example.com:9125")
        first = _read(config_path)
        changed = rcs.write_mcp_servers(config_path, "hub.example.com:9125")
        second = _read(config_path)
        self.assertFalse(changed)
        self.assertEqual(first, second)
        self.assertIn("# my own notes", second)

    def test_no_op_write_skips_backup(self):
        config_path = self.path("config.toml")
        rcs.write_mcp_servers(config_path, "hub.example.com:9125")
        # First write: config.toml did not exist beforehand, so backup()
        # found nothing to preserve — no .pre-teamster is fabricated
        # (installbackup's "nothing to preserve" case, backup.go's
        # TestBackup_NonexistentPathIsNoOp).
        self.assertFalse(os.path.exists(config_path + ".pre-teamster"))
        # Second run: nothing changes, so no NEW timestamped backup should
        # be created beyond none-yet-existing.
        backups_before = [f for f in os.listdir(self._tmp) if f.endswith(".bak")]
        rcs.write_mcp_servers(config_path, "hub.example.com:9125")
        backups_after = [f for f in os.listdir(self._tmp) if f.endswith(".bak")]
        self.assertEqual(backups_before, backups_after)


# ---------------------------------------------------------------------------
# OTEL
# ---------------------------------------------------------------------------

class TestRenderOtel(unittest.TestCase):
    def test_matches_verified_shape(self):
        body = rcs._render_otel("production", "http://hub.example.com:4329/")
        self.assertIn('environment = "production"', body)
        self.assertIn("log_user_prompt = false", body)
        self.assertIn('exporter = "none"', body)
        self.assertIn('trace_exporter = "none"', body)
        self.assertIn(
            'metrics_exporter = { otlp-http = { endpoint = "http://hub.example.com:4329/", protocol = "binary" } }',
            body)


class TestWriteOtelConfig(TempDirCase):
    def test_fresh_file(self):
        config_path = self.path("config.toml")
        changed = rcs.write_otel_config(config_path, "production", "http://hub:4329/")
        self.assertTrue(changed)
        self.assertIn("[otel]", _read(config_path))

    def test_rerun_replaces_in_place(self):
        config_path = self.path("config.toml")
        rcs.write_otel_config(config_path, "production", "http://hub:4329/")
        rcs.write_otel_config(config_path, "staging", "http://hub:4400/")
        content = _read(config_path)
        self.assertIn('environment = "staging"', content)
        self.assertNotIn('environment = "production"', content)
        self.assertEqual(content.count("[otel]"), 1)

    def test_unmarked_collision_leaves_foreign_otel_untouched(self):
        config_path = self.path("config.toml")
        with open(config_path, "w") as f:
            f.write("[otel]\nenvironment = \"operator-defined\"\n")
        changed = rcs.write_otel_config(config_path, "production", "http://hub:4329/")
        self.assertFalse(changed)
        self.assertIn("operator-defined", _read(config_path))


# ---------------------------------------------------------------------------
# hooktrust — BYTE-VERIFIED against internal/codexconfig's TrustedHash and
# HookStateKey. Golden values below were generated by a one-shot Go test
# (TestZZZGeneratePythonReferenceHashes in
# src/internal/codexconfig/zzz_pygen_test.go, deleted after use — not part
# of the shipped test suite) that called TrustedHash/HookStateKey directly
# and dumped their output. Reproducing here proves this Python port derives
# byte-identical trust hashes to the Go installer for the same inputs — a
# mismatch means a Codex-trusted hook on the hub would come up UNTRUSTED
# (or vice versa) on a remote for the exact same registration.
# ---------------------------------------------------------------------------

_GOLDEN_CASES = [
    # (event, matcher, command, timeout_sec, config_path, expected_hash, expected_state_key)
    ("SessionStart", ".*", "python3 /home/alice/teamster/lib/hook/codex-hook.py", 10,
     "/home/alice/.codex/config.toml",
     "sha256:1aeb3b44dedca33df18e848f4e29f57a4053f3f817f5abb9116adaff2d288552",
     "/home/alice/.codex/config.toml:session_start:0:0"),
    ("PreToolUse", ".*", "python3 /home/alice/teamster/lib/hook/codex-hook.py", 10,
     "/home/alice/.codex/config.toml",
     "sha256:d3402508ee53768e4b03235f006d34162cf1adda6037b192122b577c8d5d514a",
     "/home/alice/.codex/config.toml:pre_tool_use:0:0"),
    ("PostToolUse", ".*", "python3 /home/alice/teamster/lib/hook/codex-hook.py", 10,
     "/home/alice/.codex/config.toml",
     "sha256:f6a546c4a7fb5b53abb60851b71f7c1c58d481f7b7a94f8093249cb7e0e29c11",
     "/home/alice/.codex/config.toml:post_tool_use:0:0"),
    ("SessionStart", "", "python3 /x/codex-hook.py", 0,
     "/home/bob/.codex/config.toml",
     "sha256:91ec797ae76e3a242f3a0e3a0497a152a4d300da4752950b68d854cb8682a02f",
     "/home/bob/.codex/config.toml:session_start:0:0"),
    ("PreToolUse", ".*", "python3 /x/codex-hook.py", -5,
     "/home/bob/.codex/config.toml",
     "sha256:f10def50ebb4ff714d67e5c709f3c64dcbe847f2361126afe5eb2b2e2db0633f",
     "/home/bob/.codex/config.toml:pre_tool_use:0:0"),
    ("PostToolUse", ".*", "python3 /Users/carol/teamster/lib/hook/codex-hook.py", 10,
     "/Users/carol/.codex/config.toml",
     "sha256:b87e53473b10a29977f804eca2151d6d707ea6bb567784e8c457e54dc78c6a7e",
     "/Users/carol/.codex/config.toml:post_tool_use:0:0"),
]


class TestTrustedHashMatchesGoReference(unittest.TestCase):
    def test_byte_identical_to_go_implementation(self):
        for event, matcher, command, timeout_sec, config_path, expected_hash, expected_key in _GOLDEN_CASES:
            with self.subTest(event=event, command=command, timeout_sec=timeout_sec):
                got_hash = rcs.trusted_hash(event, matcher, command, timeout_sec)
                self.assertEqual(got_hash, expected_hash)
                got_key = rcs.hook_state_key(config_path, event, 0, 0)
                self.assertEqual(got_key, expected_key)

    def test_unknown_event_raises(self):
        with self.assertRaises(ValueError):
            rcs.trusted_hash("NotARealEvent", ".*", "cmd", 10)

    def test_path_independent_hash_but_path_dependent_key(self):
        # Same definition, two different config paths: hash must match
        # (hooktrust.go's TrustedHash never takes the config path as
        # input); the state KEY must differ (it embeds the path).
        h1 = rcs.trusted_hash("SessionStart", ".*", "python3 /a/codex-hook.py", 10)
        h2 = rcs.trusted_hash("SessionStart", ".*", "python3 /a/codex-hook.py", 10)
        self.assertEqual(h1, h2)
        k1 = rcs.hook_state_key("/path/one/config.toml", "SessionStart", 0, 0)
        k2 = rcs.hook_state_key("/path/two/config.toml", "SessionStart", 0, 0)
        self.assertNotEqual(k1, k2)


class TestWriteHooks(TempDirCase):
    def test_registers_three_hooks_with_trust_state(self):
        config_path = self.path("config.toml")
        specs = rcs.teamster_hook_specs(self.path("teamster"), "hub.example.com:9125", "remote-box-01")
        changed = rcs.write_hooks(config_path, specs)
        self.assertTrue(changed)
        content = _read(config_path)
        for event in ("SessionStart", "PreToolUse", "PostToolUse"):
            self.assertIn(f"[[hooks.{event}]]", content)
        self.assertIn("[hooks.state]", content)
        self.assertIn("trusted_hash =", content)

    def test_rerun_self_heals_after_definition_change(self):
        config_path = self.path("config.toml")
        specs = rcs.teamster_hook_specs(self.path("teamster"), "hub.example.com:9125", "remote-box-01", timeout_sec=10)
        rcs.write_hooks(config_path, specs)
        first_hash_line = [l for l in _read(config_path).splitlines() if "trusted_hash" in l][0]

        specs2 = rcs.teamster_hook_specs(self.path("teamster"), "hub.example.com:9125", "remote-box-01", timeout_sec=20)
        rcs.write_hooks(config_path, specs2)
        second_hash_line = [l for l in _read(config_path).splitlines() if "trusted_hash" in l][0]
        self.assertNotEqual(first_hash_line, second_hash_line)


class TestTeamsterHookSpecsEnvInjection(unittest.TestCase):
    """WP-R8: codex's hook-handler TOML schema has no `env` field, so
    codex-hook.py can only see whatever ambient process environment `codex`
    inherited when it spawned the hook — correct in an interactive shell
    (sources ~/.bashrc), silently wrong in a non-interactive one (cron, `ssh
    host 'codex exec ...'`, CI never sources it), where codex-hook.py's own
    fallback resolves to the REMOTE's own hostname instead of the hub and
    the feed channel vanishes with no operator-visible error. The fix makes
    the hub URL and host explicit at install time via an `/usr/bin/env
    VAR=value ...` command prefix — wire-verified empirically (real codex
    0.137.0, isolated CODEX_HOME, a live `codex exec` run) to actually reach
    the spawned hook's os.environ; these tests check the STRING the
    installer writes, not codex's own behavior (that's the wire-verification
    this docstring describes, done once by hand, not repeated here)."""

    def test_command_embeds_explicit_hub_url_and_host(self):
        specs = rcs.teamster_hook_specs("/home/alice/teamster", "hub.example.com:9125", "remote-box-01")
        for spec in specs:
            self.assertIn("/usr/bin/env", spec["command"])
            self.assertIn("TEAMSTER_HOOK_SERVER_URL=http://hub.example.com:9125/event", spec["command"])
            self.assertIn("TEAMSTER_HOST=remote-box-01", spec["command"])
            self.assertIn("python3 /home/alice/teamster/lib/hook/codex-hook.py", spec["command"])

    def test_no_reliance_on_ambient_environment(self):
        # The whole point: the command string alone (no external env, no
        # settings.json, no ~/.bashrc) must carry everything codex-hook.py
        # needs. Assert the two required vars appear as literal
        # VAR=value tokens in the command string itself.
        specs = rcs.teamster_hook_specs("/x", "myhub:9125", "myhost")
        command = specs[0]["command"]
        tokens = command.split()
        self.assertIn("TEAMSTER_HOOK_SERVER_URL=http://myhub:9125/event", tokens)
        self.assertIn("TEAMSTER_HOST=myhost", tokens)

    def test_whitespace_in_values_warns(self):
        import io
        import contextlib
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            rcs.teamster_hook_specs("/x", "hub with space:9125", "myhost")
        self.assertIn("WARNING", stderr.getvalue())

    def test_whitespace_in_teamster_dir_warns(self):
        # Reviewer catch: a space in a custom --teamster-dir would split the
        # codex-hook.py path across two argv tokens too (same no-shell
        # whitespace tokenization as hub_server/host) — the guard must cover
        # all three interpolated values, not just the first two.
        import io
        import contextlib
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            rcs.teamster_hook_specs("/path with space/teamster", "myhub:9125", "myhost")
        self.assertIn("WARNING", stderr.getvalue())


# ---------------------------------------------------------------------------
# Skills
# ---------------------------------------------------------------------------

class TestInstallSkills(TempDirCase):
    def test_copies_skill_directories(self):
        src = self.path("skills-src")
        os.makedirs(os.path.join(src, "teamster-status"))
        with open(os.path.join(src, "teamster-status", "SKILL.md"), "w") as f:
            f.write("---\nname: teamster-status\n---\nbody\n")
        codex_home = self.path("codex-home")
        installed = rcs.install_skills(src, codex_home)
        self.assertEqual(installed, ["teamster-status"])
        self.assertTrue(os.path.exists(os.path.join(codex_home, "skills", "teamster-status", "SKILL.md")))

    def test_overwrites_stale_copy(self):
        src = self.path("skills-src")
        os.makedirs(os.path.join(src, "start"))
        with open(os.path.join(src, "start", "SKILL.md"), "w") as f:
            f.write("v2\n")
        codex_home = self.path("codex-home")
        stale_dir = os.path.join(codex_home, "skills", "start")
        os.makedirs(stale_dir)
        with open(os.path.join(stale_dir, "SKILL.md"), "w") as f:
            f.write("v1-stale\n")
        with open(os.path.join(stale_dir, "orphan.txt"), "w") as f:
            f.write("should be removed\n")

        rcs.install_skills(src, codex_home)
        self.assertEqual(_read(os.path.join(stale_dir, "SKILL.md")), "v2\n")
        self.assertFalse(os.path.exists(os.path.join(stale_dir, "orphan.txt")))


# ---------------------------------------------------------------------------
# AGENTS.md merge
# ---------------------------------------------------------------------------

class TestMergeCodexAgentsMd(TempDirCase):
    def _teamster_dir_with_protocol(self):
        teamster_dir = self.path("teamster")
        plugin_dir = os.path.join(teamster_dir, "lib", "codex-plugin")
        os.makedirs(plugin_dir)
        real = os.path.join(_HERE, "..", "codex-plugin", "agents-protocol.md")
        with open(real) as f:
            text = f.read()
        with open(os.path.join(plugin_dir, "agents-protocol.md"), "w") as f:
            f.write(text)
        return teamster_dir

    def test_creates_when_absent(self):
        teamster_dir = self._teamster_dir_with_protocol()
        codex_home = self.path("codex-home")
        rcs.merge_codex_agents_md(teamster_dir, codex_home)
        content = _read(os.path.join(codex_home, "AGENTS.md"))
        self.assertIn(rcs.CODEX_AGENTS_MARKER, content)
        self.assertNotIn("Eight Rules", content)
        self.assertFalse(os.path.exists(os.path.join(codex_home, "AGENTS.md.pre-teamster")))

    def test_backs_up_before_write(self):
        teamster_dir = self._teamster_dir_with_protocol()
        codex_home = self.path("codex-home")
        os.makedirs(codex_home)
        original = "# My own Codex notes\n"
        path = os.path.join(codex_home, "AGENTS.md")
        with open(path, "w") as f:
            f.write(original)

        rcs.merge_codex_agents_md(teamster_dir, codex_home)
        self.assertEqual(_read(path + ".pre-teamster"), original)
        merged = _read(path)
        self.assertIn(original, merged)
        self.assertIn(rcs.CODEX_AGENTS_MARKER, merged)

    def test_no_op_when_already_merged(self):
        teamster_dir = self._teamster_dir_with_protocol()
        codex_home = self.path("codex-home")
        os.makedirs(codex_home)
        protocol = _read(os.path.join(teamster_dir, "lib", "codex-plugin", "agents-protocol.md"))
        path = os.path.join(codex_home, "AGENTS.md")
        with open(path, "w") as f:
            f.write(protocol)

        rcs.merge_codex_agents_md(teamster_dir, codex_home)
        self.assertFalse(os.path.exists(path + ".pre-teamster"))

    def test_prefers_override_when_present(self):
        teamster_dir = self._teamster_dir_with_protocol()
        codex_home = self.path("codex-home")
        os.makedirs(codex_home)
        agents_path = os.path.join(codex_home, "AGENTS.md")
        override_path = os.path.join(codex_home, "AGENTS.override.md")
        with open(agents_path, "w") as f:
            f.write("# base — should not be touched\n")
        with open(override_path, "w") as f:
            f.write("# operator override\n")

        rcs.merge_codex_agents_md(teamster_dir, codex_home)
        self.assertEqual(_read(agents_path), "# base — should not be touched\n")
        self.assertIn(rcs.CODEX_AGENTS_MARKER, _read(override_path))


# ---------------------------------------------------------------------------
# CODEX_HOME resolution
# ---------------------------------------------------------------------------

class TestDefaultConfigPath(unittest.TestCase):
    def test_uses_codex_home_env_when_set(self):
        old = os.environ.get("CODEX_HOME")
        try:
            os.environ["CODEX_HOME"] = "/opt/codexhome"
            self.assertEqual(rcs.default_config_path("/home/x"), "/opt/codexhome/config.toml")
        finally:
            if old is None:
                os.environ.pop("CODEX_HOME", None)
            else:
                os.environ["CODEX_HOME"] = old

    def test_defaults_to_dot_codex_under_home(self):
        old = os.environ.pop("CODEX_HOME", None)
        try:
            self.assertEqual(rcs.default_config_path("/home/x"), "/home/x/.codex/config.toml")
        finally:
            if old is not None:
                os.environ["CODEX_HOME"] = old


if __name__ == "__main__":
    unittest.main()

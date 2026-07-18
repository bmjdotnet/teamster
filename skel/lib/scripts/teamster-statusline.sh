#!/usr/bin/env bash
# teamster-statusline.sh — forwards Claude Code statusLine / subagentStatusLine
# JSON to hookd's POST /context endpoint for exact, plan-aware context-window
# sizing, then chains to any previously-configured statusLine command so the
# operator's own display is unaffected.
#
# Wired into ~/.claude/settings.json's statusLine.command and
# subagentStatusLine.command by the installer's non-destructive merge (same
# chain-the-existing-command pattern used for hooks). This one script serves
# both slots — it tells them apart by payload shape (.context_window vs
# .tasks), same test used for forwarding below. TEAMSTER_STATUSLINE_CHAIN and
# TEAMSTER_SUBAGENT_STATUSLINE_CHAIN each carry the operator's prior command
# for that slot, if one existed before install (they can differ, so two
# separate vars rather than one shared one).
#
# Must never block or fail the visible status line: every external call is
# best-effort, backgrounded, and timeout-bounded. No `set -e` — a failed curl
# or unexpected JSON shape must degrade silently, not blank the status line.

INPUT="$(cat)"
[ -z "$INPUT" ] && exit 0

SESSION_ID="${TEAMSTER_SESSION_ID:-}"
if [ -z "$SESSION_ID" ]; then
    # Prefer CLAUDE_CODE_SESSION_ID (per-session, set in every session's env)
    # over current-session-id (shared file, racy between concurrent sessions).
    SESSION_ID="${CLAUDE_CODE_SESSION_ID:-}"
fi
if [ -z "$SESSION_ID" ]; then
    SESSION_ID="$(cat ~/.claude/current-session-id 2>/dev/null || true)"
fi
AGENT_NAME="${TEAMSTER_AGENT_NAME:-}"
HOST="${TEAMSTER_HOST:-$(hostname -s 2>/dev/null || echo unknown)}"
HOOKD_URL="${TEAMSTER_HOOK_SERVER_URL:-http://localhost:9125/event}"
CONTEXT_URL="${HOOKD_URL%/event}/context"

have_jq=1
command -v jq >/dev/null 2>&1 || have_jq=0
have_curl=1
command -v curl >/dev/null 2>&1 || have_curl=0

# post_context sends one canonical flat JSON body (already shaped for
# POST /context) in the background with a short timeout, so a slow or
# unreachable hookd never delays the visible status line render.
post_context() {
    echo "$1" | curl -s -m 3 -X POST -H "Content-Type: application/json" \
        -d @- "$CONTEXT_URL" >/dev/null 2>&1 &
}

# Which slot is this invocation? Same payload-shape test drives both
# forwarding (below) and chain-command selection (further down) — Claude
# Code gives no other signal distinguishing statusLine from
# subagentStatusLine invocations.
is_main=0
is_subagent=0
if [ "$have_jq" = "1" ]; then
    echo "$INPUT" | jq -e '.context_window' >/dev/null 2>&1 && is_main=1
    echo "$INPUT" | jq -e '.tasks' >/dev/null 2>&1 && is_subagent=1
fi

if [ -n "$SESSION_ID" ] && [ "$have_jq" = "1" ] && [ "$have_curl" = "1" ]; then
    if [ "$is_main" = "1" ]; then
        # Main statusLine: one flat context_window object per tick.
        BODY="$(echo "$INPUT" | jq -c --arg sid "$SESSION_ID" --arg agent "$AGENT_NAME" --arg host "$HOST" '
            {
                session_id: $sid,
                agent_name: $agent,
                host: $host,
                context_window_size: (.context_window.context_window_size // 0),
                used_percentage: (.context_window.used_percentage // 0),
                total_input_tokens: (.context_window.total_input_tokens // 0),
                session_cost_usd: (.cost.total_cost_usd // 0),
                statusline_json: ({
                    cache_read_input_tokens: (.context_window.current_usage.cache_read_input_tokens // 0),
                    cache_creation_input_tokens: (.context_window.current_usage.cache_creation_input_tokens // 0),
                    output_tokens: (.context_window.current_usage.output_tokens // 0)
                } | tojson)
            }' 2>/dev/null)"
        [ -n "$BODY" ] && post_context "$BODY"
    elif [ "$is_subagent" = "1" ]; then
        # subagentStatusLine: one task per active teammate. tokenCount is
        # assumed to be the same input-token accounting as the main
        # statusLine's total_input_tokens — not independently confirmed by
        # Claude Code's docs (context-window-detection.md flags the subagent
        # schema as "not chased further"). used_percentage is computed here
        # since the per-task payload has no pre-computed one. No per-task
        # cost field exists in this schema, so session_cost_usd is omitted
        # here (defaults to 0 server-side) — cost is session-wide and only
        # available from the main statusLine's top-level .cost object, which
        # correlates with the lead's own gauge row (agent_name ""), not any
        # individual teammate's. model IS included here, unlike cost — it's
        # the authoritative per-task model Claude Code itself resolved,
        # which the /context handler uses to override token_ledger's
        # buggier per-teammate model attribution.
        echo "$INPUT" | jq -c --arg sid "$SESSION_ID" --arg host "$HOST" '
            .tasks[]? | select((.name // "") != "") | {
                session_id: $sid,
                agent_name: ("@" + .name),
                host: $host,
                context_window_size: (.contextWindowSize // 0),
                used_percentage: (if (.contextWindowSize // 0) > 0 then (100 * (.tokenCount // 0) / .contextWindowSize) else 0 end),
                total_input_tokens: (.tokenCount // 0),
                model: (.model // ""),
                statusline_json: ({status: (.status // "")} | tojson)
            }' 2>/dev/null | while IFS= read -r line; do
                [ -n "$line" ] && post_context "$line"
            done
    fi
fi

# Chain to the operator's original command for this slot, if the installer
# wrapped one. Word-splitting $ORIG_CMD is intentional: it may carry
# arguments — an operator-configured, trusted value from their own
# settings.json, invoked the same way Claude Code invoked it directly before.
if [ "$is_subagent" = "1" ]; then
    ORIG_CMD="${TEAMSTER_SUBAGENT_STATUSLINE_CHAIN:-}"
else
    ORIG_CMD="${TEAMSTER_STATUSLINE_CHAIN:-}"
fi
if [ -n "$ORIG_CMD" ]; then
    echo "$INPUT" | $ORIG_CMD
    exit 0
fi

# No prior command to chain to for this slot: render a minimal default so
# installing Teamster doesn't leave the status line blank. subagentStatusLine
# has no established single-line rendering contract (see note above), so
# only the main statusLine gets a fallback render.
if [ "$is_main" = "1" ]; then
    echo "$INPUT" | jq -r '(.context_window.used_percentage // 0 | tostring) as $pct | "ctx " + $pct + "%"' 2>/dev/null || true
fi

exit 0

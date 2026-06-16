# Session-Explorer Guide

A 9-point primer for agents using `session-explorer.sh` to drive interactive
programs (Claude, ssh sessions, wizards) via tmux. Source the library before
any `se_*` call:

```bash
source "$TEAMSTER_BASEDIR/lib/scripts/session-explorer.sh"
```

Key functions: `se_start NAME CMD`, `se_sendline NAME TEXT`,
`se_wait_scrollback NAME PATTERN TIMEOUT`, `se_read_scrollback NAME LINES`,
`se_stop NAME`.

---

## The 9 Points

1. **Bash tool shell state does not persist between calls.** Each `Bash`
   invocation spawns a fresh shell. `source session-explorer.sh` in one call
   does NOT carry `se_*` functions to the next. Every Bash call that uses
   `se_*` must re-source. Pattern: `source <path> && se_start ... &&
   se_wait_scrollback ...` chained with `&&` in a single command. Working
   directory persists; shell functions do not.

2. **tmux session names persist across Bash calls.** Once `se_start NAME ...`
   succeeds, the tmux session lives until `se_stop NAME` (or
   `tmux kill-session -t NAME`). A subsequent Bash call can re-source and
   `se_sendline NAME ...` to the same persistent session.

3. **One-shot ssh commands need a lingering primitive.** `se_start NAME "ssh
   -t host 'echo OK; exit'"` exits ssh the instant `echo` completes; the tmux
   pane may close before scrollback capture. Append `; sleep 30` (or
   `read -t 30`) inside the ssh command. Interactive sessions (`ssh -t host
   claude`, mysql, vim, etc.) stay alive on their own.

4. **`se_sendline` is asynchronous.** The remote process needs wall-clock
   time. Pattern: `se_sendline NAME "..." && sleep 2 && se_read_scrollback
   NAME 100`. Tune sleep for command latency (2s fast, 10s+ for
   build/install).

5. **`se_wait_scrollback` is a polling regex matcher** with a hard timeout in
   seconds. Escape regex chars at bash level. Use generous timeouts for long
   ops (1800 for install build).

6. **Session conflicts.** If `se_start NAME` fails with "already exists,"
   inspect with `tmux ls`, clean with `tmux kill-session -t NAME` or
   `se_stop NAME`. Choose a unique NAME per phase:
   `<runner-name>-<phase>-<unix-ts>`.

7. **Where tmux runs.** The tmux session is on THIS dev host, not the
   remote. `se_start NAME "ssh -t user@test-vm"` creates a local tmux pane
   holding the ssh. Scrollback is captured from the LOCAL pane.

8. **Output capture: use generous N** in `se_read_scrollback NAME N`
   (100-200) so you don't miss prompts or errors near the top of the visible
   buffer.

9. **Banned absolutely:** `ssh <host> '<cmd>'` (any form), `claude --print` /
   `claude -p`, `--session-id` flag, `CLAUDE_*` env coercion to fake hook
   firing, `script -q` / `unbuffer` / `expect` / `screen -d` as substitutes,
   `scp`/`rsync` between hosts using interactive auth (drive them through
   session-explorer or pre-set ssh keys). **Even non-interactive read-only
   probes (ls, stat, git rev-parse, curl) go through the persistent session.**
   A VM-revert call to a provisioning host (e.g. `nc revert-host PORT`) is the
   only exception — it runs from the dev host against the provisioner, not
   against the test host.

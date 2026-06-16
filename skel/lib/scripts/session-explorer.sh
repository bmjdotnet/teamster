#!/usr/bin/env bash
# session-explorer.sh — tmux session library for agent-driven interactive program control
# Source this file; it defines functions only.

# Launch a command in a new detached tmux session.
# Args: SESSION_NAME COMMAND...
# Returns immediately after session creation.
se_start() {
    local session="$1"
    shift
    tmux new-session -d -s "$session" -x 200 -y 50 "$@"
    tmux set-option -t "$session" history-limit 50000 2>/dev/null || true
}

# Capture and print the current visible pane content.
# Args: SESSION_NAME
se_read() {
    local session="$1"
    tmux capture-pane -t "$session" -p
}

# Capture scrollback buffer.
# Args: SESSION_NAME [LINES]  (default: 1000)
se_read_scrollback() {
    local session="$1"
    local lines="${2:-1000}"
    tmux capture-pane -t "$session" -p -S -"${lines}"
}

# Send text to the session without pressing Enter.
# Args: SESSION_NAME TEXT
se_send() {
    local session="$1"
    local text="$2"
    tmux send-keys -t "$session" -l "$text"
}

# Send text to the session followed by Enter.
# Args: SESSION_NAME TEXT
se_sendline() {
    local session="$1"
    local text="$2"
    tmux send-keys -t "$session" -l "$text"
    tmux send-keys -t "$session" Enter
}

# Poll visible pane every 1s until PATTERN (grep -q) matches or timeout.
# Args: SESSION_NAME PATTERN [TIMEOUT_SECS]  (default timeout: 60)
# Returns: 0 on match, 1 on timeout.
se_wait() {
    local session="$1"
    local pattern="$2"
    local timeout="${3:-60}"
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        if tmux capture-pane -t "$session" -p 2>/dev/null | grep -q "$pattern"; then
            return 0
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    return 1
}

# Poll full scrollback every 1s until PATTERN matches or timeout.
# Args: SESSION_NAME PATTERN [TIMEOUT_SECS]  (default timeout: 60)
# Returns: 0 on match, 1 on timeout.
se_wait_scrollback() {
    local session="$1"
    local pattern="$2"
    local timeout="${3:-60}"
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        if tmux capture-pane -t "$session" -p -S -1000 2>/dev/null | grep -q "$pattern"; then
            return 0
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    return 1
}

# Check if a session exists and has a running process.
# Args: SESSION_NAME
# Returns: 0 if alive, 1 if not.
se_alive() {
    local session="$1"
    tmux has-session -t "$session" 2>/dev/null
}

# Kill a tmux session.
# Args: SESSION_NAME
se_stop() {
    local session="$1"
    tmux kill-session -t "$session" 2>/dev/null
}

# Capture pane content to a file, or stdout if no file given.
# Args: SESSION_NAME [FILE]
se_screenshot() {
    local session="$1"
    local file="${2:-}"
    if [ -n "$file" ]; then
        tmux capture-pane -t "$session" -p > "$file"
    else
        tmux capture-pane -t "$session" -p
    fi
}

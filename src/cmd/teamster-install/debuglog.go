package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// debugLog is the process-wide trace target opened by openDebugLog when
// --debug-log is set. nil means tracing disabled — dlog* is a fast no-op.
var debugLog *os.File

// openDebugLog opens path for append, creating parent dirs as needed.
// On unwritable path it returns the error so main can die loudly per
// [[no-silent-failures]] — never silently disable.
func openDebugLog(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("--debug-log: mkdir parent: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("--debug-log: open %s: %w", path, err)
	}
	debugLog = f
	return nil
}

// closeDebugLog flushes and closes the trace file if open.
func closeDebugLog() {
	if debugLog != nil {
		debugLog.Close()
		debugLog = nil
	}
}

// dlog writes one trace line. component is "teamster-install.<subsystem>".
// kv pairs are appended as space-separated key=value with shell-safe quoting.
// Format is locked with @wizard: "<RFC3339-UTC> <LEVEL5> <component> <msg>[ k=v]"
func dlog(level, component, msg string, kv ...string) {
	if debugLog == nil {
		return
	}
	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	lvl := padLevel(level)
	var sb strings.Builder
	sb.WriteString(ts)
	sb.WriteByte(' ')
	sb.WriteString(lvl)
	sb.WriteByte(' ')
	sb.WriteString(component)
	sb.WriteByte(' ')
	sb.WriteString(msg)
	for i := 0; i+1 < len(kv); i += 2 {
		sb.WriteByte(' ')
		sb.WriteString(kv[i])
		sb.WriteByte('=')
		sb.WriteString(quoteIfNeeded(kv[i+1]))
	}
	sb.WriteByte('\n')
	// Single Write call: append on Linux is atomic for sizes <= PIPE_BUF.
	debugLog.WriteString(sb.String())
}

func padLevel(level string) string {
	// Locked levels: TRACE DEBUG INFO  WARN  ERROR (all width 5).
	switch level {
	case "INFO":
		return "INFO "
	case "WARN":
		return "WARN "
	}
	return level
}

// quoteIfNeeded wraps values containing whitespace, '=' or '"' in double quotes
// with strconv.Quote-style escaping. Bare tokens pass through unmodified.
func quoteIfNeeded(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsAny(v, " \t\n\"=") {
		return strconv.Quote(v)
	}
	return v
}

// dtrace logs function entry (">> name") or exit ("<< name") at TRACE level.
func dtrace(component, marker, fn string, kv ...string) {
	dlog("TRACE", component, marker+" "+fn, kv...)
}

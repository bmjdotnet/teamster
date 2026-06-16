package wms

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SignalReader aggregates activity signals from a JSONL event log for a set of
// session windows. The hub deployment reads config.LogFile directly — this
// requires wms-mcp to run on the same host as hookd. Remote wms-mcp instances
// cannot classify (no JSONL access).
type SignalReader interface {
	ReadSignals(ctx context.Context, sessions []SessionWindow, logFile string) (*ActivitySignals, error)
}

// SessionWindow is a (session, agent, time) span during which an entity was
// active. SessionPrefix is the 12-character prefix of the full session_id
// because hookd truncates session_id to 12 chars when writing JSONL records.
type SessionWindow struct {
	SessionPrefix string // 12-char prefix of the full session_id
	AgentName     string
	Start         time.Time
	End           time.Time
}

// ActivitySignals holds aggregated tool-use and file signals for an entity.
type ActivitySignals struct {
	ToolTagCounts map[string]int // tag field values: READ, EDIT, GREP, EXEC, ...
	BashCommands  []string       // raw bash_cmd field values
	FilesTouched  map[string]int // file extension → count (from _file field)
	TotalEvents   int
}

// JSONLSignalReader reads activity signals directly from a JSONL event log
// file on disk. It performs a full linear scan (O(n)) and filters by session
// prefix, agent name, and time window.
type JSONLSignalReader struct{}

// NewJSONLSignalReader returns a JSONLSignalReader ready for use.
func NewJSONLSignalReader() *JSONLSignalReader {
	return &JSONLSignalReader{}
}

// ReadSignals scans logFile line by line, aggregating tool-tag counts, bash
// commands, and file extensions for all events within any of the given session
// windows. Returns an empty but non-nil ActivitySignals when no matching events
// are found.
func (r *JSONLSignalReader) ReadSignals(ctx context.Context, sessions []SessionWindow, logFile string) (*ActivitySignals, error) {
	signals := &ActivitySignals{
		ToolTagCounts: make(map[string]int),
		FilesTouched:  make(map[string]int),
	}

	if len(sessions) == 0 {
		return signals, nil
	}

	f, err := os.Open(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return signals, nil
		}
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}

		var rec struct {
			Session   string `json:"session"`
			AgentName string `json:"agent_name"`
			Ts        string `json:"ts"`
			Tag       string `json:"tag"`
			BashCmd   string `json:"bash_cmd"`
			File      string `json:"file"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}

		// hookd writes ts as an RFC3339 string (server.go writeRecord), e.g.
		// "2026-05-17T23:54:04Z". Parse it; RFC3339Nano also accepts second
		// precision, so it covers both shapes. Skip lines we can't time.
		recTime, err := parseEventTime(rec.Ts)
		if err != nil {
			continue
		}

		for _, sw := range sessions {
			if len(rec.Session) < 12 {
				if rec.Session != sw.SessionPrefix {
					continue
				}
			} else if rec.Session[:12] != sw.SessionPrefix {
				continue
			}
			if rec.AgentName != sw.AgentName {
				continue
			}
			if recTime.Before(sw.Start) || recTime.After(sw.End) {
				continue
			}

			signals.TotalEvents++
			if rec.Tag != "" {
				signals.ToolTagCounts[rec.Tag]++
			}
			if rec.BashCmd != "" {
				signals.BashCommands = append(signals.BashCommands, rec.BashCmd)
			}
			if rec.File != "" {
				ext := filepath.Ext(rec.File)
				if ext == "" {
					ext = "(no-ext)"
				}
				signals.FilesTouched[ext]++
			}
			break // matched this window, no need to check others
		}
	}

	return signals, scanner.Err()
}

// parseEventTime parses the JSONL `ts` field, which hookd writes as an RFC3339
// string. It accepts both second-precision (RFC3339) and fractional-second
// (RFC3339Nano) forms, and tolerates a legacy numeric epoch-seconds value for
// any producer that still emits one. The returned time is in UTC.
func parseEventTime(ts string) (time.Time, error) {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return time.Time{}, fmt.Errorf("empty ts")
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.UTC(), nil
	}
	// Legacy/defensive: a bare numeric epoch-seconds value (possibly fractional).
	if secs, err := strconv.ParseFloat(ts, 64); err == nil {
		return time.Unix(int64(secs), int64((secs-float64(int64(secs)))*1e9)).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unparseable ts %q", ts)
}

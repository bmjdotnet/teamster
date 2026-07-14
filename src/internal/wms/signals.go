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

// BatchSignalReader is an optional SignalReader capability: scan logFile ONCE
// and answer many SessionWindow queries from that single pass, instead of one
// linear scan per window. A caller sitting in a loop over many windows that
// share a log file — classify's per-pass interval batch (up to 500 intervals,
// see internal/classify) is the motivating case, GH #13 — should use this
// instead of calling ReadSignals once per window, which re-reads the file
// from scratch every time. JSONLSignalReader implements this; a test fake
// need not, and callers fall back to repeated ReadSignals calls when it
// doesn't (a type assertion on SignalReader).
type BatchSignalReader interface {
	// ReadSignalsBatch scans logFile once and returns one ActivitySignals per
	// window, in the same order as windows. Unlike ReadSignals — which merges
	// every matching event into one aggregate, "first matching window wins"
	// per line, because it represents a single combined query — an event
	// matching multiple windows here contributes to ALL of them
	// independently, since each window here represents a distinct caller-side
	// interval that needs its own count.
	//
	// lowerBound/upperBound (either may be the zero Time, meaning unbounded)
	// let the caller skip indexing events outside the range it actually needs
	// this call, bounding per-call cost to the caller's current working set
	// rather than the log's full lifetime size — this is what lets a
	// long-running deployment's classify pass stay cheap as the log grows
	// over time instead of re-scanning history it already fully processed.
	ReadSignalsBatch(ctx context.Context, windows []SessionWindow, logFile string, lowerBound, upperBound time.Time) ([]*ActivitySignals, error)
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
// file on disk. ReadSignals performs a full linear scan (O(n)) and filters by
// session prefix, agent name, and time window; ReadSignalsBatch answers many
// such queries from a single scan (see BatchSignalReader).
type JSONLSignalReader struct{}

// NewJSONLSignalReader returns a JSONLSignalReader ready for use.
func NewJSONLSignalReader() *JSONLSignalReader {
	return &JSONLSignalReader{}
}

// jsonlRecord is the subset of a hookd JSONL line ReadSignals/ReadSignalsBatch
// care about.
type jsonlRecord struct {
	Session   string `json:"session"`
	AgentName string `json:"agent_name"`
	Ts        string `json:"ts"`
	Tag       string `json:"tag"`
	BashCmd   string `json:"bash_cmd"`
	File      string `json:"file"`
}

// decodeJSONLLine unmarshals one JSONL line and parses its ts field. ok is
// false for a malformed line or an unparseable ts — both are skipped by the
// caller, matching the historical scan behavior (continue, not fail-fast).
func decodeJSONLLine(line []byte) (rec jsonlRecord, ts time.Time, ok bool) {
	if err := json.Unmarshal(line, &rec); err != nil {
		return jsonlRecord{}, time.Time{}, false
	}
	t, err := parseEventTime(rec.Ts)
	if err != nil {
		return jsonlRecord{}, time.Time{}, false
	}
	return rec, t, true
}

// accumulate folds one matching JSONL record into sig.
func accumulate(sig *ActivitySignals, rec jsonlRecord) {
	sig.TotalEvents++
	if rec.Tag != "" {
		sig.ToolTagCounts[rec.Tag]++
	}
	if rec.BashCmd != "" {
		sig.BashCommands = append(sig.BashCommands, rec.BashCmd)
	}
	if rec.File != "" {
		ext := filepath.Ext(rec.File)
		if ext == "" {
			ext = "(no-ext)"
		}
		sig.FilesTouched[ext]++
	}
}

// sessionKey normalizes a session id to the same value SessionWindow.
// SessionPrefix carries: the 12-char prefix hookd writes, or the whole string
// when it is shorter than 12 chars (mirrors the len-based branch below).
func sessionKey(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
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

		rec, recTime, ok := decodeJSONLLine(scanner.Bytes())
		if !ok {
			continue
		}

		key := sessionKey(rec.Session)
		for _, sw := range sessions {
			if key != sessionKey(sw.SessionPrefix) {
				continue
			}
			if rec.AgentName != sw.AgentName {
				continue
			}
			if recTime.Before(sw.Start) || recTime.After(sw.End) {
				continue
			}

			accumulate(signals, rec)
			break // matched this window, no need to check others
		}
	}

	return signals, scanner.Err()
}

// ReadSignalsBatch is the batched counterpart to ReadSignals: one linear scan
// of logFile serves every window in a single pass, bucketing events by
// (session, agent) for O(1) lookup per line instead of ReadSignals' per-window
// inner loop repeated once per caller call. lowerBound/upperBound (zero value
// = unbounded) additionally skip indexing events outside the caller's needed
// range, so a caller with a narrow current working set (e.g. one classify
// pass's interval batch) doesn't pay for the log's full historical size.
func (r *JSONLSignalReader) ReadSignalsBatch(ctx context.Context, windows []SessionWindow, logFile string, lowerBound, upperBound time.Time) ([]*ActivitySignals, error) {
	out := make([]*ActivitySignals, len(windows))
	for i := range out {
		out[i] = &ActivitySignals{ToolTagCounts: make(map[string]int), FilesTouched: make(map[string]int)}
	}
	if len(windows) == 0 {
		return out, nil
	}

	type key struct {
		session string
		agent   string
	}
	buckets := make(map[key][]int, len(windows))
	for i, w := range windows {
		k := key{sessionKey(w.SessionPrefix), w.AgentName}
		buckets[k] = append(buckets[k], i)
	}

	f, err := os.Open(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	hasLower := !lowerBound.IsZero()
	hasUpper := !upperBound.IsZero()

	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}

		rec, recTime, ok := decodeJSONLLine(scanner.Bytes())
		if !ok {
			continue
		}
		if hasLower && recTime.Before(lowerBound) {
			continue
		}
		if hasUpper && recTime.After(upperBound) {
			continue
		}

		idxs, ok := buckets[key{sessionKey(rec.Session), rec.AgentName}]
		if !ok {
			continue
		}
		for _, i := range idxs {
			sw := windows[i]
			if recTime.Before(sw.Start) || recTime.After(sw.End) {
				continue
			}
			accumulate(out[i], rec)
		}
	}

	return out, scanner.Err()
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

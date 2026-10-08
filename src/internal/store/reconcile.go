package store

import "time"

// LedgerTokens is a bundle of token sums over token_ledger rows. CacheWrite is
// the total of all cache-creation tokens (token_ledger.cache_write_tokens);
// CacheWrite5m/CacheWrite1h are the ledger's TTL split of it. Rows that predate
// the split can have CacheWrite5m+CacheWrite1h below CacheWrite.
type LedgerTokens struct {
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite   int64
	CacheWrite5m int64
	CacheWrite1h int64
}

// LedgerModel is one model's aggregate within a session.
type LedgerModel struct {
	Tokens  LedgerTokens
	CostUSD float64
}

// LedgerSession is the hub-side view of one session: SUM over its
// token_ledger rows, grouped by model exactly as stored (so a "[1m]"-labelled
// model stays distinct from its base). Rows is the row count; LastRowAt is
// MAX(token_ledger.timestamp).
type LedgerSession struct {
	SessionID string
	Rows      int64
	LastRowAt time.Time
	Models    map[string]LedgerModel
}

// UniqueIDChunks drops empty and repeated ids (keeping first-seen order) and
// splits the rest into chunks of at most size, so a backend can bound the
// placeholder count of an IN (...) list.
func UniqueIDChunks(ids []string, size int) [][]string {
	if size <= 0 {
		size = 1
	}
	seen := make(map[string]struct{}, len(ids))
	var chunks [][]string
	var cur []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		cur = append(cur, id)
		if len(cur) == size {
			chunks = append(chunks, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// FoldLedgerGroup merges one (session, model) group into out, creating the
// session on first sight, so every backend builds LedgerSession identically
// from its own GROUP BY rows.
func FoldLedgerGroup(out map[string]LedgerSession, sessionID, model string, rows int64, last time.Time, t LedgerTokens, costUSD float64) {
	s, ok := out[sessionID]
	if !ok {
		s = LedgerSession{SessionID: sessionID, Models: map[string]LedgerModel{}}
	}
	s.Rows += rows
	if last = last.UTC(); last.After(s.LastRowAt) {
		s.LastRowAt = last
	}
	m := s.Models[model]
	m.Tokens.Input += t.Input
	m.Tokens.Output += t.Output
	m.Tokens.CacheRead += t.CacheRead
	m.Tokens.CacheWrite += t.CacheWrite
	m.Tokens.CacheWrite5m += t.CacheWrite5m
	m.Tokens.CacheWrite1h += t.CacheWrite1h
	m.CostUSD += costUSD
	s.Models[model] = m
	out[sessionID] = s
}

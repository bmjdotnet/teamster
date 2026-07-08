// Package codexconfig writes Teamster-owned content into a Codex CLI
// config.toml without a TOML parsing library. Prototype evidence
// (research/toml-lib-decision.md in the teamster-codex-kit) showed every
// candidate Go TOML library (pelletier/go-toml v2, its v1 Tree API,
// BurntSushi/toml) destroys 100% of operator comments and reformats the
// whole file on a decode-mutate-encode round trip. This package instead
// wraps each Teamster-owned block in `# >>> teamster:<name> >>>` /
// `# <<< teamster:<name> <<<` marker comments (legal anywhere TOML allows a
// comment) and only ever deletes-then-reappends the span between its own
// markers — every byte outside a marked span is never touched.
package codexconfig

import (
	"fmt"
	"regexp"
	"strings"
)

// Policy controls whether UpsertSection touches an already-materialized
// Teamster-owned section on a rerun.
type Policy int

const (
	// SkipIfPresent leaves an existing Teamster-owned section exactly as it
	// is on rerun. Used for content an operator might reasonably hand-edit
	// after install (mcp_servers.* tables) — running the installer again
	// must never silently revert that edit.
	SkipIfPresent Policy = iota
	// AlwaysUpsert deletes and rewrites the section on every run. Required
	// wherever staleness is unsafe — e.g. a future hooks.state trust block
	// (WP8), whose hash must be re-derived every run or an upgrade that
	// moves the hook binary/changes an arg silently strands the host on
	// invalid trust. Not used by any of WP2's own templates today; the
	// policy exists now so WP5/WP8 can add AlwaysUpsert sections onto this
	// same primitive without new merge machinery.
	AlwaysUpsert
)

// UpsertResult reports what UpsertSection actually did, so callers can log
// the steady-state outcomes (SkippedExisting, UnmarkedCollision) as
// information, not treat them as failures.
type UpsertResult struct {
	// Changed is true iff content was modified.
	Changed bool
	// SkippedExisting is true when a SkipIfPresent section already existed
	// (bounded by Teamster's own markers) and was left untouched.
	SkippedExisting bool
	// UnmarkedCollision is true when literalHeader already appears in
	// content as a bare line, but not inside Teamster's own markers for
	// this name — an operator (or some other tool) already defined this
	// exact table. UpsertSection never touches foreign content, so the
	// section is left exactly as found; the caller should surface a
	// warning, since Teamster's required fields (env, approval mode) won't
	// be present on the foreign version.
	UnmarkedCollision bool
}

func markerLines(name string) (start, end string) {
	return "# >>> teamster:" + name + " >>>", "# <<< teamster:" + name + " <<<"
}

func sectionRegexp(name string) *regexp.Regexp {
	start, end := markerLines(name)
	// (?s) so `.` matches newlines — the body between markers is multi-line.
	// Non-greedy `.*?` so two same-named marker pairs (should never happen,
	// but a hand-edited file could end up with one) don't merge into a
	// single match spanning both.
	return regexp.MustCompile(regexp.QuoteMeta(start) + `(?s).*?` + regexp.QuoteMeta(end) + `\n?`)
}

// containsLine reports whether content has a line (after trimming
// surrounding whitespace) exactly equal to line. Used only for the
// unmarked-collision check — a full-line match avoids false positives from,
// say, a comment that merely mentions the table name in prose.
func containsLine(content, line string) bool {
	for _, l := range strings.Split(content, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// blankRunRe collapses 2+ consecutive blank lines into exactly one. Without
// this, repeated SkipIfPresent reruns leave a stale blank-line gap where a
// neighboring AlwaysUpsert section used to sit right after it (that section
// gets removed-then-reappended at EOF each run), and the gap grows by one
// blank line per run. Purely cosmetic — codex's parser tolerates any amount
// of blank space — but ugly enough in a file operators are expected to read
// that it's worth fixing rather than documenting as a known wart.
var blankRunRe = regexp.MustCompile(`\n{3,}`)

func collapseBlankRuns(content string) string {
	return blankRunRe.ReplaceAllString(content, "\n\n")
}

// UpsertSection idempotently writes a Teamster-owned, marker-bounded block
// named name into content, and returns the new content plus a report of what
// happened.
//
// literalHeader is the exact TOML table-header line the block begins with
// (e.g. "[mcp_servers.wms]") — used only to detect an unmarked collision
// (see UpsertResult.UnmarkedCollision); pass "" to skip that check (e.g. for
// a body with no single natural header line, or when the caller has already
// verified no collision is possible).
//
// body must be pre-rendered, valid TOML (this package does not parse or
// validate TOML syntax) and should end in "\n".
func UpsertSection(content, name, body, literalHeader string, policy Policy) (string, UpsertResult) {
	re := sectionRegexp(name)
	hasMarker := re.MatchString(content)

	if !hasMarker && literalHeader != "" && containsLine(content, literalHeader) {
		return content, UpsertResult{UnmarkedCollision: true}
	}

	if hasMarker && policy == SkipIfPresent {
		return content, UpsertResult{SkippedExisting: true}
	}

	stripped := re.ReplaceAllString(content, "")
	stripped = collapseBlankRuns(strings.TrimRight(stripped, "\n"))

	start, end := markerLines(name)
	block := start + "\n" + body + end + "\n"

	if strings.TrimSpace(stripped) == "" {
		return block, UpsertResult{Changed: true}
	}
	return stripped + "\n\n" + block, UpsertResult{Changed: true}
}

// RemoveSection deletes a previously-written marker span for name, if
// present, and is a no-op otherwise. Used by uninstall (WP7) — calling this
// for every name Teamster ever wrote restores the file to its pre-install
// state.
func RemoveSection(content, name string) string {
	re := sectionRegexp(name)
	if !re.MatchString(content) {
		return content
	}
	stripped := re.ReplaceAllString(content, "")
	return collapseBlankRuns(strings.TrimRight(stripped, "\n")) + "\n"
}

// sectionName and literalHeader are small helpers shared by every concrete
// spec type in this package (MCPServerSpec today; future otel/hooks specs in
// WP5/WP8) so the "table identity" naming convention lives in one place.
func mcpServerSectionName(id string) string  { return "mcp_servers." + id }
func mcpServerLiteralHeader(id string) string { return fmt.Sprintf("[mcp_servers.%s]", id) }

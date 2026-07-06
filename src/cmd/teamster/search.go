package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// runSearch dispatches the `teamster search <subcommand>` family. Returns the exit code.
func runSearch(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, searchUsage)
		return 2
	}
	switch args[0] {
	case "sessions":
		return runSearchSessions(args[1:])
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stdout, searchUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown search subcommand: %s\n%s\n", args[0], searchUsage)
		return 2
	}
}

const searchUsage = `usage: teamster search <subcommand>

subcommands:
  sessions <query>      find sessions by outcome/workunit/focus match

flags (sessions):
  --type <list>         outcomes,workunits,focus,all (default all)
  --user <u>            filter to a user
  --host <h>            filter to a host
  --status <s>          filter session status (active, idle, closed)
  --tag key=value       exact tag filter, repeatable (AND)
  --since <dur>         sessions active within window (e.g. 72h)
  --limit <n>           cap rows (default 50)
  --json                machine-readable []SessionMatch output`

// tagFilterFlag collects repeatable --tag key=value flags into []string.
type tagFilterFlag []string

func (t *tagFilterFlag) String() string { return strings.Join(*t, ",") }
func (t *tagFilterFlag) Set(v string) error {
	*t = append(*t, v)
	return nil
}

func runSearchSessions(args []string) int {
	fs := flag.NewFlagSet("teamster search sessions", flag.ContinueOnError)
	typeFlag := fs.String("type", wms.SearchTypeAll, "outcomes,workunits,focus,all (default all)")
	user := fs.String("user", "", "filter to a user")
	host := fs.String("host", "", "filter to a host")
	status := fs.String("status", "", "filter session status (active, idle, closed)")
	since := fs.String("since", "", "sessions active within window (e.g. 72h)")
	limit := fs.Int("limit", 50, "cap rows")
	jsonOut := fs.Bool("json", false, "machine-readable []SessionMatch output")
	var tagFilters tagFilterFlag
	fs.Var(&tagFilters, "tag", "exact tag filter key=value, repeatable (AND)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	types, err := parseSearchTypes(*typeFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "search sessions: %v\n", err)
		return 2
	}

	var sinceTime time.Time
	if *since != "" {
		d, err := time.ParseDuration(*since)
		if err != nil {
			fmt.Fprintf(os.Stderr, "search sessions: --since: %v\n", err)
			return 2
		}
		sinceTime = time.Now().UTC().Add(-d)
	}

	query := strings.Join(fs.Args(), " ")
	if query == "" && len(tagFilters) == 0 {
		fmt.Fprintln(os.Stderr, "search sessions: a query or at least one --tag filter is required")
		return 2
	}

	s, err := openTagsDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "search sessions: %v\n", err)
		return 1
	}
	defer s.Close() //nolint:errcheck

	ctx := context.Background()
	q := wms.SearchQuery{
		Query:  query,
		Types:  types,
		User:   *user,
		Host:   *host,
		Status: *status,
		Tags:   []string(tagFilters),
		Since:  sinceTime,
		Limit:  *limit,
	}

	sessions, err := wms.SearchSessions(ctx, s, q)
	if err != nil {
		fmt.Fprintf(os.Stderr, "search sessions: %v\n", err)
		return 1
	}

	if *jsonOut {
		b, err := json.Marshal(sessions)
		if err != nil {
			fmt.Fprintf(os.Stderr, "search sessions: %v\n", err)
			return 1
		}
		fmt.Println(string(b))
		return 0
	}

	if len(sessions) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions found")
		return 0
	}

	writeSessionsTable(os.Stdout, sessions)
	return 0
}

// parseSearchTypes splits a comma-separated --type value into the surfaces
// wms.SearchQuery.Types expects, validating each against the known surface
// names. An empty raw value yields nil (core default: search everything).
func parseSearchTypes(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	types := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		switch p {
		case wms.SearchTypeOutcomes, wms.SearchTypeWorkUnits, wms.SearchTypeFocus, wms.SearchTypeAll:
			types = append(types, p)
		default:
			return nil, fmt.Errorf("--type: invalid value %q (want outcomes, workunits, focus, or all)", p)
		}
	}
	return types, nil
}

// writeSessionsTable renders one row per session — USER, HOST, SESSION,
// MATCHED, WHEN — followed by a "N sessions · M hosts" footer. SESSION is
// always printed in full; only MATCHED collapses to a "first (+N)" overflow.
func writeSessionsTable(w io.Writer, sessions []wms.SessionMatch) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tHOST\tSESSION\tMATCHED\tWHEN")
	hosts := make(map[string]bool)
	for _, sm := range sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", sm.User, sm.Host, sm.SessionID, renderMatched(sm.Matched), humanizeAgo(sm.LastSeen))
		hosts[sm.Host] = true
	}
	tw.Flush() //nolint:errcheck
	fmt.Fprintf(w, "%d sessions · %d hosts\n", len(sessions), len(hosts))
}

// renderMatched formats a session's matched entities as "first (+N)" — the
// entity overflow ellipsizes, SESSION never does (writeSessionsTable prints
// SessionID untouched).
func renderMatched(refs []wms.EntityRef) string {
	if len(refs) == 0 {
		return ""
	}
	primary := matchedText(refs[0])
	if len(refs) > 1 {
		return fmt.Sprintf("%s (+%d)", primary, len(refs)-1)
	}
	return primary
}

// matchedText renders one EntityRef: "type:id", or for a focus-string match
// (no backing entity) the focus text itself, with the "focus:" prefix that
// SearchSessions puts on Why stripped back off.
func matchedText(ref wms.EntityRef) string {
	if ref.EntityType == wms.SearchTypeFocus {
		return strings.TrimPrefix(ref.Why, "focus:")
	}
	return ref.EntityType + ":" + ref.EntityID
}

// humanizeAgo renders t relative to now, e.g. "12h ago", "3d ago".
func humanizeAgo(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

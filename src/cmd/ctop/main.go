// Command ctop is a Bubbletea TUI dashboard for muster agent health and
// activity. It is a pure HTTP client over hookd's /health/api/* +
// /health/stream surface — no DB imports, no internal/agenthealth/gauge
// (design D3). Works identically hub-local and remote.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
)

// minPollInterval is the floor for --interval (design §2.1).
const minPollInterval = 2 * time.Second

// maxHistory caps --history (design §2.1).
const maxHistory = 500

// defaultInactiveAfter is --inactive-after's compiled default (§ctop
// AgentAF redesign item 4): an agent alive but silent this long reads as
// "inactive" in the STATUS/ACTIVITY dots and dims (hue preserved) in the
// row. Temporarily 0 (disabled — isInactive treats <=0 as "never inactive")
// pending a broader UX pass on the dimming behavior.
const defaultInactiveAfter = 0

func main() {
	serverFlag := flag.String("server", "", "")
	intervalFlag := flag.Duration("interval", 5*time.Second, "")
	hostFlag := flag.String("host", "", "")
	runtimeFlag := flag.String("runtime", "", "")
	teamFlag := flag.String("team", "", "")
	historyFlag := flag.Int("history", 50, "")
	maxAgeFlag := flag.Duration("max-age", time.Hour, "")
	agePresetsFlag := flag.String("age-presets", "", "")
	inactiveAfterFlag := flag.String("inactive-after", "", "")
	noColorFlag := flag.Bool("no-color", false, "")
	versionFlag := flag.Bool("version", false, "")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ctop [flags]\n\nFlags:\n")
		fmt.Fprintf(os.Stderr, "  --server URL      hub base URL (default: TEAMSTER_HOOK_SERVER_URL via config, else http://localhost:9125)\n")
		fmt.Fprintf(os.Stderr, "  --interval DUR    health poll interval (default 5s, floor 2s)\n")
		fmt.Fprintf(os.Stderr, "  --host HOST       filter agents to one host (server-side)\n")
		fmt.Fprintf(os.Stderr, "  --runtime RT      filter by runtime: claude_code | codex (server-side)\n")
		fmt.Fprintf(os.Stderr, "  --team TEAM       client-side filter on team_name\n")
		fmt.Fprintf(os.Stderr, "  --history N       activity backfill lines (default 50, max %d)\n", maxHistory)
		fmt.Fprintf(os.Stderr, "  --max-age DUR     hide agents inactive longer than this (default 1h, e.g. 15m/6h/24h; 0 = no filter). Starting position in the \"r\"-key cycle\n")
		fmt.Fprintf(os.Stderr, "  --age-presets LIST  comma-separated \"r\"-key cycle, e.g. 30m,2h,12h,0 (0 = all; default 1h,6h,0). Overrides TEAMSTER_CTOP_AGE_PRESETS\n")
		fmt.Fprintf(os.Stderr, "  --inactive-after DUR  alive-but-silent threshold for the inactive dim state (default 10m). Overrides TEAMSTER_CTOP_INACTIVE_AFTER\n")
		fmt.Fprintf(os.Stderr, "  --no-color        strip ANSI color\n")
		fmt.Fprintf(os.Stderr, "  --version         print version and exit\n")
	}
	flag.Parse()

	if *versionFlag {
		fmt.Println("ctop (dev)")
		return
	}

	logging.Init("ctop")

	// ctop has no non-TTY mode (design §2.1) — refuse with a pointer to the
	// tool that does.
	if !term.IsTerminal(os.Stdout.Fd()) {
		fmt.Fprintln(os.Stderr, "ctop requires an interactive terminal; use `feed --tail` for piped/non-TTY output")
		os.Exit(1)
	}

	if *noColorFlag {
		os.Setenv("NO_COLOR", "1")
	}

	interval := *intervalFlag
	if interval < minPollInterval {
		interval = minPollInterval
	}

	history := *historyFlag
	if history < 0 {
		history = 0
	}
	if history > maxHistory {
		history = maxHistory
	}

	server := resolveServer(*serverFlag)

	agePresetsSrc := *agePresetsFlag
	if agePresetsSrc == "" {
		agePresetsSrc = os.Getenv("TEAMSTER_CTOP_AGE_PRESETS")
	}
	agePresets := defaultAgePresets
	if agePresetsSrc != "" {
		parsed, err := parseAgePresets(agePresetsSrc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ctop: %v\n", err)
			os.Exit(1)
		}
		agePresets = parsed
	}

	inactiveAfter, err := resolveInactiveAfter(*inactiveAfterFlag, os.Getenv("TEAMSTER_CTOP_INACTIVE_AFTER"), defaultInactiveAfter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ctop: %v\n", err)
		os.Exit(1)
	}

	client := newHubClient(server)
	m := newModel(client, server, *hostFlag, *runtimeFlag, *teamFlag, interval, history, *maxAgeFlag, agePresets, inactiveAfter)
	// NO_COLOR (above) covers lipgloss-rendered styles; m.colorize additionally
	// strips the raw truecolor ANSI that internal/display emits for agent-name
	// and activity-feed coloring, which NO_COLOR alone doesn't reach.
	m.colorize = !*noColorFlag

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		slog.Error("program run failed", "error", err)
		os.Exit(1)
	}
}

// resolveInactiveAfter applies --inactive-after's precedence: the flag
// value if set, else envVal (TEAMSTER_CTOP_INACTIVE_AFTER), else def — the
// same flag-wins/env-fallback/compiled-default chain as
// --age-presets/TEAMSTER_CTOP_AGE_PRESETS (parseAgePresets), just for a
// single duration instead of a comma-separated cycle.
func resolveInactiveAfter(flagVal, envVal string, def time.Duration) (time.Duration, error) {
	src := flagVal
	if src == "" {
		src = envVal
	}
	if src == "" {
		return def, nil
	}
	return time.ParseDuration(src)
}

// resolveServer applies the --server precedence: explicit flag, else
// internal/config's HookServerURL (which itself already applies
// TEAMSTER_HOOK_SERVER_URL over a hostname-based default), else a hardcoded
// localhost fallback if config.Load fails entirely.
func resolveServer(flagVal string) string {
	if flagVal != "" {
		return strings.TrimSuffix(flagVal, "/")
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Warn("config load failed, defaulting to localhost", "error", err)
		return "http://localhost:9125"
	}
	return strings.TrimSuffix(strings.TrimSuffix(cfg.HookServerURL, "/event"), "/")
}

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/render"
	"github.com/bmjdotnet/teamster/internal/tui"
)

// tickMsg fires the health poll timer.
type tickMsg time.Time

// displayTickMsg fires a 1s display-only re-render, independent of the
// (much slower) health poll cycle, so the LAST column's relative-time label
// visibly ticks (1s, 2s, ... 1m, ...) instead of updating only every
// pollInterval seconds. Carries no data — Update just re-arms it.
type displayTickMsg time.Time

func displayTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return displayTickMsg(t) })
}

type agentsResultMsg struct {
	rows []Agent
	err  error
}

type alertsResultMsg struct {
	rows []Agent
	err  error
}

type snapshotResultMsg struct {
	snap     *Snapshot
	rosterID string
	err      error
}

// model is the root Bubbletea program state (§2.9). It owns ALL state —
// shared (hub client, SSE subscription, health poll results, status bar)
// and the Fleet view's own (agents/detail/activity/fleet) — see views.go's
// view doc comment for why view state lives here rather than on a separate
// view struct.
type model struct {
	width, height int

	client   *hubClient
	interval time.Duration
	host     string
	runtime  string
	team     string
	server   string

	agents   agentsModel
	detail   detailModel
	activity activityModel
	fleet    fleetModel

	sseConnected bool
	lastPollErr  error
	lastPollAt   time.Time

	// costHistory is the burn-rate meter's sample ring: one (ts, total_cost)
	// pair per poll where the grand-total cost actually changed. Pruned to
	// the last 15 minutes in appendCostSample — comfortably more than the
	// 5-minute window burnRate reads from.
	costHistory []costSample

	events <-chan tea.Msg
	cancel context.CancelFunc

	showHelp bool
	colorize bool // false when --no-color is set
}

func newModel(client *hubClient, server, host, runtime, team string, interval time.Duration, historyN int, maxAge time.Duration, agePresets []time.Duration, inactiveAfter time.Duration) model {
	ctx, cancel := context.WithCancel(context.Background())
	presets, ageIdx := resolveAgePresets(agePresets, maxAge)
	return model{
		client:   client,
		server:   server,
		host:     host,
		runtime:  runtime,
		team:     team,
		interval: interval,
		agents:   agentsModel{lastActions: make(map[string]lastAction), maxAge: maxAge, agePresets: presets, ageIdx: ageIdx, inactiveAfter: inactiveAfter},
		activity: newActivityModel(),
		events:   client.Subscribe(ctx, historyN),
		cancel:   cancel,
		colorize: true,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.pollCmd(), tickCmd(m.interval), m.waitEventCmd(), displayTickCmd())
}

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// waitEventCmd blocks for the next message from the shared SSE channel.
// Every branch that could race a value already in flight on this channel
// must re-arm it — see the Update cases below; arming it twice is harmless
// (it's a shared channel read). This mirrors a real bug in cmd/feed
// (main.go:283): returning nil on WindowSizeMsg silently stopped the event
// loop.
func (m model) waitEventCmd() tea.Cmd {
	ch := m.events
	return func() tea.Msg {
		v, ok := <-ch
		if !ok {
			return sseStateMsg{connected: false}
		}
		return v
	}
}

func (m model) fetchAgentsCmd() tea.Cmd {
	// liveness is always left nil — client.go's ListAgents already defaults
	// to live+idle+stale+closed (everything), and the age filter (--max-age,
	// the "r" cycle) is applied client-side over that full set, not via a
	// server-side liveness scope.
	client, host, runtime := m.client, m.host, m.runtime
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		rows, err := client.ListAgents(ctx, host, runtime, nil)
		return agentsResultMsg{rows: rows, err: err}
	}
}

func (m model) fetchAlertsCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		rows, err := client.Alerts(ctx)
		return alertsResultMsg{rows: rows, err: err}
	}
}

func (m model) fetchSnapshotCmd(rosterID string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		snap, err := client.Snapshot(ctx, rosterID)
		return snapshotResultMsg{snap: snap, rosterID: rosterID, err: err}
	}
}

// pollCmd fetches agents + alerts (+ the live snapshot if something is
// selected), each as its own concurrent Cmd (tea.Batch).
func (m model) pollCmd() tea.Cmd {
	cmds := []tea.Cmd{m.fetchAgentsCmd(), m.fetchAlertsCmd()}
	if m.detail.rosterID != "" {
		cmds = append(cmds, m.fetchSnapshotCmd(m.detail.rosterID))
	}
	return tea.Batch(cmds...)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, m.waitEventCmd()

	case tickMsg:
		return m, tea.Batch(m.pollCmd(), tickCmd(m.interval))

	case agentsResultMsg:
		if msg.err != nil {
			m.lastPollErr = msg.err
		} else {
			m.lastPollErr = nil
			m.lastPollAt = time.Now()
			rows := msg.rows
			if m.team != "" {
				rows = filterByTeam(rows, m.team)
			}
			m.agents.setRows(rows)
			m.recordCostSample()
			if sel, ok := m.agents.current(); ok && m.agents.selected == sel.SelectionKey() {
				m.detail.selectedRow = &sel
			}
		}
		return m, nil

	case alertsResultMsg:
		if msg.err == nil {
			m.detail.setAlerts(msg.rows)
		}
		return m, nil

	case snapshotResultMsg:
		if msg.rosterID == m.detail.rosterID {
			m.detail.setSnapshot(msg.snap, msg.err)
		}
		return m, nil

	case activityEventMsg:
		m.activity.selectedName = m.currentSelectedAgentName()
		m.activity.append(render.Record(msg))
		m.agents.recordActivity(render.Record(msg))
		return m, m.waitEventCmd()

	case sseStateMsg:
		m.sseConnected = msg.connected
		return m, m.waitEventCmd()

	case displayTickMsg:
		// Also re-derives the age-filtered row set every second, not just on
		// poll/key events — so a row aging past the max-age boundary drops
		// out (or the ticking LAST label it's shown next to) stays honest
		// second-by-second instead of lagging up to a full poll interval.
		m.agents.recomputeRows()
		return m, displayTickCmd()

	}

	return m, nil
}

func (m model) currentSelectedAgentName() string {
	if row := m.detail.selectedRow; row != nil {
		name := row.AgentName
		if name == "" {
			name = "@lead"
		}
		return name
	}
	return ""
}

// handleKey handles GLOBAL keys (quit, help toggle) — always available —
// then forwards everything else to fleetView's own Update (see
// fleet_view.go). ctop has a single view (Fleet); there is no
// view-switching left to handle here.
func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if m.showHelp {
		m.showHelp = false
		return m, nil
	}

	switch key {
	case "q", "ctrl+c":
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "?":
		m.showHelp = true
		return m, nil
	}

	_, cmd := fleetView{m: &m}.Update(msg)
	return m, cmd
}

// filterByTeam applies the --team client-side filter (there is no
// server-side team filter on /health/api/agents; /health/api/team/{name} is
// a separate team-summary endpoint, not a listing filter).
func filterByTeam(rows []Agent, team string) []Agent {
	out := make([]Agent, 0, len(rows))
	for _, r := range rows {
		if r.TeamName == team {
			out = append(out, r)
		}
	}
	return out
}

// --- burn rate ---

// costSample is one (timestamp, total_cost) observation for the burn-rate
// meter.
type costSample struct {
	ts   time.Time
	cost float64
}

// burnRateWindow is the lookback burnRate reads its $/hr rate from.
const burnRateWindow = 5 * time.Minute

// burnRateHistoryTTL bounds costHistory's growth — comfortably longer than
// burnRateWindow so the window always has samples to work with, without
// keeping the ring growing unboundedly for a long-running session.
const burnRateHistoryTTL = 15 * time.Minute

// burnRateMinElapsed is the shortest span burnRate will compute a rate over
// — below this, a tiny elapsed duration turns even a small cost delta into
// a wildly exaggerated $/hr number.
const burnRateMinElapsed = 30 * time.Second

// appendCostSample appends (now, total) to history if total differs from
// the last recorded sample (poll-to-poll no-ops don't pad the ring), then
// prunes anything older than burnRateHistoryTTL.
func appendCostSample(history []costSample, total float64, now time.Time) []costSample {
	if len(history) > 0 && history[len(history)-1].cost == total {
		return history
	}
	history = append(history, costSample{ts: now, cost: total})
	cutoff := now.Add(-burnRateHistoryTTL)
	i := 0
	for i < len(history) && history[i].ts.Before(cutoff) {
		i++
	}
	return history[i:]
}

// recordCostSample sums SessionCostUSD across every currently-loaded row
// (only ever non-zero on a session's lead — see agents.go's
// headerDisplayAgent doc — so this is a true grand total, not a double
// count) and appends it to m.costHistory.
func (m *model) recordCostSample() {
	var total float64
	for _, r := range m.agents.rows {
		total += r.SessionCostUSD
	}
	m.costHistory = appendCostSample(m.costHistory, total, time.Now())
}

// burnRate computes a $/hr spend rate from the change across
// history within burnRateWindow of now. Returns (0, false) — "insufficient
// data" — with fewer than 2 samples, or when the samples in the window span
// less than burnRateMinElapsed. A negative delta (a session closing/cost
// resetting) clamps to 0 rather than showing a negative burn rate.
func burnRate(history []costSample, now time.Time) (float64, bool) {
	if len(history) < 2 {
		return 0, false
	}
	windowStart := now.Add(-burnRateWindow)
	start := history[0]
	for _, s := range history {
		if !s.ts.Before(windowStart) {
			break
		}
		start = s
	}
	end := history[len(history)-1]
	elapsed := end.ts.Sub(start.ts)
	if elapsed < burnRateMinElapsed {
		return 0, false
	}
	delta := end.cost - start.cost
	if delta < 0 {
		delta = 0
	}
	return delta / elapsed.Hours(), true
}

func (m model) View() string {
	if m.width < 40 || m.height < 8 {
		return lipgloss.Place(max(m.width, 1), max(m.height, 1), lipgloss.Center, lipgloss.Center,
			"terminal too small (min 40x8)")
	}
	if m.showHelp {
		return helpOverlay(m.width, m.height, fleetView{m: &m})
	}

	bodyH := m.height - 1 // status bar
	if bodyH < 1 {
		bodyH = 1
	}

	// fleetView.View's width/height are the OUTER footprint it must fill
	// exactly (the full terminal width, and whatever's left after the
	// status bar) — NOT a content budget.
	body := fleetView{m: &m}.View(m.width, bodyH)
	rendered := body + "\n" + m.statusBar()
	if !m.colorize {
		return display.StripANSI(rendered)
	}
	return rendered
}

func (m model) statusBar() string {
	sseDot := lipgloss.NewStyle().Foreground(tui.ColorError).Render("○")
	if m.sseConnected {
		sseDot = lipgloss.NewStyle().Foreground(tui.ColorSuccess).Render("◉")
	}

	visible, active, closed := m.agents.visibleAgentCounts()

	warn, crit := 0, 0
	for _, a := range m.detail.alerts {
		switch a.PressureLevel {
		case "warning":
			warn++
		case "critical":
			crit++
		}
	}

	pollState := "poll ✓"
	if m.lastPollErr != nil {
		age := "?"
		if !m.lastPollAt.IsZero() {
			age = relativeTime(m.lastPollAt)
		}
		pollState = "poll ✗ " + age
	}

	burnLabel := "—/hr"
	if rate, ok := burnRate(m.costHistory, time.Now()); ok {
		burnLabel = fmt.Sprintf("$%.2f/hr", rate)
	}

	parts := []string{
		"hub " + m.server,
		"sse " + sseDot,
		fmt.Sprintf("%d visible · %d active · %d closed", visible, active, closed),
		fmt.Sprintf("%d WARN %d CRIT", warn, crit),
		burnLabel,
		"sort:" + m.agents.sort.label(),
		pollState,
		"q quit · ? help",
		time.Now().Format("15:04:05"),
	}
	sep := lipgloss.NewStyle().Foreground(tui.ColorDim).Render(" · ")
	bar := " " + strings.Join(parts, sep)
	return lipgloss.NewStyle().Background(tui.ColorHighBg).Foreground(tui.ColorText).Width(m.width).Render(padTrunc(bar, m.width))
}

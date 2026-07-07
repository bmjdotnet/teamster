package statusui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/version"
)

const refreshInterval = 5 * time.Second

// ServiceRowFetcher is a function that returns current service health rows.
// cmd/teamster supplies this so statusui doesn't depend on package main.
type ServiceRowFetcher func() []ServiceRow

// tickMsg triggers a periodic data refresh.
type tickMsg struct{}

// dataMsg carries the result of an async data fetch.
type dataMsg struct {
	serviceRows []ServiceRow
	summary     store.StatusSummary
	sweepAge    string
	hasStore    bool
}

// Model is the Bubbletea model for the status dashboard.
type Model struct {
	cfg         config.Config
	fetchRows   ServiceRowFetcher
	width       int
	height      int
	serviceRows []ServiceRow
	summary     store.StatusSummary
	sweepAge    string
	hasStore    bool
	ready       bool
}

// New creates a Model ready to run. fetchRows provides current service health.
func New(cfg config.Config, fetchRows ServiceRowFetcher) Model {
	return Model{cfg: cfg, fetchRows: fetchRows, width: 120, height: 40}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.fetch(), tick())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case tickMsg:
		return m, tea.Batch(m.fetch(), tick())

	case dataMsg:
		m.serviceRows = msg.serviceRows
		m.summary = msg.summary
		m.sweepAge = msg.sweepAge
		m.hasStore = msg.hasStore
		m.ready = true
	}

	return m, nil
}

func (m Model) View() string {
	if !m.ready {
		return dimStyle.Render("\n  Loading…\n")
	}

	const (
		minLeft  = 54
		minRight = 36
		gap      = 2
	)
	usable := m.width - 4
	if usable < minLeft {
		usable = minLeft
	}
	twoCol := usable >= minLeft+gap+minRight

	var leftW, rightW int
	if twoCol {
		leftW = usable * 55 / 100
		if leftW < minLeft {
			leftW = minLeft
		}
		rightW = usable - leftW - gap
		if rightW < minRight {
			rightW = minRight
		}
	} else {
		leftW = usable
		rightW = usable
	}

	// Header
	versionStr := fmt.Sprintf("Teamster %s", version.String())
	host := m.cfg.Host

	var header string
	if twoCol {
		left := lipgloss.NewStyle().Width(leftW).Render(dimStyle.Render(versionStr))
		right := boldStyle.Render(host)
		header = left + strings.Repeat(" ", gap) + right
	} else {
		header = dimStyle.Render(versionStr) + "\n" + boldStyle.Render(host)
	}

	// Left column panels
	svcPanel := renderServicesPanel(m.serviceRows, leftW)
	wmsPanel := renderWMSPanel(m.summary, m.hasStore, leftW)
	leftCol := lipgloss.JoinVertical(lipgloss.Left, svcPanel, wmsPanel)

	// Right column panels
	var body string
	if twoCol {
		var rightCol string
		if m.hasStore {
			dbPanel := renderDatabasePanel(m.summary, rightW)
			costPanel := renderCostPanel(m.summary, m.sweepAge, rightW)
			sessPanel := renderSessionsPanel(m.summary, rightW)
			rightCol = lipgloss.JoinVertical(lipgloss.Left, dbPanel, costPanel, sessPanel)
		}
		body = lipgloss.JoinHorizontal(lipgloss.Top, leftCol, strings.Repeat(" ", gap), rightCol)
	} else {
		body = leftCol
		if m.hasStore {
			dbPanel := renderDatabasePanel(m.summary, leftW)
			costPanel := renderCostPanel(m.summary, m.sweepAge, leftW)
			sessPanel := renderSessionsPanel(m.summary, leftW)
			body = lipgloss.JoinVertical(lipgloss.Left, body, dbPanel, costPanel, sessPanel)
		}
	}

	hint := dimStyle.Render("q quit  •  auto-refresh every 5s")

	var sb strings.Builder
	sb.WriteString("\n  ")
	sb.WriteString(header)
	sb.WriteString("\n\n")
	for _, line := range strings.Split(body, "\n") {
		sb.WriteString("  ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	sb.WriteString("\n  ")
	sb.WriteString(hint)
	sb.WriteString("\n")
	return sb.String()
}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

func (m Model) fetch() tea.Cmd {
	cfg := m.cfg
	fetchRows := m.fetchRows
	return func() tea.Msg {
		var rows []ServiceRow
		if fetchRows != nil {
			rows = fetchRows()
		}

		var summary store.StatusSummary
		var hasStore bool
		if cfg.StoreDSN.Raw != "" {
			s, err := store.Open(context.Background(), cfg.StoreDSN.Raw, store.WithSkipMigrate())
			if err == nil {
				defer s.Close() //nolint:errcheck
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if sm, err := s.GetStatusSummary(ctx); err == nil {
					summary = sm
					hasStore = true
				}
			}
		}

		return dataMsg{
			serviceRows: rows,
			summary:     summary,
			sweepAge:    fetchSweepAge(cfg.DataDir),
			hasStore:    hasStore,
		}
	}
}

// Run launches the Bubbletea TUI program.
func Run(cfg config.Config, fetchRows ServiceRowFetcher) error {
	m := New(cfg, fetchRows)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// RenderOnce renders a single snapshot (for non-TTY / --once mode).
func RenderOnce(cfg config.Config, fetchRows ServiceRowFetcher) string {
	var rows []ServiceRow
	if fetchRows != nil {
		rows = fetchRows()
	}

	var summary store.StatusSummary
	var hasStore bool
	if cfg.StoreDSN.Raw != "" {
		s, err := store.Open(context.Background(), cfg.StoreDSN.Raw, store.WithSkipMigrate())
		if err == nil {
			defer s.Close() //nolint:errcheck
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if sm, err := s.GetStatusSummary(ctx); err == nil {
				summary = sm
				hasStore = true
			}
		}
	}

	m := Model{
		cfg:         cfg,
		width:       120,
		height:      40,
		serviceRows: rows,
		summary:     summary,
		sweepAge:    fetchSweepAge(cfg.DataDir),
		hasStore:    hasStore,
		ready:       true,
	}
	return m.View()
}

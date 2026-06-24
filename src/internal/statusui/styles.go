// Package statusui provides the Bubbletea TUI for the teamster status command.
package statusui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/tui"
)

// Panel border styles.
var (
	panelStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tui.ColorBorder)

	headerStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(tui.ColorAccent)

	dimStyle = lipgloss.NewStyle().
		Foreground(tui.ColorDim)

	successStyle = lipgloss.NewStyle().
		Foreground(tui.ColorSuccess)

	warnStyle = lipgloss.NewStyle().
		Foreground(tui.ColorWarn)

	errorStyle = lipgloss.NewStyle().
		Foreground(tui.ColorError)

	boldStyle = lipgloss.NewStyle().
		Bold(true)

	textStyle = lipgloss.NewStyle().
		Foreground(tui.ColorText)
)

// statusLevel classifies a service status for styling.
type statusLevel int

const (
	levelHealthy statusLevel = iota
	levelWarn
	levelError
	levelDim
)

// classifyStatus maps plain status text to a level.
func classifyStatus(plain string) statusLevel {
	switch {
	case strContainsAny(plain, "Healthy", "Connected", "Provisioned"):
		return levelHealthy
	case strContainsAny(plain, "Not running", "Unreachable", "Unauthorized"):
		return levelError
	case strContainsAny(plain, "Not responding", "Not provisioned"):
		return levelWarn
	default:
		return levelDim
	}
}

func strContainsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) == 0 {
			continue
		}
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

func renderIcon(level statusLevel) string {
	switch level {
	case levelHealthy:
		return successStyle.Render("●")
	case levelError:
		return errorStyle.Render("●")
	case levelWarn:
		return warnStyle.Render("●")
	default:
		return dimStyle.Render("●")
	}
}

func renderStatusText(text string, level statusLevel) string {
	switch level {
	case levelHealthy:
		return successStyle.Render(text)
	case levelError:
		return errorStyle.Render(text)
	case levelWarn:
		return warnStyle.Render(text)
	default:
		return dimStyle.Render(text)
	}
}

func renderAttribution(pct float64) string {
	s := fmt.Sprintf("%.1f%%", pct)
	switch {
	case pct >= 90:
		return successStyle.Render(s)
	case pct >= 70:
		return warnStyle.Render(s)
	default:
		return errorStyle.Render(s)
	}
}

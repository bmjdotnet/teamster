package tui

import "github.com/charmbracelet/lipgloss"

// Color palette matching hookd dark terminal aesthetic.
var (
	ColorBg      = lipgloss.Color("#0d1117")
	ColorText    = lipgloss.Color("#c9d1d9")
	ColorAccent  = lipgloss.Color("#58a6ff")
	ColorPurple  = lipgloss.Color("#bc8cff")
	ColorSuccess = lipgloss.Color("#3fb950")
	ColorWarn    = lipgloss.Color("#d29922")
	ColorError   = lipgloss.Color("#f85149")
	ColorDim     = lipgloss.Color("#484f58")
	ColorBorder  = lipgloss.Color("#30363d")
	ColorHighBg  = lipgloss.Color("#161b22")
)

// Panel border styles.
var (
	FocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorAccent)

	UnfocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorBorder)

	DimBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorDim)
)

// Text styles.
var (
	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorAccent)

	SubtitleStyle = lipgloss.NewStyle().
			Foreground(ColorDim)

	TextStyle = lipgloss.NewStyle().
			Foreground(ColorText)

	DimStyle = lipgloss.NewStyle().
			Foreground(ColorDim)

	AccentStyle = lipgloss.NewStyle().
			Foreground(ColorAccent)

	BoldAccentStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorAccent)

	ErrorStyle = lipgloss.NewStyle().
			Foreground(ColorError)

	SuccessStyle = lipgloss.NewStyle().
			Foreground(ColorSuccess)

	LifecycleKeyStyle = lipgloss.NewStyle().
				Foreground(ColorDim)

	CursorStyle = lipgloss.NewStyle().
			Foreground(ColorAccent)

	SelectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorAccent).
			Background(ColorHighBg)

	CheckedStyle = lipgloss.NewStyle().
			Foreground(ColorAccent)

	UncheckedStyle = lipgloss.NewStyle().
			Foreground(ColorDim)

	ReqStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorWarn)

	StatusBarStyle = lipgloss.NewStyle().
			Foreground(ColorDim)

	StepStyle = lipgloss.NewStyle().
			Foreground(ColorDim)
)

// HeaderStyle renders a screen title + separator rule.
func HeaderStyle(title, step string, width int) string {
	titleStr := TitleStyle.Render(title)
	stepStr := StepStyle.Render(step)
	gap := width - lipgloss.Width(titleStr) - lipgloss.Width(stepStr)
	if gap < 1 {
		gap = 1
	}
	header := titleStr + repeat(" ", gap) + stepStr
	rule := DimStyle.Render(repeat("─", width))
	return header + "\n" + rule
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// truncate shortens a display string to at most n visible characters.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	vis := lipgloss.Width(s)
	if vis <= n {
		return s
	}
	runes := []rune(lipgloss.NewStyle().Render(s))
	if len(runes) <= n {
		return string(runes)
	}
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}

package tui

import "github.com/charmbracelet/lipgloss"

// Legacy palette: fixed dark colours. cmd/ctop consumes these as lipgloss.Color
// (including in tests), so their type and values must not change.
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
	ColorMetric  = lipgloss.Color("#39c5cf")
)

// Adaptive palette, TradeWars-style: saturated 16-colour hues on dark
// terminals, darker equivalents of the same hues on light ones. Values and
// user data are the brightest things on screen; labels and chrome are grey.
// lipgloss queries the terminal background once at first render.
var (
	pBg      = lipgloss.AdaptiveColor{Dark: "#000000", Light: "#ffffff"}
	pText    = lipgloss.AdaptiveColor{Dark: "#e4e4e4", Light: "#1f2328"}
	pAccent  = lipgloss.AdaptiveColor{Dark: "#55ffff", Light: "#00758a"} // bright cyan: prompts, interactive
	pPurple  = lipgloss.AdaptiveColor{Dark: "#ff55ff", Light: "#a40e9c"} // magenta: special categories
	pSuccess = lipgloss.AdaptiveColor{Dark: "#55ff55", Light: "#1a7f37"} // bright green: positive, counts
	pWarn    = lipgloss.AdaptiveColor{Dark: "#ffaa00", Light: "#9a6700"}
	pError   = lipgloss.AdaptiveColor{Dark: "#ff5555", Light: "#cf222e"} // bright red: danger
	pDim     = lipgloss.AdaptiveColor{Dark: "#8a8a8a", Light: "#6e7781"} // grey: structure
	pBorder  = lipgloss.AdaptiveColor{Dark: "#5a5a5a", Light: "#b8c0c8"}
	pHighBg  = lipgloss.AdaptiveColor{Dark: "#0000af", Light: "#bcd7ff"} // selection
	pMetric  = lipgloss.AdaptiveColor{Dark: "#55ff55", Light: "#1a7f37"}
	pYellow  = lipgloss.AdaptiveColor{Dark: "#ffff55", Light: "#8a6d00"} // bright yellow: values, user data
	pGold    = lipgloss.AdaptiveColor{Dark: "#b3a000", Light: "#7a6100"} // muted gold: read-only values
	pWhite   = lipgloss.AdaptiveColor{Dark: "#ffffff", Light: "#000000"} // bright white: headings
	pBlue    = lipgloss.AdaptiveColor{Dark: "#5f87ff", Light: "#0550ae"}

	ColorKey          = pAccent
	ColorHelp         = lipgloss.AdaptiveColor{Dark: "#a8a8a8", Light: "#57606a"}
	ColorFieldBg      = lipgloss.AdaptiveColor{Dark: "#00005f", Light: "#dce8ff"}
	ColorFieldFocusBg = lipgloss.AdaptiveColor{Dark: "#0000af", Light: "#b6d0ff"}
	ColorOnAccent     = lipgloss.AdaptiveColor{Dark: "#000000", Light: "#ffffff"}
	ColorOutcome      = pPurple
	ColorWorkunit     = pBlue
)

// Panel border styles.
var (
	FocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(pAccent)

	UnfocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(pBorder)

	DimBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(pDim)
)

// Text styles.
var (
	TitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(pAccent)

	SubtitleStyle = lipgloss.NewStyle().
			Foreground(pDim)

	TextStyle = lipgloss.NewStyle().
			Foreground(pText)

	DimStyle = lipgloss.NewStyle().
			Foreground(pDim)

	AccentStyle = lipgloss.NewStyle().
			Foreground(pAccent)

	BoldAccentStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(pAccent)

	ErrorStyle = lipgloss.NewStyle().
			Foreground(pError)

	SuccessStyle = lipgloss.NewStyle().
			Foreground(pSuccess)

	LifecycleKeyStyle = lipgloss.NewStyle().
				Foreground(pDim)

	CursorStyle = lipgloss.NewStyle().
			Foreground(pAccent)

	SelectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(pAccent).
			Background(pHighBg)

	CheckedStyle = lipgloss.NewStyle().
			Foreground(pAccent)

	UncheckedStyle = lipgloss.NewStyle().
			Foreground(pDim)

	ReqStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ffffff")).
			Background(lipgloss.AdaptiveColor{Dark: "#d70000", Light: "#cf222e"}).
			Padding(0, 1)

	StatusBarStyle = lipgloss.NewStyle().
			Foreground(pDim)

	StepStyle = lipgloss.NewStyle().
			Foreground(pDim)

	FieldLabelStyle = lipgloss.NewStyle().Foreground(ColorHelp).Width(14)

	FieldLabelFocusStyle = lipgloss.NewStyle().Bold(true).Foreground(pAccent).Width(14)

	LockedStyle = lipgloss.NewStyle().Foreground(pDim)

	HeadingStyle = lipgloss.NewStyle().Bold(true).Foreground(pWhite)

	ButtonStyle = lipgloss.NewStyle().Foreground(ColorHelp).Background(lipgloss.AdaptiveColor{Dark: "#3a3a3a", Light: "#d0d7de"}).Padding(0, 2)

	ButtonSaveFocusStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorOnAccent).Background(pSuccess).Padding(0, 2)

	ButtonDangerFocusStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.AdaptiveColor{Dark: "#d70000", Light: "#cf222e"}).Padding(0, 2)

	ButtonNeutralFocusStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.AdaptiveColor{Dark: "#6c6c6c", Light: "#6e7781"}).Padding(0, 2)

	FieldStyle = lipgloss.NewStyle().Bold(true).Foreground(pYellow).Background(ColorFieldBg)

	FieldFocusStyle = lipgloss.NewStyle().Bold(true).Foreground(pYellow).Background(ColorFieldFocusBg)

	FieldPlaceholderStyle = lipgloss.NewStyle().Foreground(pDim).Background(ColorFieldBg)

	FieldCursorStyle = lipgloss.NewStyle().Reverse(true)

	OverflowStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorKey)

	ChipStyle = lipgloss.NewStyle().Foreground(pYellow).Background(ColorFieldBg).Padding(0, 1)

	ChipFocusStyle = lipgloss.NewStyle().Bold(true).Foreground(pYellow).Background(ColorFieldFocusBg).Padding(0, 1)

	KeyStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorKey)

	HintStyle = lipgloss.NewStyle().Foreground(pDim)

	HelpPanelStyle = lipgloss.NewStyle().Foreground(ColorHelp)

	RuleStyle = lipgloss.NewStyle().Foreground(pBorder)

	MetricStyle = lipgloss.NewStyle().Bold(true).Foreground(pMetric)

	GroupStyle = lipgloss.NewStyle().Bold(true).Foreground(pWhite)

	KeyNameStyle = lipgloss.NewStyle().Bold(true).Foreground(pAccent)

	ValueNameStyle = lipgloss.NewStyle().Bold(true).Foreground(pYellow)

	DetailLabelStyle = lipgloss.NewStyle().Foreground(ColorHelp).Width(14)

	PlaceholderTextStyle = lipgloss.NewStyle().Foreground(pDim).Italic(true)

	BoldTextStyle = lipgloss.NewStyle().Bold(true).Foreground(pWhite)

	WarnStyle = lipgloss.NewStyle().Bold(true).Foreground(pWarn)

	DataStyle = lipgloss.NewStyle().Bold(true).Foreground(pYellow)

	DataDimStyle = lipgloss.NewStyle().Foreground(pGold)

	GoodStyle = lipgloss.NewStyle().Bold(true).Foreground(pSuccess)

	ChromeStyle = lipgloss.NewStyle().Foreground(pAccent)

	HelpHeadingStyle = lipgloss.NewStyle().Bold(true).Foreground(pAccent)

	OutcomeStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorOutcome)

	WorkunitStyle = lipgloss.NewStyle().Bold(true).Foreground(ColorWorkunit)
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

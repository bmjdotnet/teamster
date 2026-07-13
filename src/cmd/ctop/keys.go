package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/tui"
)

// Global keys — always win regardless of focused panel (§2.5).
const (
	keyQuitQ = "q"
	keyQuitC = "ctrl+c"
	keyHelp  = "?"
)

// Agents-panel keys.
const (
	keyDown     = "down"
	keyDownJ    = "j"
	keyUp       = "up"
	keyUpK      = "k"
	keyFirst    = "g"
	keyLast     = "G"
	keySelect   = "enter"
	keySelectL  = "l"
	keyClear    = "esc"
	keyClearH   = "h"
	keySort     = "s"
	keyAgeCycle = "r"
	keyPageUp   = "pgup"
	keyPageDown = "pgdown"
	keyEnd      = "end"
	keyFilter   = "f"
	keyPause    = "p"
)

// helpLines is the GLOBAL keymap reference shown in the help overlay —
// bindings that apply regardless of which view is active. Each view
// contributes its own block below this one via its KeyBindings() method
// (see the view interface in views.go).
var helpLines = []string{
	"GLOBAL",
	"  q, ctrl+c        quit",
	"  ?                toggle this help",
}

// helpOverlay renders a centered box listing the keymap, sized to fit
// within width x height: the always-shown GLOBAL section, then v's own
// title + keybindings. Any key closes it (handled by the caller's Update).
func helpOverlay(width, height int, v view) string {
	title := tui.TitleStyle.Render("ctop — keys")
	footer := tui.DimStyle.Render("(any key closes)")
	body := title + "\n\n" + strings.Join(helpLines, "\n") + "\n\n" + v.KeyBindings() + "\n\n" + footer
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(tui.ColorAccent).
		Padding(1, 2).
		Render(body)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

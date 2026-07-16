package main

import (
	tea "github.com/charmbracelet/bubbletea"
)

// view is implemented by ctop's single top-level screen, fleetView. The
// root model owns ALL state — shared (hub client, SSE subscription, health
// poll results, status bar) and the Fleet view's own (agents grid state,
// the fleet cursor, ...). A view value is never stored persistently: it's
// constructed fresh from the model in hand on every Update/View call (see
// fleetView{m: &m} call sites in model.go) and is nothing more than a
// *model-backed selector of which rendering routine and key-handler apply.
type view interface {
	// Update handles one input message (in practice, only tea.KeyMsg —
	// every other message type carries genuinely shared state and is
	// handled directly in model.Update instead, since the view interface
	// has no reason to special-case messages it will structurally never
	// receive). Returns the view to keep displaying (almost always itself)
	// and any resulting Cmd.
	Update(msg tea.Msg) (view, tea.Cmd)
	// View renders this screen's full body — the entire area between the
	// top of the terminal and the status bar. width/height are that area's
	// OUTER budget (border chars included, if any), matching what
	// model.View() has left after reserving one row for the status bar.
	View(width, height int) string
	// KeyBindings returns this view's own keymap reference block for the
	// help overlay (see keys.go's helpOverlay), on top of the always-shown
	// GLOBAL section.
	KeyBindings() string
	// Title names the view, shown in the help overlay and available for
	// any other view-aware chrome.
	Title() string
}

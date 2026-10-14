package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (m EditorModel) View() string {
	if m.loading {
		return DimStyle.Render("loading tag vocabulary...")
	}
	if m.err != "" {
		return ErrorStyle.Render("error: " + m.err)
	}
	if m.width < 80 || m.height < 24 {
		return ErrorStyle.Render("terminal too small (need 80x24)")
	}
	if m.showHelp {
		return m.renderHelp()
	}
	if m.form != nil {
		return m.renderModal()
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(),
		DimStyle.Render(strings.Repeat("─", m.width)),
		lipgloss.JoinHorizontal(lipgloss.Top, m.renderTree(), m.renderDetail()),
		m.renderStatusBar(),
	)
}

func (m EditorModel) columnWidths() (treeW, detailW int) {
	treeW = min(max(m.width*2/5, 30), 48)
	detailW = m.width - treeW - 4
	return
}

// colorNumbers renders s dim with every digit run in the metric colour.
func colorNumbers(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); {
		j := i
		digit := rs[i] >= '0' && rs[i] <= '9'
		for j < len(rs) && (rs[j] >= '0' && rs[j] <= '9') == digit {
			j++
		}
		if digit {
			b.WriteString(MetricStyle.Render(string(rs[i:j])))
		} else {
			b.WriteString(DimStyle.Render(string(rs[i:j])))
		}
		i = j
	}
	return b.String()
}

func yesNoStyled(b bool, yes lipgloss.Style) string {
	if b {
		return yes.Render("yes")
	}
	return DimStyle.Render("no")
}

func (m EditorModel) renderHeader() string {
	keys, values, entities := m.treeTotals()
	title := BoldTextStyle.Render("Tag Vocabulary Editor")
	stats := colorNumbers(fmt.Sprintf("%d keys · %d values · %d entities", keys, values, entities))
	gap := max(1, m.width-lipgloss.Width(title)-lipgloss.Width(stats))
	return title + strings.Repeat(" ", gap) + stats
}

// --- Tree pane ---------------------------------------------------------------

func (m EditorModel) treeRow(n *treeNode, w int, selected bool) string {
	indent := strings.Repeat("  ", n.depth)
	glyph := "  "
	switch n.kind {
	case nodeGroup, nodeKey:
		switch {
		case n.visKids == 0 && n.kind == nodeKey:
			glyph = "· "
		case n.open:
			glyph = "▾ "
		default:
			glyph = "▸ "
		}
	}

	on := func(st lipgloss.Style) lipgloss.Style {
		if selected {
			return st.Background(pHighBg)
		}
		return st
	}

	var label, badges string
	suffix := ""
	count := 0
	nameStyle := ValueNameStyle
	switch n.kind {
	case nodeGroup:
		label, count, nameStyle = n.group.label, n.group.entityCount, GroupStyle
	case nodeKey:
		label, count = n.key.tagKey, n.key.entityCount
		nameStyle = KeyNameStyle
		if n.key.lifecycle {
			nameStyle = LifecycleKeyStyle
		}
		if ns := groupOf(n).group; ns.namespace != "" && !ns.lifecycle {
			label = "." + strings.TrimPrefix(n.key.tagKey, ns.namespace+".")
		}
		if n.key.required {
			badges = on(ReqStyle).Render("REQ")
		}
	case nodeValue:
		label, count = n.value.value, n.value.entityCount
		if n.value.retired {
			suffix = " [retired]"
			nameStyle = DimStyle
		}
	}
	if count > 0 {
		if badges != "" {
			badges += on(lipgloss.NewStyle()).Render(" ")
		}
		badges += on(MetricStyle).Render(fmt.Sprint(count))
	}

	prefix := on(lipgloss.NewStyle()).Render(indent) + on(DimStyle).Render(glyph)
	avail := w - lipgloss.Width(prefix) - lipgloss.Width(suffix)
	if badges != "" {
		avail -= lipgloss.Width(badges) + 1
	}
	label = truncate(label, max(avail, 1))

	if selected {
		nameStyle = lipgloss.NewStyle().Bold(true).Foreground(pWhite).Background(pHighBg)
	}
	styled := nameStyle.Render(label)
	if suffix != "" {
		styled += on(DimStyle).Render(suffix)
	}
	left := prefix + styled
	pad := max(0, w-lipgloss.Width(left)-lipgloss.Width(badges))
	if badges != "" {
		pad = max(1, pad)
	}
	return left + on(lipgloss.NewStyle()).Render(strings.Repeat(" ", pad)) + badges
}

func (m EditorModel) renderTree() string {
	w, _ := m.columnWidths()
	h := m.bodyHeight()
	var lines []string
	if m.filterActive {
		lines = append(lines, AccentStyle.Render("/ ")+m.filterInput.View())
	} else if m.filter != "" {
		lines = append(lines, DimStyle.Render("/ "+m.filter))
	}
	rows := h - len(lines)

	if len(m.visible) == 0 {
		lines = append(lines, DimStyle.Render("(no matching tags)"))
	}
	start := 0
	if len(m.visible) > rows {
		start = min(max(0, m.cursor-rows/2), len(m.visible)-rows)
	}
	for i := start; i < len(m.visible) && len(lines) < h; i++ {
		lines = append(lines, m.treeRow(m.visible[i], w, i == m.cursor))
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	border := UnfocusedBorder
	if m.focus == focusTree {
		border = FocusedBorder
	}
	return border.Width(w).Height(h).Render(strings.Join(lines, "\n"))
}

// --- Detail pane --------------------------------------------------------------

func detailField(label, val string) string {
	return DetailLabelStyle.Render(label+":") + DataStyle.Render(val)
}

func detailFieldStyled(label, styledVal string) string {
	return DetailLabelStyle.Render(label+":") + styledVal
}

func conventionField(label, val, def string) string {
	style := DataStyle
	if val == "" {
		val, style = def, DimStyle
	}
	return "  " + DetailLabelStyle.Render(label+":") + style.Render(val)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func wrapIndented(s string, w int, style lipgloss.Style) []string {
	var out []string
	for _, l := range wordWrap(s, max(w-2, 10)) {
		out = append(out, style.Render("  "+l))
	}
	return out
}

func (m EditorModel) detailLines(w int) []string {
	n := m.cur()
	if n == nil {
		return []string{DimStyle.Render("(nothing to show)")}
	}
	var lines []string
	switch n.kind {
	case nodeGroup:
		g := n.group
		lines = append(lines,
			DimStyle.Render("Group: ")+GroupStyle.Render(g.label),
			detailFieldStyled("Keys", MetricStyle.Render(fmt.Sprint(len(n.children)))),
			detailFieldStyled("Entities", MetricStyle.Render(fmt.Sprint(g.entityCount))),
			"")
		var text string
		switch {
		case g.lifecycle:
			text = "Engine-managed lifecycle keys: vocabulary, category and cardinality are locked. Descriptions and the required flag are editable."
		case g.namespace != "":
			text = fmt.Sprintf("Integration namespace %q. Keys are create-on-apply.", g.namespace)
		default:
			text = "Operator-owned context keys. Add a key with A."
		}
		for _, l := range wordWrap(text, w-2) {
			lines = append(lines, TextStyle.Render(l))
		}

	case nodeKey:
		k := n.key
		title := DimStyle.Render("Key: ") + KeyNameStyle.Render(k.tagKey)
		if k.required {
			title += "   " + ReqStyle.Render("REQ")
		}
		if k.lifecycle {
			title += "  " + DimStyle.Render("(engine-managed)")
		}
		vals := MetricStyle.Render(fmt.Sprint(k.valueCount))
		switch {
		case k.createOnApply:
			vals = DimStyle.Render("create-on-apply")
		case k.retiredCount > 0:
			vals += DimStyle.Render(fmt.Sprintf(" (+%d retired)", k.retiredCount))
		}
		lines = append(lines, title, "",
			detailField("Category", k.category),
			detailField("Cardinality", k.cardinality),
			detailFieldStyled("Required", yesNoStyled(k.required, GoodStyle)),
			detailFieldStyled("Values", vals),
			detailFieldStyled("Entities", MetricStyle.Render(fmt.Sprint(k.entityCount))),
			"", HeadingStyle.Render("Conventions"),
			conventionField("Scope", k.scope, "(any)"),
			conventionField("Interview", k.interview, "propose"),
			conventionField("Excl. group", k.exclusionGroup, "(none)"),
			conventionField("Auto-extract", k.autoExtract, "(none)"))
		if k.facetSource != "" {
			lines = append(lines, conventionField("Facet source", k.facetSource, ""))
		}
		lines = append(lines, "", HeadingStyle.Render("Key description"))
		if k.description == "" {
			lines = append(lines, PlaceholderTextStyle.Render("  (none — press e to add one)"))
		} else {
			lines = append(lines, wrapIndented(k.description, w, TextStyle)...)
		}

	case nodeValue:
		v := n.value
		lines = append(lines,
			KeyNameStyle.Render(v.key.tagKey)+DimStyle.Render(" = ")+DataStyle.Render(v.value), "",
			detailFieldStyled("Seed", yesNoStyled(v.isSeed, GoodStyle)),
			detailFieldStyled("Retired", yesNoStyled(v.retired, WarnStyle)),
			detailFieldStyled("Entities", MetricStyle.Render(fmt.Sprint(v.entityCount))),
			"", HeadingStyle.Render("Value description"))
		if v.description == "" {
			lines = append(lines, PlaceholderTextStyle.Render("  (none — press e to add one)"))
		} else {
			lines = append(lines, wrapIndented(v.description, w, TextStyle)...)
		}
		lines = append(lines, "", DimStyle.Render("Key description"))
		if v.key.description == "" {
			lines = append(lines, PlaceholderTextStyle.Render("  (none)"))
		} else {
			lines = append(lines, wrapIndented(v.key.description, w, DimStyle)...)
		}
		lines = append(lines, "", HeadingStyle.Render("Used by"))
		switch {
		case v.entityCount == 0:
			lines = append(lines, PlaceholderTextStyle.Render("  (not applied to any entities)"))
		case m.detailState == detailLoading || m.detailState == detailIdle:
			lines = append(lines, DimStyle.Render("  loading…"))
		case m.detailState == detailError:
			lines = append(lines, ErrorStyle.Render("  "+truncate(m.detailErr, w-2)))
		default:
			for _, e := range m.detail.BoundEntities {
				lines = append(lines, usedByLine(e.EntityType, e.EntityID, e.Why, w))
			}
		}
	}
	return lines
}

func (m EditorModel) renderDetail() string {
	_, w := m.columnWidths()
	h := m.bodyHeight()
	all := m.detailLines(w)
	start := min(m.detailScroll, max(0, len(all)-h))
	lines := all[start:min(len(all), start+h)]
	for len(lines) < h {
		lines = append(lines, "")
	}
	border := UnfocusedBorder
	if m.focus == focusDetail {
		border = FocusedBorder
	}
	return border.Width(w).Height(h).Render(strings.Join(lines, "\n"))
}

// --- Used-by -----------------------------------------------------------------

func entityTypeStyle(t string) lipgloss.Style {
	switch t {
	case "outcome":
		return OutcomeStyle
	case "workunit":
		return WorkunitStyle
	}
	return DimStyle
}

func usedByLine(entityType, id, why string, w int) string {
	plain := fmt.Sprintf("  %-8s %s — %s", entityType, id, why)
	if lipgloss.Width(plain) > w {
		return DimStyle.Render(truncate(plain, w))
	}
	return "  " + entityTypeStyle(entityType).Width(8).Render(entityType) + " " +
		DataStyle.Render(id) + DimStyle.Render(" — "+why)
}

// --- Hints and status bar ------------------------------------------------------

type hint struct{ key, label string }

// renderHints joins hints as KEY label pairs, two spaces apart, truncated to width.
func renderHints(hs []hint, width int) string {
	var parts []string
	for _, h := range hs {
		parts = append(parts, KeyStyle.Render(h.key)+" "+HintStyle.Render(h.label))
	}
	out := strings.Join(parts, "  ")
	for lipgloss.Width(out) > width && len(parts) > 1 {
		parts = parts[:len(parts)-1]
		out = strings.Join(parts, "  ")
	}
	return out
}

var (
	hintsFilter = []hint{{"type", "to filter"}, {"⏎", "keep"}, {"esc", "clear"}}
	hintsDetail = []hint{{"↑↓", "scroll"}, {"e", "edit"}, {"tab/esc", "back to tree"}, {"?", "help"}, {"q", "quit"}}
	hintsGroup  = []hint{{"↑↓", "move"}, {"⏎/space", "expand"}, {"A", "add key"}, {"r", "retired"}, {"/", "filter"}, {"tab", "detail"}, {"?", "help"}, {"q", "quit"}}
	hintsKey    = []hint{{"↑↓", "move"}, {"→←", "expand"}, {"⏎/e", "edit"}, {"t", "required"}, {"a", "add value"}, {"A", "add key"}, {"d", "retire"}, {"r", "retired"}, {"/", "filter"}, {"?", "help"}, {"q", "quit"}}
	hintsValue  = []hint{{"↑↓", "move"}, {"←", "collapse"}, {"⏎/e", "edit"}, {"t", "required"}, {"a", "add value"}, {"d", "retire"}, {"r", "retired"}, {"/", "filter"}, {"?", "help"}, {"q", "quit"}}
)

func (m EditorModel) renderStatusBar() string {
	if m.statusMsg != "" {
		style := StatusBarStyle
		switch {
		case strings.HasPrefix(m.statusMsg, "error:"):
			style = ErrorStyle
		case strings.HasPrefix(m.statusMsg, "✓ "):
			style = SuccessStyle
		}
		return style.Render(truncate(m.statusMsg, m.width))
	}
	n := m.cur()
	var hs []hint
	switch {
	case m.filterActive:
		hs = hintsFilter
	case m.focus == focusDetail:
		hs = hintsDetail
	case n == nil || n.kind == nodeGroup:
		hs = hintsGroup
	case n.kind == nodeKey:
		hs = hintsKey
	default:
		hs = hintsValue
	}
	return renderHints(hs, m.width)
}

// --- Overlays -----------------------------------------------------------------

type helpRow struct{ keys, text string }

type helpSection struct {
	title string
	rows  []helpRow // keys == "" renders as a prose line
}

var helpSections = []helpSection{
	{"Navigation", []helpRow{
		{"↑ ↓ / k j", "move"},
		{"→ ← / l h", "expand / collapse (← on a value jumps to its key)"},
		{"space", "toggle expand"},
		{"tab", "switch tree ↔ detail pane (detail scrolls with ↑↓)"},
		{"/", "filter keys and values (⏎ keep, esc clear)"},
		{"R", "reload from the database"},
	}},
	{"Editing", []helpRow{
		{"⏎ or e", "open the editor form for the highlighted key or value"},
		{"t", "toggle the required flag on the highlighted key"},
		{"a", "add a value under the highlighted key"},
		{"A", "add a new context key (under the highlighted namespace)"},
		{"d", "retire the highlighted key or value (asks to confirm)"},
		{"r", "show / hide retired values"},
	}},
	{"In a form", []helpRow{
		{"tab ↑ ↓", "move between fields and the Save / Discard buttons"},
		{"↑↓ (in a description)", "move within the text; at the first/last line they leave the field"},
		{"space ⏎ ← →", "toggle a checkbox, cycle a choice"},
		{"ctrl+s", "save"},
		{"esc", "discard"},
	}},
	{"Descriptions", []helpRow{
		{"", "A KEY description says what the dimension is for."},
		{"", "A VALUE description says when to apply that value."},
		{"", "Lifecycle keys are engine-managed: their vocabulary is locked,"},
		{"", "but descriptions and the required flag are editable."},
	}},
}

func (m EditorModel) helpWidth() int { return min(max(m.width-8, 72), 100) }

func (m EditorModel) renderHelp() string {
	var lines []string
	for i, sec := range helpSections {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, HeadingStyle.Render(sec.title))
		for _, r := range sec.rows {
			if r.keys == "" {
				lines = append(lines, "  "+HelpPanelStyle.Render(r.text))
				continue
			}
			lines = append(lines, "  "+KeyStyle.Width(22).Render(r.keys)+TextStyle.Render(r.text))
		}
	}
	closeLine := KeyStyle.Render("esc") + " " + HintStyle.Render("close")
	inner := m.helpWidth() - 4
	if limit := m.height - 4; len(lines) > limit-2 {
		lines = append(lines[:limit-3], HintStyle.Render("…"))
	}
	lines = append(lines, "", strings.Repeat(" ", max(0, inner-lipgloss.Width(closeLine)))+closeLine)
	box := UnfocusedBorder.Width(m.helpWidth()).Padding(1, 2).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box,
		lipgloss.WithWhitespaceChars(" "))
}

func (m EditorModel) modalWidth() int { return min(max(m.width-8, 72), 120) }
func (m EditorModel) modalInner() int { return m.modalWidth() - 4 }

func (m EditorModel) renderModal() string {
	box := FocusedBorder.Width(m.modalWidth()).Padding(1, 2).Render(m.form.render())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box,
		lipgloss.WithWhitespaceChars(" "))
}

// --- Utility ----------------------------------------------------------------

func wordWrap(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	words := strings.Fields(s)
	var lines []string
	var cur strings.Builder
	for _, w := range words {
		if cur.Len()+len(w)+1 > width && cur.Len() > 0 {
			lines = append(lines, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(w)
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

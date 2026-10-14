package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type fieldKind int

const (
	fieldText fieldKind = iota
	fieldTextArea
	fieldBool
	fieldEnum
	fieldReadOnly
)

type formField struct {
	key, label string
	kind       fieldKind
	locked     bool

	input       textinput.Model // fieldText
	area        textarea.Model  // fieldTextArea
	vp          int             // fieldText: viewport start (rune index)
	help        string
	placeholder string
	boolVal     bool     // fieldBool
	options     []string // fieldEnum
	optIdx      int
	display     map[string]string // fieldEnum: option → label ("" → "(any)")
	roVal       string            // fieldReadOnly

	initial string
}

func (f *formField) current() string {
	switch f.kind {
	case fieldText:
		return strings.TrimSpace(f.input.Value())
	case fieldTextArea:
		return strings.Join(strings.Fields(f.area.Value()), " ")
	case fieldBool:
		return fmt.Sprint(f.boolVal)
	case fieldEnum:
		return f.options[f.optIdx]
	}
	return f.roVal
}

func (f *formField) focusable() bool { return f.kind != fieldReadOnly && !f.locked }

func (fld *formField) clampViewport(inner int) {
	inner = max(inner, 1)
	n, pos := len([]rune(fld.input.Value())), fld.input.Position()
	if pos < fld.vp {
		fld.vp = pos
	}
	if pos > fld.vp+inner-1 {
		fld.vp = pos - inner + 1
	}
	if fld.vp > 0 && n+1-fld.vp < inner {
		fld.vp = max(0, n+1-inner)
	}
	if fld.vp < 0 {
		fld.vp = 0
	}
}

func (f *formField) cycle(delta int) {
	n := len(f.options)
	f.optIdx = ((f.optIdx+delta)%n + n) % n
}

func (f *formField) enumLabel(opt string) string {
	if d, ok := f.display[opt]; ok {
		return d
	}
	return opt
}

type formKind int

const (
	formEditKey formKind = iota
	formEditValue
	formAddKey
	formAddValue
	formConfirmRetire
)

type formResult int

const (
	formOpen formResult = iota
	formSave
	formDiscard
)

type formModel struct {
	kind     formKind
	title    string
	subtitle string
	fields   []formField
	focus    int // 0..len(fields)-1 fields, len(fields) = Save, len(fields)+1 = Discard
	errMsg   string

	saveLabel, discardLabel string
	saveHelp, discardHelp   string

	inner, fieldW int // set by layout

	tagKey, tagValue string
	retireValue      bool
	check            func(vals map[string]string) string
}

func newTextField(key, label, initial string, limit int, placeholder, help string) formField {
	ti := textinput.New()
	ti.CharLimit = limit
	ti.Prompt = ""
	ti.SetValue(initial)
	ti.CursorEnd()
	return formField{key: key, label: label, kind: fieldText, input: ti,
		initial: strings.TrimSpace(initial), placeholder: placeholder, help: help}
}

func newTextAreaField(key, label, initial, placeholder string, limit int, help string) formField {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.CharLimit = limit
	ta.Placeholder = placeholder
	ta.EndOfBufferCharacter = ' '
	ta.KeyMap.InsertNewline.SetEnabled(false)
	ta.FocusedStyle.Base = FieldFocusStyle
	ta.FocusedStyle.CursorLine = FieldFocusStyle
	ta.FocusedStyle.Text = FieldFocusStyle
	ta.FocusedStyle.Placeholder = FieldPlaceholderStyle.Background(ColorFieldFocusBg)
	ta.FocusedStyle.EndOfBuffer = FieldFocusStyle
	ta.BlurredStyle.Base = FieldStyle
	ta.BlurredStyle.CursorLine = FieldStyle
	ta.BlurredStyle.Text = FieldStyle
	ta.BlurredStyle.Placeholder = FieldPlaceholderStyle
	ta.BlurredStyle.EndOfBuffer = FieldStyle
	norm := strings.Join(strings.Fields(initial), " ")
	ta.SetValue(norm)
	ta.CursorStart()
	return formField{key: key, label: label, kind: fieldTextArea, area: ta, initial: norm, help: help}
}

func newBoolField(key, label string, v bool, help string) formField {
	return formField{key: key, label: label, kind: fieldBool, boolVal: v, initial: fmt.Sprint(v), help: help}
}

func newEnumField(key, label, initial string, options []string, display map[string]string, help string) formField {
	f := formField{key: key, label: label, kind: fieldEnum, options: options, display: display, help: help}
	for i, o := range options {
		if o == initial {
			f.optIdx = i
		}
	}
	f.initial = options[f.optIdx]
	return f
}

func newReadOnlyField(key, label, val, help string) formField {
	return formField{key: key, label: label, kind: fieldReadOnly, roVal: val, initial: val, help: help}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

const (
	helpKeyDescription = "What this dimension is for — the rule an agent reads to decide whether this key applies at all. Each value carries its own \"when to apply\" text."
	helpCardinality    = "single: one value per entity (a re-tag replaces it). multi: values accumulate. Fixed after creation; shown for reference."
	helpDiscardEdit    = "Close without writing anything. esc does the same from any field."
)

func newEditKeyForm(k *keyEntry) *formModel {
	f := &formModel{
		kind:  formEditKey,
		title: "Edit Key: " + k.tagKey,
		subtitle: fmt.Sprintf("%s key · %d values · %d entities",
			k.category, k.valueCount, k.entityCount),
		tagKey:    k.tagKey,
		saveLabel: "Save", discardLabel: "Discard",
		saveHelp:    "Write only the fields you changed, then reload the tree with this key highlighted. ctrl+s works from any field.",
		discardHelp: helpDiscardEdit,
	}
	interview := k.interview
	if interview == "" {
		interview = "propose"
	}
	f.fields = []formField{
		newTextAreaField("description", "Description", k.description, "What is this dimension for?", 1024, helpKeyDescription),
		newBoolField("required", "Required", k.required,
			"When on, every work unit must carry this key before it can reach done. Outcomes are exempt. Lifecycle keys may be required too."),
		newReadOnlyField("cardinality", "Cardinality", k.cardinality, helpCardinality),
		newReadOnlyField("category", "Category", k.category,
			"context: operator-owned vocabulary. lifecycle: engine-managed (phase, work-type, …). Cannot be changed here."),
		newEnumField("scope", "Scope", k.scope, scopeValues, map[string]string{"": "(any)"},
			"Advisory: where agents should apply this key. (any) = outcomes and work units; outcome / workunit restrict the proposal."),
		newEnumField("interview", "Interview", interview, interviewValues, nil,
			"How the tag interview treats this key: propose = ask the operator, auto = derive silently, skip = never offer it."),
		newTextField("exclusionGroup", "Excl. group", k.exclusionGroup, 64, "e.g. work-scope",
			"Keys sharing a group slug are mutually exclusive on one entity (e.g. work-scope for the feature/bug/… facets). Blank = none."),
		newEnumField("autoExtract", "Auto-extract", k.autoExtract, autoExtractValues, map[string]string{"": "(none)"},
			"Source agents may read this key from automatically: git (repo/branch metadata) or env (environment variables). (none) = manual only."),
	}
	f.settleFocus()
	return f
}

func newEditValueForm(v *valueEntry) *formModel {
	seed := "not seed"
	if v.isSeed {
		seed = "seed"
	}
	f := &formModel{
		kind:      formEditValue,
		title:     fmt.Sprintf("Edit Value: %s = %s", v.key.tagKey, v.value),
		subtitle:  fmt.Sprintf("%s · %d entities", seed, v.entityCount),
		tagKey:    v.key.tagKey,
		tagValue:  v.value,
		saveLabel: "Save", discardLabel: "Discard",
		saveHelp:    "Write the value description if it changed, then reload the tree with this value highlighted. ctrl+s works from any field.",
		discardHelp: helpDiscardEdit,
	}
	f.fields = []formField{
		newTextAreaField("description", "Description", v.description, "When should an agent pick this value?", 1024,
			"When to apply this specific value — the rubric an agent reads to pick it over its siblings. One paragraph."),
		newReadOnlyField("retired", "Retired", yesNo(v.retired),
			"Retired values stay bound to existing entities but are hidden from the vocabulary. Retire with d from the tree; there is no un-retire yet."),
		newReadOnlyField("seed", "Seed", yesNo(v.isSeed),
			"seed = part of the declared vocabulary (yaml or DefineTag). not seed = minted on first use or added in this editor."),
	}
	f.settleFocus()
	return f
}

var systemManagedKeys = map[string]bool{"phase": true, "work-type": true, "resolution": true, "lifecycle": true}

func newAddKeyForm(prefill, groupLabel string, existing map[string]*keyEntry) *formModel {
	f := &formModel{
		kind:      formAddKey,
		title:     "Add Key",
		subtitle:  "New context key under " + groupLabel,
		saveLabel: "Save", discardLabel: "Discard",
		saveHelp:    "Define the key as a context key with no fixed values (create-on-apply). Add values afterwards with a.",
		discardHelp: "Close without creating anything.",
	}
	f.fields = []formField{
		newTextField("key", "Key name", prefill, 64, "e.g. priority or github.pr",
			"Lowercase slug, no spaces. A dot namespaces the key under an integration group (github.pr). Cannot be renamed later."),
		newTextAreaField("description", "Description", "", "What is this dimension for?", 1024,
			"What this dimension is for. Stored on the key; each value you add afterwards gets its own \"when to apply\" text."),
		newEnumField("cardinality", "Cardinality", "single", []string{"single", "multi"}, nil,
			"single: one value per entity (a re-tag replaces it). multi: values accumulate. Choose now — the editor cannot change it later."),
		newBoolField("required", "Required", false,
			"Make every work unit carry this key before it can reach done. Leave off unless the dimension applies to all work."),
	}
	f.check = func(vals map[string]string) string {
		key := vals["key"]
		switch {
		case key == "":
			return "key name is required"
		case strings.ContainsAny(key, " \t\r\n"):
			return "key name must not contain whitespace"
		case existing[key] != nil:
			return "key already exists"
		case systemManagedKeys[key]:
			return "system-managed key"
		}
		return ""
	}
	f.settleFocus()
	return f
}

func newAddValueForm(k *keyEntry) *formModel {
	desc := strings.Join(strings.Fields(k.description), " ")
	f := &formModel{
		kind:      formAddValue,
		title:     "Add Value: " + k.tagKey,
		subtitle:  truncate(desc, 60),
		tagKey:    k.tagKey,
		saveLabel: "Save", discardLabel: "Discard",
		saveHelp:    fmt.Sprintf("Insert the value under %s and highlight it in the tree.", k.tagKey),
		discardHelp: "Close without adding anything.",
	}
	f.fields = []formField{
		newTextField("value", "Value", "", 128, "e.g. p0",
			"Short slug, no spaces. Appears as <key>=<value> on tagged entities. Retired values can be shown in the tree with r."),
		newTextAreaField("description", "Description", "", "When should an agent pick this value?", 1024,
			"When an agent should pick this value over the key's other values. One paragraph — this is the classification rule."),
	}
	f.check = func(vals map[string]string) string {
		v := vals["value"]
		if v == "" {
			return "value is required"
		}
		if strings.ContainsAny(v, " \t\r\n") {
			return "value must not contain whitespace"
		}
		for _, e := range k.values {
			if e.value == v {
				if e.retired {
					return "value exists (retired — press r to show)"
				}
				return "value already exists"
			}
		}
		return ""
	}
	f.settleFocus()
	return f
}

func newConfirmRetireForm(k *keyEntry, v *valueEntry) *formModel {
	f := &formModel{
		kind:      formConfirmRetire,
		tagKey:    k.tagKey,
		saveLabel: "Retire", discardLabel: "Cancel",
		discardHelp: "Keep everything as it is.",
	}
	if v == nil {
		f.title = "Retire key: " + k.tagKey
		f.subtitle = fmt.Sprintf("Demotes the key (is_seed=0). Bindings are kept. %d entities carry it.", k.entityCount)
		f.saveHelp = "Demotes the key (is_seed=0): hidden from the proposed vocabulary, every existing binding kept. Reversible by re-declaring the key."
	} else {
		f.retireValue = true
		f.tagValue = v.value
		f.title = fmt.Sprintf("Retire value: %s = %s", k.tagKey, v.value)
		f.subtitle = fmt.Sprintf("Hides the value from the vocabulary. Bindings are kept. %d entities carry it.", v.entityCount)
		f.saveHelp = "Marks the value retired: hidden from the vocabulary, bindings kept. Show retired values with r. No un-retire exists yet."
	}
	f.focus = 1
	return f
}

func (f *formModel) saveSlot() int    { return len(f.fields) }
func (f *formModel) discardSlot() int { return len(f.fields) + 1 }

func (f *formModel) slotFocusable(i int) bool {
	if i >= len(f.fields) {
		return true
	}
	return f.fields[i].focusable()
}

// settleFocus puts focus on the first focusable slot and syncs text focus.
func (f *formModel) settleFocus() {
	f.focus = 0
	for !f.slotFocusable(f.focus) {
		f.focus++
	}
	f.syncInputFocus()
}

func (f *formModel) move(delta int) {
	n := len(f.fields) + 2
	for i := 0; i < n; i++ {
		f.focus = ((f.focus+delta)%n + n) % n
		if f.slotFocusable(f.focus) {
			break
		}
	}
	f.syncInputFocus()
}

// layout sizes every control for a modal whose inner content width is inner
// and a terminal of the given height. Called when the form opens and on resize.
func (f *formModel) layout(inner, height int) {
	f.inner = inner
	f.fieldW = max(24, inner-17)
	h := 3
	if height >= 36 {
		h = 5
	}
	for i := range f.fields {
		switch f.fields[i].kind {
		case fieldTextArea:
			f.fields[i].area.SetWidth(f.fieldW)
			f.fields[i].area.SetHeight(h)
		case fieldText:
			f.fields[i].clampViewport(f.fieldW - 2)
		}
	}
}

func (f *formModel) syncInputFocus() {
	for i := range f.fields {
		fld := &f.fields[i]
		switch fld.kind {
		case fieldText:
			if i == f.focus {
				fld.input.Focus()
			} else {
				fld.input.Blur()
			}
		case fieldTextArea:
			if i == f.focus {
				fld.area.Focus()
			} else {
				fld.area.Blur()
			}
		}
	}
}

func (f *formModel) focused() *formField {
	if f.focus < len(f.fields) {
		return &f.fields[f.focus]
	}
	return nil
}

func (f *formModel) activateButton() formResult {
	if f.focus == f.saveSlot() {
		return formSave
	}
	return formDiscard
}

// handleKey applies a key to the form. Any other result than formOpen closes it.
func (f *formModel) handleKey(msg tea.KeyMsg) (formResult, tea.Cmd) {
	key := msg.String()
	fld := f.focused()

	if fld != nil && fld.kind == fieldTextArea {
		switch key {
		case "ctrl+s":
			return formSave, nil
		case "esc":
			return formDiscard, nil
		case "tab", "enter":
			f.move(1)
		case "shift+tab":
			f.move(-1)
		case "down":
			if li := fld.area.LineInfo(); fld.area.Line() == fld.area.LineCount()-1 && li.RowOffset >= li.Height-1 {
				f.move(1)
			} else {
				fld.area, _ = fld.area.Update(msg)
			}
		case "up":
			if li := fld.area.LineInfo(); fld.area.Line() == 0 && li.RowOffset == 0 {
				f.move(-1)
			} else {
				fld.area, _ = fld.area.Update(msg)
			}
		default:
			fld.area, _ = fld.area.Update(msg)
		}
		return formOpen, nil
	}

	switch key {
	case "ctrl+s":
		return formSave, nil
	case "esc":
		return formDiscard, nil
	case "tab", "down":
		f.move(1)
		return formOpen, nil
	case "shift+tab", "up":
		f.move(-1)
		return formOpen, nil
	case "enter":
		switch {
		case fld == nil:
			return f.activateButton(), nil
		case fld.kind == fieldText:
			f.move(1)
		case fld.kind == fieldBool:
			fld.boolVal = !fld.boolVal
		case fld.kind == fieldEnum:
			fld.cycle(1)
		}
		return formOpen, nil
	case " ":
		switch {
		case fld == nil:
			return f.activateButton(), nil
		case fld.kind == fieldBool:
			fld.boolVal = !fld.boolVal
			return formOpen, nil
		case fld.kind == fieldEnum:
			fld.cycle(1)
			return formOpen, nil
		}
	case "left", "right":
		delta := 1
		if key == "left" {
			delta = -1
		}
		switch {
		case fld == nil:
			if f.focus == f.saveSlot() {
				f.focus = f.discardSlot()
			} else {
				f.focus = f.saveSlot()
			}
			return formOpen, nil
		case fld.kind == fieldBool:
			fld.boolVal = !fld.boolVal
			return formOpen, nil
		case fld.kind == fieldEnum:
			fld.cycle(delta)
			return formOpen, nil
		}
	}

	if fld != nil && fld.kind == fieldText {
		fld.input, _ = fld.input.Update(msg)
		fld.clampViewport(f.fieldW - 2)
	}
	return formOpen, nil
}

func (f *formModel) dirty() bool {
	for i := range f.fields {
		if f.fields[i].current() != f.fields[i].initial {
			return true
		}
	}
	return false
}

func (f *formModel) changed(key string) bool {
	for i := range f.fields {
		if f.fields[i].key == key {
			return f.fields[i].current() != f.fields[i].initial
		}
	}
	return false
}

func (f *formModel) values() map[string]string {
	out := make(map[string]string, len(f.fields))
	for i := range f.fields {
		out[f.fields[i].key] = f.fields[i].current()
	}
	return out
}

// validate returns an error message, or "" when the form may be saved.
func (f *formModel) validate() string {
	if f.check == nil {
		return ""
	}
	return f.check(f.values())
}

// renderTextField draws a single-line field on a fixed-width block with
// ‹ › overflow markers. It reads the viewport; handleKey/layout maintain it.
func renderTextField(fld *formField, w int, focused bool) string {
	base, bg := FieldStyle, ColorFieldBg
	if focused {
		base, bg = FieldFocusStyle, ColorFieldFocusBg
	}
	inner := max(w-2, 1)
	runes := []rune(fld.input.Value())
	vp := min(fld.vp, len(runes))
	end := min(len(runes), vp+inner)
	body := runes[vp:end]

	left, right := base.Render(" "), base.Render(" ")
	if vp > 0 {
		left = OverflowStyle.Background(bg).Render("‹")
	}
	if end < len(runes) {
		right = OverflowStyle.Background(bg).Render("›")
	}

	var out string
	used := 0
	switch {
	case len(runes) == 0 && fld.placeholder != "":
		ph := truncate(fld.placeholder, inner-1)
		if focused {
			out = FieldCursorStyle.Render(" ")
			used = 1
		}
		out += FieldPlaceholderStyle.Background(bg).Render(ph)
		used += lipgloss.Width(ph)
	case focused:
		cur := min(max(fld.input.Position()-vp, 0), len(body))
		ch, after := " ", ""
		if cur < len(body) {
			ch, after = string(body[cur]), string(body[cur+1:])
		}
		out = base.Render(string(body[:cur])) + FieldCursorStyle.Render(ch) + base.Render(after)
		used = max(len(body), cur+1)
	default:
		out = base.Render(string(body))
		used = len(body)
	}
	if pad := inner - used; pad > 0 {
		out += base.Render(strings.Repeat(" ", pad))
	}
	return left + out + right
}

func (f *formModel) helpLabel() string {
	switch {
	case f.focus < len(f.fields):
		return f.fields[f.focus].label
	case f.focus == f.saveSlot():
		return f.saveLabel
	}
	return f.discardLabel
}

func (f *formModel) helpFor(slot int) string {
	switch {
	case slot < len(f.fields):
		return f.fields[slot].help
	case slot == f.saveSlot():
		return f.saveHelp
	}
	return f.discardHelp
}

func ellipsize(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:max(w-1, 0)]) + "…"
}

// helpPanel is always exactly two lines so the modal height never changes.
func (f *formModel) helpPanel() []string {
	tw := max(f.inner-13, 10)
	lines := wordWrap(f.helpFor(f.focus), tw)
	l1, l2 := "", ""
	if len(lines) > 0 {
		l1 = lines[0]
	}
	if len(lines) > 1 {
		l2 = ellipsize(strings.Join(lines[1:], " "), tw)
	}
	return []string{
		HelpHeadingStyle.Width(13).Render(truncate(f.helpLabel(), 12)) + HelpPanelStyle.Render(l1),
		strings.Repeat(" ", 13) + HelpPanelStyle.Render(l2),
	}
}

// renderTitle bolds the title, with the key name in the key colour, the value
// (if any) in plain bold text, and a warn-coloured * when dirty.
// controlBg is the block colour behind chips and fields.
func controlBg(focused bool) lipgloss.TerminalColor {
	if focused {
		return ColorFieldFocusBg
	}
	return ColorFieldBg
}

// renderBoolControl draws " [x] yes " with the brackets as chrome and the
// check mark and word as values, all on one background block.
func renderBoolControl(on, focused bool) string {
	bg := controlBg(focused)
	pad := lipgloss.NewStyle().Background(bg).Render(" ")
	chrome := ChromeStyle.Background(bg)
	if on {
		return pad + chrome.Render("[") + GoodStyle.Background(bg).Render("x") + chrome.Render("]") +
			pad + GoodStyle.Background(bg).Render("yes") + pad
	}
	return pad + chrome.Render("[") + pad + chrome.Render("]") + pad + DimStyle.Background(bg).Render("no") + pad
}

// renderEnumControl draws " ◂ value ▸ " (chevrons only when focused) with the
// chevrons as chrome and the value in the data colour.
func renderEnumControl(label string, focused bool) string {
	bg := controlBg(focused)
	pad := lipgloss.NewStyle().Background(bg).Render(" ")
	left, right := pad, pad
	if focused {
		left = ChromeStyle.Background(bg).Render("◂") + pad
		right = pad + ChromeStyle.Background(bg).Render("▸")
	} else {
		left, right = pad+pad, pad+pad
	}
	return pad + left + DataStyle.Background(bg).Render(label) + right + pad
}

func (f *formModel) renderTitle() string {
	t := f.title
	var out string
	if i := strings.Index(t, f.tagKey); f.tagKey != "" && i >= 0 {
		rest := t[i+len(f.tagKey):]
		out = BoldTextStyle.Render(t[:i]) + KeyNameStyle.Render(f.tagKey)
		if f.tagValue != "" && strings.HasSuffix(rest, f.tagValue) {
			out += BoldTextStyle.Render(strings.TrimSuffix(rest, f.tagValue)) + DataStyle.Render(f.tagValue)
		} else {
			out += BoldTextStyle.Render(rest)
		}
	} else {
		out = BoldTextStyle.Render(t)
	}
	if f.dirty() {
		out += " " + WarnStyle.Bold(true).Render("*")
	}
	return out
}

func (f *formModel) renderSubtitle() string {
	lines := wordWrap(f.subtitle, f.inner)
	for i, l := range lines {
		if f.kind == formAddKey || f.kind == formAddValue {
			lines[i] = DimStyle.Render(l)
		} else {
			lines[i] = colorNumbers(l)
		}
	}
	return strings.Join(lines, "\n")
}

func (f *formModel) render() string {
	var b strings.Builder
	b.WriteString(f.renderTitle() + "\n")
	b.WriteString(f.renderSubtitle() + "\n")

	if len(f.fields) > 0 {
		b.WriteString("\n")
	}
	for i := range f.fields {
		fld := &f.fields[i]
		focused := i == f.focus
		prefix := "  "
		if focused {
			prefix = AccentStyle.Render("▸ ")
		}
		label := FieldLabelStyle.Render(fld.label)
		switch {
		case !fld.focusable():
			label = LockedStyle.Width(14).Render(fld.label)
		case focused:
			label = FieldLabelFocusStyle.Render(fld.label)
		}

		var val string
		switch fld.kind {
		case fieldTextArea:
			val = fld.area.View()
		case fieldText:
			val = renderTextField(fld, f.fieldW, focused)
		case fieldBool:
			val = renderBoolControl(fld.boolVal, focused)
		case fieldEnum:
			val = renderEnumControl(fld.enumLabel(fld.options[fld.optIdx]), focused)
		}
		if !fld.focusable() {
			val = DataDimStyle.Render(fld.enumLabel(fld.current()))
		}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, prefix+label+" ", val) + "\n")
	}

	b.WriteString("\n" + RuleStyle.Render(strings.Repeat("─", f.inner)) + "\n")
	b.WriteString(strings.Join(f.helpPanel(), "\n") + "\n")

	if f.errMsg != "" {
		b.WriteString("\n" + ErrorStyle.Render("  ⚠ "+f.errMsg) + "\n")
	}

	save, discard := ButtonStyle, ButtonStyle
	if f.focus == f.saveSlot() {
		save = ButtonSaveFocusStyle
		if f.kind == formConfirmRetire {
			save = ButtonDangerFocusStyle
		}
	}
	if f.focus == f.discardSlot() {
		discard = ButtonNeutralFocusStyle
	}
	b.WriteString("\n  " + save.Render(f.saveLabel) + "   " + discard.Render(f.discardLabel) + "\n")

	hints := []hint{{"tab/↑↓", "field"}, {"space", "toggle"}, {"⏎", "next"}, {"ctrl+s", "save"}, {"esc", "discard"}}
	if fld := f.focused(); fld != nil && fld.kind == fieldTextArea {
		hints = []hint{{"↑↓", "move in text"}, {"tab", "next field"}, {"⏎", "next"}, {"ctrl+s", "save"}, {"esc", "discard"}}
	}
	b.WriteString(renderHints(hints, f.inner))
	return b.String()
}

var (
	scopeValues       = []string{"", "outcome", "workunit"}
	interviewValues   = []string{"propose", "auto", "skip"}
	autoExtractValues = []string{"", "git", "env"}
)

func cycleValue(current string, options []string) string {
	for i, v := range options {
		if v == current {
			return options[(i+1)%len(options)]
		}
	}
	return options[0]
}

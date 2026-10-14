package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// tagEditorStore is the editor's dependency: the full TagAdminStore surface
// plus the four wms methods (DefineTag, RetireTag, UpdateTagValueDescription,
// ListTags) its actions need — those stay on wms.Writer/Reader per the design
// (the MCP path), not duplicated onto TagAdminStore. store.Store satisfies
// this structurally, so callers pass their store.Store unchanged.
type tagEditorStore interface {
	store.TagAdminStore
	DefineTag(ctx context.Context, spec wms.TagSpec) error
	RetireTag(ctx context.Context, tagKey string) error
	UpdateTagValueDescription(ctx context.Context, tagKey, tagValue, description string) error
	ListTags(ctx context.Context) ([]wms.Tag, error)
}

// --- Messages ---------------------------------------------------------------

type treeLoaded struct {
	keys   []wms.TagKeySummary
	rows   []wms.Tag
	counts []store.TagCountRow
	err    error
}

type detailLoaded struct {
	seq    int
	detail store.TagValueDetail
	err    error
}

type writeDone struct {
	err      error
	status   string
	selectID string
	expand   []string
}

type statusExpired struct{ seq int }

// --- Model ------------------------------------------------------------------

type paneFocus int

const (
	focusTree paneFocus = iota
	focusDetail
)

type detailState int

const (
	detailIdle detailState = iota
	detailLoading
	detailReady
	detailError
)

const lifecycleBlockMsg = "engine-managed key: vocabulary is locked (descriptions and required are editable)"

// EditorModel is the bubbletea model for the full-screen tree + detail tag
// editor. Exported so the wizard can embed/transition to it.
type EditorModel struct {
	st tagEditorStore

	groups   []*groupEntry
	keys     map[string]*keyEntry
	tree     []*treeNode
	expanded map[string]bool
	visible  []*treeNode
	cursor   int

	detail       store.TagValueDetail
	detailSeq    int
	detailState  detailState
	detailErr    string
	detailScroll int

	focus         paneFocus
	width, height int
	filterActive  bool
	filterInput   textinput.Model
	filter        string
	showRetired   bool
	showHelp      bool
	form          *formModel
	statusMsg     string
	statusSeq     int
	pendingSelect string

	loading bool
	err     string
}

// NewEditor creates an EditorModel ready to run. Call tea.NewProgram(NewEditor(st)).
func NewEditor(st tagEditorStore) EditorModel {
	fi := textinput.New()
	fi.Placeholder = "filter…"
	fi.CharLimit = 64
	fi.Width = 24
	return EditorModel{
		st:          st,
		expanded:    map[string]bool{},
		keys:        map[string]*keyEntry{},
		filterInput: fi,
		loading:     true,
	}
}

// RunEditor is the standalone entry point for `teamster setup tags` on a system
// with existing tag data.
func RunEditor(st tagEditorStore) error {
	p := tea.NewProgram(NewEditor(st), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// --- Commands ---------------------------------------------------------------

func (m EditorModel) Init() tea.Cmd { return loadTree(m.st) }

func loadTree(st tagEditorStore) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		keys, err := st.TagKeys(ctx)
		if err != nil {
			return treeLoaded{err: err}
		}
		rows, err := st.ListTags(ctx)
		if err != nil {
			return treeLoaded{err: err}
		}
		counts, err := st.TagEntityCounts(ctx)
		if err != nil {
			return treeLoaded{err: err}
		}
		return treeLoaded{keys: keys, rows: rows, counts: counts}
	}
}

func loadDetail(st tagEditorStore, tagKey, tagValue string, seq int) tea.Cmd {
	return func() tea.Msg {
		d, err := st.TagValueDetail(context.Background(), tagKey, tagValue)
		return detailLoaded{seq: seq, detail: d, err: err}
	}
}

func (m *EditorModel) requestDetail() tea.Cmd {
	n := m.cur()
	if n == nil || n.kind != nodeValue {
		m.detailState = detailIdle
		return nil
	}
	m.detailSeq++
	m.detailState = detailLoading
	return loadDetail(m.st, n.key.tagKey, n.value.value, m.detailSeq)
}

// onCursorMoved resets detail state for the newly highlighted node.
func (m *EditorModel) onCursorMoved() tea.Cmd {
	m.detailScroll = 0
	return m.requestDetail()
}

// afterNav runs onCursorMoved only when the highlighted node changed.
func (m *EditorModel) afterNav(prevID string) tea.Cmd {
	if m.currentID() == prevID {
		return nil
	}
	return m.onCursorMoved()
}

func (m *EditorModel) setStatus(s string) tea.Cmd {
	m.statusSeq++
	m.statusMsg = s
	seq := m.statusSeq
	return tea.Tick(4*time.Second, func(time.Time) tea.Msg { return statusExpired{seq: seq} })
}

// writeCmd runs the store calls sequentially, aborting on the first error.
func writeCmd(w writeDone, calls ...func(ctx context.Context) error) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		for _, c := range calls {
			if err := c(ctx); err != nil {
				w.err = err
				break
			}
		}
		return w
	}
}

// --- Update -----------------------------------------------------------------

func (m EditorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.form != nil {
			m.form.layout(m.modalInner(), m.height)
		}
		return m, nil

	case treeLoaded:
		return m.onTreeLoaded(msg)

	case detailLoaded:
		if msg.seq != m.detailSeq {
			return m, nil
		}
		if msg.err != nil {
			m.detailState, m.detailErr = detailError, msg.err.Error()
			return m, nil
		}
		m.detail, m.detailState, m.detailErr = msg.detail, detailReady, ""
		return m, nil

	case writeDone:
		var cmds []tea.Cmd
		if msg.err != nil {
			cmds = append(cmds, m.setStatus("error: "+msg.err.Error()))
		} else if msg.status != "" {
			cmds = append(cmds, m.setStatus("✓ "+msg.status))
		}
		m.pendingSelect = msg.selectID
		for _, id := range msg.expand {
			m.expanded[id] = true
		}
		cmds = append(cmds, loadTree(m.st))
		return m, tea.Batch(cmds...)

	case statusExpired:
		if msg.seq == m.statusSeq {
			m.statusMsg = ""
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m EditorModel) onTreeLoaded(msg treeLoaded) (tea.Model, tea.Cmd) {
	first := m.loading
	m.loading = false
	if msg.err != nil {
		if first {
			m.err = msg.err.Error()
			return m, nil
		}
		return m, m.setStatus("error: " + msg.err.Error())
	}
	prevID := m.currentID()
	m.groups, m.keys, m.tree = buildTree(msg)
	m.rebuildVisible()
	m.restoreCursor(m.pendingSelect, prevID)
	m.pendingSelect = ""
	return m, m.onCursorMoved()
}

func (m EditorModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch {
	case m.showHelp:
		if k := msg.String(); k == "esc" || k == "?" {
			m.showHelp = false
		}
	case m.form != nil:
		cmd = m.handleFormKey(msg)
	case m.filterActive:
		cmd = m.handleFilterKey(msg)
	case m.focus == focusDetail:
		cmd = m.handleDetailKey(msg.String())
	default:
		cmd = m.handleTreeKey(msg.String())
	}
	return m, cmd
}

func (m *EditorModel) handleFormKey(msg tea.KeyMsg) tea.Cmd {
	res, cmd := m.form.handleKey(msg)
	switch res {
	case formDiscard:
		m.form = nil
	case formSave:
		return m.submitForm()
	}
	return cmd
}

func (m *EditorModel) handleFilterKey(msg tea.KeyMsg) tea.Cmd {
	prev := m.currentID()
	switch msg.String() {
	case "esc":
		m.filterActive = false
		m.filter = ""
		m.filterInput.Reset()
		m.filterInput.Blur()
		m.rebuildVisible()
		m.restoreCursor(prev)
		return m.afterNav(prev)
	case "enter":
		m.filterActive = false
		m.filterInput.Blur()
		return nil
	}
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	m.filter = m.filterInput.Value()
	m.rebuildVisible()
	return tea.Batch(cmd, m.afterNav(prev))
}

func (m *EditorModel) bodyHeight() int { return m.height - 5 }

func (m *EditorModel) scrollDetail(delta int) {
	_, detailW := m.columnWidths()
	maxScroll := max(0, len(m.detailLines(detailW))-m.bodyHeight())
	m.detailScroll = min(max(0, m.detailScroll+delta), maxScroll)
}

func (m *EditorModel) handleDetailKey(key string) tea.Cmd {
	switch key {
	case "up", "k":
		m.scrollDetail(-1)
	case "down", "j":
		m.scrollDetail(1)
	case "pgup":
		m.scrollDetail(-max(1, m.bodyHeight()-2))
	case "pgdown":
		m.scrollDetail(max(1, m.bodyHeight()-2))
	case "tab", "shift+tab", "esc":
		m.focus = focusTree
	case "e", "enter":
		m.openEditForm()
	case "?":
		m.showHelp = true
	case "q", "ctrl+c":
		return tea.Quit
	}
	return nil
}

func (m *EditorModel) moveCursor(delta int) tea.Cmd {
	prev := m.currentID()
	m.cursor = min(max(0, m.cursor+delta), len(m.visible)-1)
	m.cursor = max(m.cursor, 0)
	return m.afterNav(prev)
}

func (m *EditorModel) setExpanded(n *treeNode, open bool) {
	m.expanded[n.id] = open
	m.rebuildVisible()
	m.restoreCursor(n.id)
}

func (m *EditorModel) handleTreeKey(key string) tea.Cmd {
	n := m.cur()
	switch key {
	case "q", "ctrl+c":
		return tea.Quit
	case "?":
		m.showHelp = true
		return nil
	case "tab":
		m.focus = focusDetail
		return nil
	case "/":
		m.filterActive = true
		m.filterInput.SetValue(m.filter)
		m.filterInput.CursorEnd()
		m.filterInput.Focus()
		return nil
	case "R":
		m.pendingSelect = m.currentID()
		return tea.Batch(m.setStatus("reloaded"), loadTree(m.st))
	case "r":
		prev := m.currentID()
		m.showRetired = !m.showRetired
		m.rebuildVisible()
		m.restoreCursor(prev)
		status := "hiding retired values"
		if m.showRetired {
			status = "showing retired values"
		}
		return tea.Batch(m.setStatus(status), m.afterNav(prev))
	case "up", "k":
		return m.moveCursor(-1)
	case "down", "j":
		return m.moveCursor(1)
	case "A":
		m.openAddKeyForm()
		return nil
	}
	if n == nil {
		return nil
	}

	switch key {
	case "right", "l":
		if (n.kind == nodeGroup || n.kind == nodeKey) && !n.open && n.visKids > 0 {
			m.setExpanded(n, true)
		}
	case "left", "h":
		switch {
		case n.kind != nodeValue && n.open:
			m.setExpanded(n, false)
		case n.parent != nil:
			prev := n.id
			if idx := m.indexOfVisible(n.parent.id); idx >= 0 {
				m.cursor = idx
			}
			return m.afterNav(prev)
		}
	case " ":
		if n.kind != nodeValue && (n.open || n.visKids > 0) {
			m.setExpanded(n, !n.open)
		}
	case "enter":
		if n.kind == nodeGroup {
			m.setExpanded(n, !n.open)
		} else {
			m.openEditForm()
		}
	case "e":
		m.openEditForm()
	case "t":
		if n.kind == nodeGroup {
			return nil
		}
		k := keyOf(n).key
		status := "required on: " + k.tagKey
		if k.required {
			status = "required off: " + k.tagKey
		}
		tagKey, req, st := k.tagKey, !k.required, m.st
		return writeCmd(writeDone{status: status, selectID: n.id},
			func(ctx context.Context) error { return st.SetTagRequired(ctx, tagKey, req) })
	case "a":
		if n.kind == nodeGroup {
			return nil
		}
		k := keyOf(n).key
		if k.lifecycle {
			return m.setStatus(lifecycleBlockMsg)
		}
		m.openForm(newAddValueForm(k))
	case "d":
		switch n.kind {
		case nodeKey:
			if n.key.lifecycle {
				return m.setStatus(lifecycleBlockMsg)
			}
			if !n.key.isSeed {
				k, st := n.key, m.st
				req := k.required
				spec := wms.TagSpec{Key: k.tagKey, Category: k.category, Cardinality: k.cardinality, Description: k.description, Required: &req}
				return writeCmd(writeDone{status: "restored key " + k.tagKey, selectID: k.id},
					func(ctx context.Context) error { return st.DefineTag(ctx, spec) })
			}
			m.openForm(newConfirmRetireForm(n.key, nil))
		case nodeValue:
			if n.key.lifecycle {
				return m.setStatus(lifecycleBlockMsg)
			}
			if n.value.retired {
				key, val, st := n.key.tagKey, n.value.value, m.st
				return writeCmd(writeDone{status: fmt.Sprintf("restored %s = %s", key, val), selectID: n.value.id},
					func(ctx context.Context) error { return st.UnretireTagValue(ctx, key, val) })
			}
			m.openForm(newConfirmRetireForm(n.key, n.value))
		}
	}
	return nil
}

func (m *EditorModel) openForm(f *formModel) {
	f.layout(m.modalInner(), m.height)
	m.form = f
}

func (m *EditorModel) openEditForm() {
	n := m.cur()
	if n == nil {
		return
	}
	switch n.kind {
	case nodeKey:
		m.openForm(newEditKeyForm(n.key))
	case nodeValue:
		m.openForm(newEditValueForm(n.value))
	}
}

func (m *EditorModel) openAddKeyForm() {
	prefill, label := "", "Context"
	if n := m.cur(); n != nil {
		g := groupOf(n).group
		label = g.label
		if g.namespace != "" && !g.lifecycle {
			prefill = g.namespace + "."
		}
		if g.lifecycle {
			label = "Context"
		}
	}
	m.openForm(newAddKeyForm(prefill, label, m.keys))
}

// --- Save -------------------------------------------------------------------

// submitForm validates the open form and, if valid, closes it and returns the
// command that performs only the store calls its changed fields need.
func (m *EditorModel) submitForm() tea.Cmd {
	f := m.form
	if msg := f.validate(); msg != "" {
		f.errMsg = msg
		return nil
	}
	m.form = nil
	vals := f.values()
	st := m.st
	key := f.tagKey

	switch f.kind {
	case formEditKey:
		var calls []func(context.Context) error
		if f.changed("description") {
			d := vals["description"]
			calls = append(calls, func(ctx context.Context) error { return st.UpdateTagDescription(ctx, key, d) })
		}
		if f.changed("required") {
			req := vals["required"] == "true"
			calls = append(calls, func(ctx context.Context) error { return st.SetTagRequired(ctx, key, req) })
		}
		if f.changed("scope") || f.changed("interview") || f.changed("exclusionGroup") || f.changed("autoExtract") {
			interview := vals["interview"]
			if !f.changed("interview") {
				interview = m.keys[key].interview
			}
			scope, excl, auto := vals["scope"], vals["exclusionGroup"], vals["autoExtract"]
			calls = append(calls, func(ctx context.Context) error {
				return st.UpdateTagConventions(ctx, key, scope, excl, auto, interview)
			})
		}
		if len(calls) == 0 {
			return m.setStatus("no changes")
		}
		return writeCmd(writeDone{status: "saved " + key, selectID: keyNodeID(key)}, calls...)

	case formEditValue:
		if !f.dirty() {
			return m.setStatus("no changes")
		}
		val, d := f.tagValue, vals["description"]
		return writeCmd(writeDone{status: fmt.Sprintf("saved %s = %s", key, val), selectID: valueNodeID(key, val)},
			func(ctx context.Context) error { return st.UpdateTagValueDescription(ctx, key, val, d) })

	case formAddKey:
		newKey := vals["key"]
		spec := wms.TagSpec{
			Key:         newKey,
			Category:    "context",
			Cardinality: vals["cardinality"],
			Description: vals["description"],
		}
		req := vals["required"] == "true"
		spec.Required = &req
		return writeCmd(writeDone{
			status:   "added key " + newKey,
			selectID: keyNodeID(newKey),
			expand:   []string{groupIDForKey(newKey, false)},
		}, func(ctx context.Context) error { return st.DefineTag(ctx, spec) })

	case formAddValue:
		val, d := vals["value"], vals["description"]
		return writeCmd(writeDone{
			status:   fmt.Sprintf("added %s = %s", key, val),
			selectID: valueNodeID(key, val),
			expand:   []string{keyNodeID(key)},
		}, func(ctx context.Context) error { return st.AddTagValue(ctx, key, val, d) })

	case formConfirmRetire:
		if f.retireValue {
			val := f.tagValue
			return writeCmd(writeDone{status: fmt.Sprintf("retired %s = %s", key, val), selectID: keyNodeID(key)},
				func(ctx context.Context) error { return st.RetireTagValue(ctx, key, val) })
		}
		return writeCmd(writeDone{status: "retired key " + key, selectID: keyNodeID(key)},
			func(ctx context.Context) error { return st.RetireTag(ctx, key) })
	}
	return nil
}

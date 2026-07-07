package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

// tagEditorStore is the editor's dependency: the full TagAdminStore surface
// plus the three wms.Writer methods (DefineTag/RetireTag/
// UpdateTagValueDescription) its add-key/retire-key/edit-value-description
// actions need — those stay on wms.Writer per the design (the MCP write
// path), not duplicated onto TagAdminStore. store.Store satisfies this
// structurally, so callers pass their store.Store unchanged.
type tagEditorStore interface {
	store.TagAdminStore
	DefineTag(ctx context.Context, spec wms.TagSpec) error
	RetireTag(ctx context.Context, tagKey string) error
	UpdateTagValueDescription(ctx context.Context, tagKey, tagValue, description string) error
}

// --- Data types -------------------------------------------------------------

type keyEntry struct {
	tagKey      string
	category    string
	cardinality string
	description string
	required    bool
	entityCount int

	scope          string
	exclusionGroup string
	autoExtract    string
	interview      string
}

type keyGroup struct {
	namespace string // "" = top-level context key; "github" = integration group; "lifecycle" = lifecycle section
	label     string // display label for group header
	keys      []keyEntry
	collapsed bool
	isSection bool // true = non-selectable section header
}

type tagValue struct {
	value       string
	isSeed      bool
	description string
	entityCount int
	retired     bool
}

type entityRef struct {
	entityType string
	entityID   string
	title      string
}

type valueDetail struct {
	value       string
	isSeed      bool
	description string
	entityCount int
	entities    []entityRef
}

// --- Messages ---------------------------------------------------------------

type editorDataLoaded struct {
	groups []keyGroup
	err    error
}

type valuesLoaded struct {
	values []tagValue
	err    error
}

type detailLoaded struct {
	detail valueDetail
	err    error
}

type dbWriteDone struct{ err error }

// --- Modal model ------------------------------------------------------------

type modalMode int

const (
	modalNone modalMode = iota
	modalAddKey
	modalAddValue
	modalEditDesc
	modalHelp
	modalReadOnly // shown briefly when lifecycle key action attempted
)

type modalModel struct {
	mode       modalMode
	title      string
	input      textinput.Model
	input2     textinput.Model // category for add-key
	input3     textinput.Model // cardinality for add-key
	fieldIdx   int             // which field has focus in multi-field modal
	editTarget int             // 0=key, 1=value — which column triggered modalEditDesc
	errMsg     string
}

func newModal(mode modalMode) *modalModel {
	ti := textinput.New()
	ti.Focus()
	ti.CharLimit = 64
	ti.Width = 30

	ti2 := textinput.New()
	ti2.CharLimit = 16
	ti2.Width = 16
	ti2.Placeholder = "context"

	ti3 := textinput.New()
	ti3.CharLimit = 8
	ti3.Width = 16
	ti3.Placeholder = "single"

	var title string
	switch mode {
	case modalAddKey:
		title = "Add Key"
	case modalAddValue:
		title = "Add Value"
	case modalEditDesc:
		title = "Edit Description"
		// Descriptions carry rich, rule-bearing text — match tags.description
		// (varchar(1024)). Key/value name modals keep the shorter 64 cap.
		ti.CharLimit = 1024
	}
	return &modalModel{
		mode:   mode,
		title:  title,
		input:  ti,
		input2: ti2,
		input3: ti3,
	}
}

// --- EditorModel ------------------------------------------------------------

// editorMode selects which family of tag keys the editor shows. The two
// families are never visible at once: context mode (the default) shows the
// operator-owned Context + integration keys exactly as before lifecycle keys
// existed; lifecycle mode shows only the engine-managed lifecycle keys.
type editorMode int

const (
	modeContext   editorMode = iota // default: Context + integration keys
	modeLifecycle                   // engine-managed lifecycle keys only
)

// EditorModel is the bubbletea model for the full-screen 3-column tag editor.
// Export it so the wizard can embed/transition to it.
type EditorModel struct {
	st tagEditorStore

	groups    []keyGroup
	keyCursor int          // flat index into visible key rows
	visKeys   []visibleKey // flattened view of groups for navigation

	values    []tagValue
	valCursor int

	detail    valueDetail
	detailErr string

	focusCol int // 0=keys, 1=values, 2=detail

	width  int
	height int

	filterMode  bool
	filterInput textinput.Model
	filterVal   string

	modal    *modalModel
	showHelp bool

	selectedKey   keyEntry
	selectedValue tagValue

	statusMsg   string
	statusTimer int // countdown ticks to clear statusMsg

	showRetired bool

	mode editorMode // modeContext (default) | modeLifecycle — gates key visibility

	loading bool
	err     string
}

type visibleKey struct {
	entry     *keyEntry
	group     *keyGroup
	isHeader  bool   // section header
	isGroupHd bool   // collapsible group header
	indent    string // prefix indent
	label     string // display text
}

// NewEditor creates an EditorModel ready to run. Call tea.NewProgram(NewEditor(st)).
func NewEditor(st tagEditorStore) EditorModel {
	fi := textinput.New()
	fi.Placeholder = "filter..."
	fi.CharLimit = 32
	fi.Width = 20

	m := EditorModel{
		st:          st,
		filterInput: fi,
		loading:     true,
	}
	return m
}

// RunEditor is the standalone entry point for `teamster setup tags` on a system
// with existing tag data.
func RunEditor(st tagEditorStore) error {
	m := NewEditor(st)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// --- Init -------------------------------------------------------------------

func (m EditorModel) Init() tea.Cmd {
	return m.loadData()
}

func (m EditorModel) loadData() tea.Cmd {
	return func() tea.Msg {
		groups, err := loadKeyGroups(m.st)
		return editorDataLoaded{groups: groups, err: err}
	}
}

// loadKeyGroups fetches the per-key rollup and the shared entity-count read
// model, then groups keys into context / integration-namespace / lifecycle
// sections for column 1. Entity counts are summed per key from
// TagEntityCounts rather than a per-key raw join, since a key's bindings can
// span multiple entity types.
func loadKeyGroups(st store.TagAdminStore) ([]keyGroup, error) {
	ctx := context.Background()
	summaries, err := st.TagKeys(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := st.TagEntityCounts(ctx)
	if err != nil {
		return nil, err
	}
	entityCountByKey := map[string]int{}
	for _, c := range counts {
		entityCountByKey[c.TagKey] += int(c.Count)
	}

	// Build groups: context keys, integration namespaces, lifecycle section.
	contextGroup := keyGroup{label: "Context", isSection: true}
	lifecycleGroup := keyGroup{namespace: "lifecycle", label: "Lifecycle (engine-managed)", isSection: true}
	integrationGroups := map[string]*keyGroup{}
	var integrationOrder []string

	for _, sm := range summaries {
		entry := keyEntry{
			tagKey:         sm.Key,
			category:       sm.Category,
			cardinality:    sm.Cardinality,
			description:    sm.Description,
			required:       sm.Required,
			entityCount:    entityCountByKey[sm.Key],
			scope:          sm.Scope,
			exclusionGroup: sm.ExclusionGroup,
			autoExtract:    sm.AutoExtract,
			interview:      sm.Interview,
		}

		if sm.Category == "lifecycle" {
			lifecycleGroup.keys = append(lifecycleGroup.keys, entry)
			continue
		}

		if idx := strings.Index(sm.Key, "."); idx > 0 {
			ns := sm.Key[:idx]
			if _, ok := integrationGroups[ns]; !ok {
				integrationGroups[ns] = &keyGroup{namespace: ns, label: ns}
				integrationOrder = append(integrationOrder, ns)
			}
			integrationGroups[ns].keys = append(integrationGroups[ns].keys, entry)
			continue
		}

		contextGroup.keys = append(contextGroup.keys, entry)
	}

	var groups []keyGroup
	groups = append(groups, contextGroup)
	for _, ns := range integrationOrder {
		groups = append(groups, *integrationGroups[ns])
	}
	// Lifecycle keys are engine-managed: their vocabulary, category, and
	// cardinality are locked, but per-value descriptions and the required flag
	// are editable. Render the section only when it has keys.
	if len(lifecycleGroup.keys) > 0 {
		groups = append(groups, lifecycleGroup)
	}
	return groups, nil
}

// loadValues fetches every value of tagKey (retired included) plus the
// shared entity-count read model, filters by showRetired, and sorts by
// entity count desc then value — matching the prior raw-join ordering.
func loadValues(st store.TagAdminStore, tagKey string, showRetired bool) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		all, err := st.TagValues(ctx, tagKey)
		if err != nil {
			return valuesLoaded{err: err}
		}
		counts, err := st.TagEntityCounts(ctx)
		if err != nil {
			return valuesLoaded{err: err}
		}
		countByValue := map[string]int{}
		for _, c := range counts {
			if c.TagKey != tagKey {
				continue
			}
			countByValue[c.TagValue] += int(c.Count)
		}

		var vals []tagValue
		for _, t := range all {
			if t.Retired && !showRetired {
				continue
			}
			vals = append(vals, tagValue{
				value:       t.Value,
				isSeed:      t.IsSeed,
				description: t.Description,
				entityCount: countByValue[t.Value],
				retired:     t.Retired,
			})
		}
		sort.SliceStable(vals, func(i, j int) bool {
			if vals[i].entityCount != vals[j].entityCount {
				return vals[i].entityCount > vals[j].entityCount
			}
			return vals[i].value < vals[j].value
		})
		return valuesLoaded{values: vals}
	}
}

func loadDetail(st store.TagAdminStore, tagKey, tagValue string) tea.Cmd {
	return func() tea.Msg {
		detail, err := st.TagValueDetail(context.Background(), tagKey, tagValue)
		if err != nil {
			return detailLoaded{err: err}
		}
		var refs []entityRef
		for _, e := range detail.BoundEntities {
			refs = append(refs, entityRef{entityType: e.EntityType, entityID: e.EntityID, title: e.Why})
		}
		return detailLoaded{detail: valueDetail{
			value:       detail.Value,
			isSeed:      detail.IsSeed,
			description: detail.Description,
			entityCount: int(detail.EntityCount),
			entities:    refs,
		}}
	}
}

// --- Update -----------------------------------------------------------------

func (m EditorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case editorDataLoaded:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		m.groups = msg.groups
		m.rebuildVisKeys()
		if len(m.visKeys) > 0 {
			m.syncSelectedKey()
			return m, m.loadValuesCmd()
		}
		return m, nil

	case valuesLoaded:
		if msg.err != nil {
			m.statusMsg = "error loading values: " + msg.err.Error()
			return m, nil
		}
		m.values = msg.values
		m.valCursor = 0
		m.detail = valueDetail{}
		if len(m.values) > 0 {
			m.selectedValue = m.values[0]
			return m, loadDetail(m.st, m.selectedKey.tagKey, m.selectedValue.value)
		}
		return m, nil

	case detailLoaded:
		if msg.err != nil {
			m.detailErr = msg.err.Error()
			return m, nil
		}
		m.detail = msg.detail
		m.detailErr = ""
		return m, nil

	case dbWriteDone:
		if msg.err != nil {
			m.statusMsg = "error: " + msg.err.Error()
		}
		return m, m.loadData()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	if m.statusTimer > 0 {
		m.statusTimer--
		if m.statusTimer == 0 {
			m.statusMsg = ""
		}
	}
	return m, nil
}

func (m *EditorModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Help overlay consumes all keys except Esc.
	if m.showHelp {
		if msg.String() == "esc" || msg.String() == "?" {
			m.showHelp = false
		}
		return m, nil
	}

	// Modal consumes all keys.
	if m.modal != nil {
		return m.handleModalKey(msg)
	}

	// Filter mode in column 0 or 1.
	if m.filterMode {
		return m.handleFilterKey(msg)
	}

	key := msg.String()

	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "?":
		m.showHelp = true
		return m, nil

	case "tab":
		m.focusCol = (m.focusCol + 1) % 3
		return m, nil

	case "shift+tab":
		m.focusCol = (m.focusCol + 2) % 3
		return m, nil

	case "/":
		if m.focusCol <= 1 {
			m.filterMode = true
			m.filterInput.Reset()
			m.filterInput.Focus()
		}
		return m, nil

	case "L":
		return m.toggleMode()
	}

	switch m.focusCol {
	case 0:
		return m.handleCol0Key(key)
	case 1:
		return m.handleCol1Key(key)
	case 2:
		// col 2 is read-only detail; only navigation
		return m, nil
	}
	return m, nil
}

func (m *EditorModel) handleCol0Key(key string) (tea.Model, tea.Cmd) {
	vks := m.visKeys
	if len(vks) == 0 {
		return m, nil
	}

	switch key {
	case "up", "k":
		for {
			if m.keyCursor > 0 {
				m.keyCursor--
			} else {
				break
			}
			if !vks[m.keyCursor].isHeader {
				break
			}
		}
		m.syncSelectedKey()
		return m, m.loadValuesCmd()

	case "down", "j":
		for {
			if m.keyCursor < len(vks)-1 {
				m.keyCursor++
			} else {
				break
			}
			if !vks[m.keyCursor].isHeader {
				break
			}
		}
		m.syncSelectedKey()
		return m, m.loadValuesCmd()

	case "enter", "right", "l":
		vk := vks[m.keyCursor]
		if vk.isGroupHd {
			vk.group.collapsed = !vk.group.collapsed
			m.rebuildVisKeys()
			// keep cursor on the same group header
			m.findGroupHeaderCursor(vk.group)
		}
		return m, nil

	case "left", "h":
		vk := vks[m.keyCursor]
		if vk.isGroupHd && !vk.group.collapsed {
			vk.group.collapsed = true
			m.rebuildVisKeys()
			m.findGroupHeaderCursor(vk.group)
		}
		return m, nil

	case "a":
		if m.currentKeyIsLifecycle() {
			m.setReadOnlyMsg()
			return m, nil
		}
		modal := newModal(modalAddKey)
		m.modal = modal
		return m, nil

	case "d":
		if m.currentKeyIsLifecycle() {
			m.setReadOnlyMsg()
			return m, nil
		}
		if m.selectedKey.tagKey == "" {
			return m, nil
		}
		return m, m.retireKeyCmd()

	case "e":
		if m.currentKeyIsLifecycle() {
			m.setReadOnlyMsg()
			return m, nil
		}
		if m.selectedKey.tagKey == "" {
			return m, nil
		}
		modal := newModal(modalEditDesc)
		modal.title = "Edit Description: " + m.selectedKey.tagKey
		modal.input.SetValue(m.selectedKey.description)
		m.modal = modal
		return m, nil

	case "t":
		// Toggle the required flag for the focused key. Unlike a/d/e this is a
		// policy property (like cardinality), allowed on lifecycle keys too —
		// work-type ships required and is lifecycle.
		if m.selectedKey.tagKey == "" {
			return m, nil
		}
		newReq := !m.selectedKey.required
		return m, m.toggleRequiredCmd(m.selectedKey.tagKey, newReq)

	case "s":
		if m.selectedKey.tagKey == "" {
			return m, nil
		}
		k := m.selectedKey
		k.scope = cycleValue(k.scope, scopeValues)
		return m, m.updateConventionCmd(k.tagKey, k.scope, k.exclusionGroup, k.autoExtract, k.interview)

	case "i":
		if m.selectedKey.tagKey == "" {
			return m, nil
		}
		k := m.selectedKey
		k.interview = cycleValue(k.interview, interviewValues)
		return m, m.updateConventionCmd(k.tagKey, k.scope, k.exclusionGroup, k.autoExtract, k.interview)

	case "x":
		if m.selectedKey.tagKey == "" {
			return m, nil
		}
		modal := newModal(modalEditDesc)
		modal.title = "Exclusion Group: " + m.selectedKey.tagKey
		modal.input.SetValue(m.selectedKey.exclusionGroup)
		modal.input.Placeholder = "e.g. work-scope"
		modal.editTarget = 2
		m.modal = modal
		return m, nil

	case "X":
		if m.selectedKey.tagKey == "" {
			return m, nil
		}
		k := m.selectedKey
		k.autoExtract = cycleValue(k.autoExtract, autoExtractValues)
		return m, m.updateConventionCmd(k.tagKey, k.scope, k.exclusionGroup, k.autoExtract, k.interview)
	}
	return m, nil
}

func (m *EditorModel) handleCol1Key(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k":
		if m.valCursor > 0 {
			m.valCursor--
			if m.valCursor < len(m.values) {
				m.selectedValue = m.values[m.valCursor]
				return m, loadDetail(m.st, m.selectedKey.tagKey, m.selectedValue.value)
			}
		}
	case "down", "j":
		if m.valCursor < len(m.values)-1 {
			m.valCursor++
			m.selectedValue = m.values[m.valCursor]
			return m, loadDetail(m.st, m.selectedKey.tagKey, m.selectedValue.value)
		}

	case "a":
		if m.currentKeyIsLifecycle() {
			m.setReadOnlyMsg()
			return m, nil
		}
		if m.selectedKey.tagKey == "" {
			return m, nil
		}
		modal := newModal(modalAddValue)
		modal.title = "Add Value to: " + m.selectedKey.tagKey
		m.modal = modal
		return m, nil

	case "d":
		if m.currentKeyIsLifecycle() {
			m.setReadOnlyMsg()
			return m, nil
		}
		if m.selectedValue.value == "" {
			return m, nil
		}
		return m, m.retireValueCmd()

	case "e":
		// Description editing is allowed for lifecycle VALUES (per-value): a
		// description has no engine coupling. Add/retire stay blocked above.
		if m.selectedValue.value == "" {
			return m, nil
		}
		modal := newModal(modalEditDesc)
		modal.title = "Edit Description: " + m.selectedValue.value
		modal.input.SetValue(m.detail.description)
		modal.editTarget = 1
		m.modal = modal
		return m, nil

	case "r":
		m.showRetired = !m.showRetired
		return m, m.loadValuesCmd()
	}
	return m, nil
}

func (m *EditorModel) handleModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if m.modal.mode == modalHelp || m.modal.mode == modalReadOnly {
		m.modal = nil
		return m, nil
	}

	switch key {
	case "esc":
		m.modal = nil
		return m, nil

	case "enter":
		return m.submitModal()

	case "tab":
		if m.modal.mode == modalAddKey {
			m.modal.fieldIdx = (m.modal.fieldIdx + 1) % 3
			m.modal.input.Blur()
			m.modal.input2.Blur()
			m.modal.input3.Blur()
			switch m.modal.fieldIdx {
			case 0:
				m.modal.input.Focus()
			case 1:
				m.modal.input2.Focus()
			case 2:
				m.modal.input3.Focus()
			}
		}
		return m, nil

	default:
		var cmd tea.Cmd
		switch m.modal.fieldIdx {
		case 0:
			m.modal.input, cmd = m.modal.input.Update(msg)
		case 1:
			m.modal.input2, cmd = m.modal.input2.Update(msg)
		case 2:
			m.modal.input3, cmd = m.modal.input3.Update(msg)
		}
		return m, cmd
	}
}

func (m *EditorModel) submitModal() (tea.Model, tea.Cmd) {
	switch m.modal.mode {
	case modalAddKey:
		tagKey := strings.TrimSpace(m.modal.input.Value())
		category := strings.TrimSpace(m.modal.input2.Value())
		cardinality := strings.TrimSpace(m.modal.input3.Value())
		if tagKey == "" {
			m.modal.errMsg = "key name is required"
			return m, nil
		}
		if category == "" {
			category = "context"
		}
		if cardinality == "" {
			cardinality = "single"
		}
		if category != "context" && category != "lifecycle" {
			m.modal.errMsg = "category must be 'context' or 'lifecycle'"
			return m, nil
		}
		if cardinality != "single" && cardinality != "multi" {
			m.modal.errMsg = "cardinality must be 'single' or 'multi'"
			return m, nil
		}
		m.modal = nil
		return m, m.addKeyCmd(tagKey, category, cardinality)

	case modalAddValue:
		val := strings.TrimSpace(m.modal.input.Value())
		if val == "" {
			m.modal.errMsg = "value is required"
			return m, nil
		}
		m.modal = nil
		return m, m.addValueCmd(val)

	case modalEditDesc:
		desc := strings.TrimSpace(m.modal.input.Value())
		editTarget := m.modal.editTarget
		m.modal = nil
		if editTarget == 2 {
			k := m.selectedKey
			return m, m.updateConventionCmd(k.tagKey, k.scope, desc, k.autoExtract, k.interview)
		}
		if editTarget == 1 {
			return m, m.editValueDescCmd(desc)
		}
		return m, m.editDescCmd(desc)
	}
	m.modal = nil
	return m, nil
}

func (m *EditorModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "esc" {
		m.filterMode = false
		m.filterVal = ""
		m.filterInput.Reset()
		m.rebuildVisKeys()
		return m, nil
	}
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	m.filterVal = m.filterInput.Value()
	m.rebuildVisKeys()
	return m, cmd
}

// --- DB commands ------------------------------------------------------------

func (m *EditorModel) addKeyCmd(tagKey, category, cardinality string) tea.Cmd {
	st := m.st
	return func() tea.Msg {
		err := st.DefineTag(context.Background(), wms.TagSpec{
			Key:         tagKey,
			Category:    category,
			Cardinality: cardinality,
		})
		return dbWriteDone{err: err}
	}
}

func (m *EditorModel) addValueCmd(value string) tea.Cmd {
	st := m.st
	tagKey := m.selectedKey.tagKey
	return func() tea.Msg {
		err := st.AddTagValue(context.Background(), tagKey, value, "")
		return dbWriteDone{err: err}
	}
}

func (m *EditorModel) editDescCmd(desc string) tea.Cmd {
	st := m.st
	tagKey := m.selectedKey.tagKey
	return func() tea.Msg {
		err := st.UpdateTagDescription(context.Background(), tagKey, desc)
		return dbWriteDone{err: err}
	}
}

func (m *EditorModel) retireKeyCmd() tea.Cmd {
	st := m.st
	tagKey := m.selectedKey.tagKey
	return func() tea.Msg {
		err := st.RetireTag(context.Background(), tagKey)
		return dbWriteDone{err: err}
	}
}

// toggleRequiredCmd flips the per-key required flag across all of the key's
// value rows. Equivalent to store.DefineTag with Required set — required is a
// per-key property like cardinality, so it is written to every row of the key.
func (m *EditorModel) toggleRequiredCmd(tagKey string, required bool) tea.Cmd {
	st := m.st
	return func() tea.Msg {
		err := st.SetTagRequired(context.Background(), tagKey, required)
		return dbWriteDone{err: err}
	}
}

func (m *EditorModel) retireValueCmd() tea.Cmd {
	st := m.st
	tagKey := m.selectedKey.tagKey
	tagValue := m.selectedValue.value
	return func() tea.Msg {
		err := st.RetireTagValue(context.Background(), tagKey, tagValue)
		return dbWriteDone{err: err}
	}
}

func (m *EditorModel) editValueDescCmd(desc string) tea.Cmd {
	st := m.st
	tagKey := m.selectedKey.tagKey
	tagValue := m.selectedValue.value
	return func() tea.Msg {
		err := st.UpdateTagValueDescription(context.Background(), tagKey, tagValue, desc)
		return dbWriteDone{err: err}
	}
}

func (m *EditorModel) loadValuesCmd() tea.Cmd {
	if m.selectedKey.tagKey == "" {
		return nil
	}
	return loadValues(m.st, m.selectedKey.tagKey, m.showRetired)
}

// --- Helpers ----------------------------------------------------------------

// rebuildVisKeys flattens m.groups into the navigable visible key list,
// applying filterVal if set.
func (m *EditorModel) rebuildVisKeys() {
	filter := strings.ToLower(m.filterVal)
	var vks []visibleKey

	for gi := range m.groups {
		g := &m.groups[gi]

		// Mode gate: context mode hides lifecycle keys; lifecycle mode hides
		// everything else. The two families are never visible together.
		isLifecycle := g.namespace == "lifecycle"
		if (m.mode == modeContext && isLifecycle) || (m.mode == modeLifecycle && !isLifecycle) {
			continue
		}

		// Section header (non-selectable)
		if g.isSection {
			vks = append(vks, visibleKey{
				group:    g,
				isHeader: true,
				label:    g.label,
			})
		}

		if g.namespace != "" && !g.isSection {
			// Collapsible integration group header.
			prefix := "▸ "
			if !g.collapsed {
				prefix = "▾ "
			}
			vks = append(vks, visibleKey{
				group:     g,
				isGroupHd: true,
				label:     prefix + g.namespace,
			})
		}

		if g.collapsed && g.namespace != "" && !g.isSection {
			continue
		}

		for ki := range g.keys {
			k := &g.keys[ki]
			indent := "  "
			displayKey := k.tagKey
			if g.namespace != "" && !g.isSection {
				indent = "    "
				displayKey = "." + strings.TrimPrefix(k.tagKey, g.namespace+".")
			}
			if filter != "" && !strings.Contains(strings.ToLower(k.tagKey), filter) {
				continue
			}
			vks = append(vks, visibleKey{
				entry:  k,
				group:  g,
				indent: indent,
				label:  indent + displayKey,
			})
		}
	}

	// Clamp cursor.
	if m.keyCursor >= len(vks) {
		m.keyCursor = len(vks) - 1
	}
	if m.keyCursor < 0 {
		m.keyCursor = 0
	}
	m.visKeys = vks
}

func (m *EditorModel) syncSelectedKey() {
	vks := m.visKeys
	if len(vks) == 0 {
		m.selectedKey = keyEntry{}
		return
	}
	vk := vks[m.keyCursor]
	if vk.entry != nil {
		m.selectedKey = *vk.entry
	}
}

// toggleMode flips between context and lifecycle editing. The two key families
// are never shown together: switching rebuilds the visible list for the new
// mode, parks the cursor on its first real key, and reloads that key's values.
func (m *EditorModel) toggleMode() (tea.Model, tea.Cmd) {
	if m.mode == modeContext {
		m.mode = modeLifecycle
		m.statusMsg = "lifecycle tags -- engine-managed"
	} else {
		m.mode = modeContext
		m.statusMsg = "context tags"
	}
	m.statusTimer = 30
	m.filterVal = ""
	m.filterInput.Reset()
	m.keyCursor = 0
	m.focusCol = 0
	m.rebuildVisKeys()
	m.cursorToFirstKey()
	m.syncSelectedKey()
	return m, m.loadValuesCmd()
}

// cursorToFirstKey advances keyCursor past leading section headers to the first
// selectable key, so a freshly-built list lands on a real key rather than a
// non-selectable header.
func (m *EditorModel) cursorToFirstKey() {
	for m.keyCursor < len(m.visKeys)-1 && m.visKeys[m.keyCursor].isHeader {
		m.keyCursor++
	}
}

func (m *EditorModel) findGroupHeaderCursor(g *keyGroup) {
	for i, vk := range m.visKeys {
		if vk.isGroupHd && vk.group == g {
			m.keyCursor = i
			return
		}
	}
}

func (m *EditorModel) currentKeyIsLifecycle() bool {
	vks := m.visKeys
	if len(vks) == 0 || m.keyCursor >= len(vks) {
		return false
	}
	vk := vks[m.keyCursor]
	if vk.entry != nil {
		return vk.entry.category == "lifecycle"
	}
	// if we're on a section header for lifecycle, also treat as read-only
	if vk.isHeader && vk.group != nil && vk.group.namespace == "lifecycle" {
		return true
	}
	return false
}

func (m *EditorModel) setReadOnlyMsg() {
	m.statusMsg = "read only -- engine-managed key"
	m.statusTimer = 30
}

func (m *EditorModel) totalStats() (keys, values, entities int) {
	for _, g := range m.groups {
		for _, k := range g.keys {
			keys++
			entities += k.entityCount
		}
	}
	values = len(m.values)
	return
}

// --- View -------------------------------------------------------------------

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

	content := m.renderEditor()

	if m.showHelp {
		return m.renderWithHelp(content)
	}
	if m.modal != nil {
		return m.renderWithModal(content)
	}
	return content
}

func (m EditorModel) renderEditor() string {
	totalKeys, _, totalEntities := m.totalStats()
	headerLine := fmt.Sprintf("Tag Vocabulary Editor%s%d keys  %d entities",
		strings.Repeat(" ", max(1, m.width-50)), totalKeys, totalEntities)
	modeLabel := "Context tags"
	if m.mode == modeLifecycle {
		modeLabel = "Lifecycle tags -- engine-managed"
	}
	header := BoldAccentStyle.Render("Tag Vocabulary Editor") +
		AccentStyle.Render("  ["+modeLabel+"]") +
		DimStyle.Render(fmt.Sprintf("%s%d keys  %d entities",
			strings.Repeat(" ", max(1, m.width-22-lipgloss.Width(modeLabel)-4-countDigits(totalKeys)-countDigits(totalEntities)-17)),
			totalKeys, totalEntities))
	_ = headerLine

	rule := DimStyle.Render(strings.Repeat("─", m.width))
	statusBar := m.renderStatusBar()

	// Body height = total - header(1) - rule(1) - statusBar(1) - padding(2 for border).
	bodyH := m.height - 5

	col1W, col2W, col3W := m.columnWidths()

	col1 := m.renderCol1(col1W, bodyH)
	col2 := m.renderCol2(col2W, bodyH)
	col3 := m.renderCol3(col3W, bodyH)

	columns := lipgloss.JoinHorizontal(lipgloss.Top, col1, col2, col3)

	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		rule,
		columns,
		statusBar,
	)
}

func (m EditorModel) columnWidths() (col1, col2, col3 int) {
	w := m.width
	// borders: each panel has 2 border chars; 3 panels = 6 total
	inner := w - 6
	if inner < 10 {
		inner = 10
	}
	if w >= 120 {
		col1 = 22
		col2 = inner/2 - col1/2
		col3 = inner - col1 - col2
	} else {
		col1 = 16
		col2 = 30
		col3 = inner - col1 - col2
	}
	if col3 < 10 {
		col3 = 10
	}
	return
}

func (m EditorModel) renderCol1(w, h int) string {
	var lines []string

	if m.filterMode && m.focusCol == 0 {
		filterLine := "/" + m.filterInput.View()
		lines = append(lines, AccentStyle.Render(filterLine))
		h--
	}

	vks := m.visKeys

	// Empty mode (defensive — both context and lifecycle are always seeded).
	if len(vks) == 0 {
		msg := "(no context keys -- run setup)"
		if m.mode == modeLifecycle {
			msg = "(no lifecycle keys)"
		}
		lines = append(lines, DimStyle.Render(msg))
		for len(lines) < h {
			lines = append(lines, "")
		}
		borderStyle := UnfocusedBorder
		if m.focusCol == 0 {
			borderStyle = FocusedBorder
		}
		return borderStyle.Width(w).Height(h).Render(strings.Join(lines, "\n"))
	}

	// Simple windowing: show a window of h items centered on cursor.
	start := 0
	if len(vks) > h {
		start = m.keyCursor - h/2
		if start < 0 {
			start = 0
		}
		if start+h > len(vks) {
			start = len(vks) - h
		}
	}

	for i := start; i < len(vks) && len(lines) < h; i++ {
		vk := vks[i]
		isCursor := i == m.keyCursor && m.focusCol == 0

		var line string
		switch {
		case vk.isHeader:
			line = BoldAccentStyle.Render(vk.label)
		case vk.isGroupHd:
			if isCursor {
				line = SelectedStyle.Render(truncate(vk.label, w))
			} else {
				line = AccentStyle.Render(truncate(vk.label, w))
			}
		default:
			entry := vk.entry
			countStr := ""
			if entry != nil && entry.entityCount > 0 {
				countStr = DimStyle.Render(fmt.Sprintf(" %d", entry.entityCount))
			}
			reqStr := ""
			if entry != nil && entry.required {
				reqStr = ReqStyle.Render(" REQ")
			}
			keyStr := truncate(vk.label, w-lipgloss.Width(countStr)-lipgloss.Width(reqStr)-1)
			var styledKey string
			if isCursor {
				styledKey = SelectedStyle.Render(keyStr)
			} else {
				styledKey = TextStyle.Render(keyStr)
			}
			line = styledKey + reqStr + countStr
		}
		lines = append(lines, line)
	}

	// Pad to height.
	for len(lines) < h {
		lines = append(lines, "")
	}

	content := strings.Join(lines, "\n")
	borderStyle := UnfocusedBorder
	if m.focusCol == 0 {
		borderStyle = FocusedBorder
	}
	return borderStyle.Width(w).Height(h).Render(content)
}

func (m EditorModel) renderCol2(w, h int) string {
	var lines []string

	if m.selectedKey.tagKey == "" {
		lines = append(lines, DimStyle.Render("(select a key)"))
	} else {
		header := BoldAccentStyle.Render("Values for: " + m.selectedKey.tagKey)
		lines = append(lines, header)
		lines = append(lines, "")
	}

	if m.filterMode && m.focusCol == 1 {
		filterLine := "/" + m.filterInput.View()
		lines = append(lines, AccentStyle.Render(filterLine))
		h -= len(lines)
	}

	if len(m.values) == 0 && m.selectedKey.tagKey != "" {
		lines = append(lines, DimStyle.Render("(no values -- press 'a' to add)"))
	}

	for vi, v := range m.values {
		isCursor := vi == m.valCursor && m.focusCol == 1
		displayVal := v.value
		if displayVal == "" {
			displayVal = "(stub)"
		}
		suffix := ""
		if v.retired {
			suffix = DimStyle.Render(" [retired]")
		}
		countStr := ""
		if v.entityCount > 0 {
			countStr = DimStyle.Render(fmt.Sprintf("  (%d)", v.entityCount))
		}
		valStr := truncate(displayVal, w-lipgloss.Width(countStr)-lipgloss.Width(suffix)-3)
		var line string
		if isCursor {
			line = SelectedStyle.Render("▸ "+valStr) + suffix + countStr
		} else if v.retired {
			line = DimStyle.Render("  "+valStr) + suffix + countStr
		} else {
			line = TextStyle.Render("  "+valStr) + countStr
		}
		lines = append(lines, line)
	}

	for len(lines) < h {
		lines = append(lines, "")
	}

	content := strings.Join(lines[:min(len(lines), h)], "\n")
	borderStyle := UnfocusedBorder
	if m.focusCol == 1 {
		borderStyle = FocusedBorder
	}
	return borderStyle.Width(w).Height(h).Render(content)
}

func (m EditorModel) renderCol3(w, h int) string {
	var lines []string

	if m.detailErr != "" {
		lines = append(lines, ErrorStyle.Render("error: "+m.detailErr))
	} else if m.selectedKey.tagKey == "" {
		lines = append(lines, DimStyle.Render("(select a key)"))
	} else if m.selectedValue.value == "" && len(m.values) == 0 {
		// key selected but no value
		lines = append(lines, BoldAccentStyle.Render("Key: "+m.selectedKey.tagKey))
		lines = append(lines, "")
		lines = append(lines, TextStyle.Render("Category: "+m.selectedKey.category))
		lines = append(lines, TextStyle.Render("Cardinality: "+m.selectedKey.cardinality))
		required := "no"
		if m.selectedKey.required {
			required = "yes"
		}
		lines = append(lines, TextStyle.Render("Required: "+required))
		lines = append(lines, "")
		lines = append(lines, m.renderConventionFields(w)...)
		if m.selectedKey.description != "" {
			lines = append(lines, "")
			lines = append(lines, TextStyle.Render("Description:"))
			for _, dl := range wordWrap(m.selectedKey.description, w-2) {
				lines = append(lines, TextStyle.Render("  "+dl))
			}
		}
	} else {
		// value selected
		d := m.detail
		val := d.value
		if val == "" {
			val = "(stub)"
		}
		lines = append(lines, BoldAccentStyle.Render("value: "+val))
		lines = append(lines, "")
		lines = append(lines, TextStyle.Render(fmt.Sprintf("entities: %d", d.entityCount)))
		seed := "no"
		if d.isSeed {
			seed = "yes"
		}
		lines = append(lines, TextStyle.Render("seed: "+seed))
		lines = append(lines, "")
		lines = append(lines, m.renderConventionFields(w)...)
		if d.description != "" {
			lines = append(lines, "")
			lines = append(lines, TextStyle.Render("Description:"))
			for _, dl := range wordWrap(d.description, w-2) {
				lines = append(lines, TextStyle.Render("  "+dl))
			}
		}
		if len(d.entities) > 0 {
			lines = append(lines, "")
			lines = append(lines, DimStyle.Render("Used by:"))
			for _, e := range d.entities {
				title := truncate(e.title, w-4)
				lines = append(lines, DimStyle.Render("  "+title))
			}
		} else if d.entityCount == 0 {
			lines = append(lines, "")
			lines = append(lines, DimStyle.Render("(not applied to any entities)"))
		}
	}

	for len(lines) < h {
		lines = append(lines, "")
	}

	content := strings.Join(lines[:min(len(lines), h)], "\n")
	borderStyle := UnfocusedBorder
	if m.focusCol == 2 {
		borderStyle = FocusedBorder
	}
	return borderStyle.Width(w).Height(h).Render(content)
}

func (m EditorModel) renderStatusBar() string {
	if m.statusMsg != "" {
		return StatusBarStyle.Render(m.statusMsg)
	}
	return StatusBarStyle.Render("Tab: col  ↑↓ nav  a add  d retire  e edit  t req  s scope  i interview  x excl  X extract  L mode  ?: help  q quit")
}

func (m EditorModel) renderWithHelp(content string) string {
	helpContent := `Navigation
  Tab          switch between columns
  ↑ ↓          move selection up/down
  ← →          collapse/expand group (column 1)
  Enter        select / expand group
  Esc          close this help / go back

Actions
  a            add new key (col 1) or value (col 2)
  d            retire key (col 1) or value (col 2)
  r            toggle show/hide retired values (col 2)
  e            edit description
  t            toggle required flag (col 1 key)
  L            switch Context <-> Lifecycle keys
  /            filter keys by name
  ?            this help
  q            quit editor

Conventions (col 1 — per-key)
  s            cycle scope: (any) → outcome → workunit
  i            cycle interview: propose → auto → skip
  x            edit exclusion group (free text slug)
  X            cycle auto-extract: (none) → git → env

Conventions are advisory metadata on each tag key.
scope: where the key should be applied.
interview: how the key behaves in the tag interview.
exclusion_group: keys sharing a group are mutually
exclusive. auto_extract: source for auto-extraction.

                            Press Esc to close`

	overlay := UnfocusedBorder.
		Width(60).
		Padding(1, 2).
		Render(BoldAccentStyle.Render("Help\n\n") + DimStyle.Render(helpContent))

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		overlay,
		lipgloss.WithWhitespaceChars(" "),
	)
}

func (m EditorModel) renderWithModal(content string) string {
	modal := m.modal
	var body strings.Builder

	body.WriteString(BoldAccentStyle.Render(modal.title) + "\n\n")

	switch modal.mode {
	case modalAddKey:
		fieldFocus := func(idx int) string {
			if modal.fieldIdx == idx {
				return AccentStyle.Render("> ")
			}
			return "  "
		}
		body.WriteString(fieldFocus(0) + "Key name:    " + modal.input.View() + "\n")
		body.WriteString(fieldFocus(1) + "Category:    " + modal.input2.View() + "\n")
		body.WriteString(fieldFocus(2) + "Cardinality: " + modal.input3.View() + "\n")
	case modalAddValue:
		body.WriteString("Value: " + modal.input.View() + "\n")
	case modalEditDesc:
		body.WriteString("Description: " + modal.input.View() + "\n")
	}

	if modal.errMsg != "" {
		body.WriteString("\n" + ErrorStyle.Render(modal.errMsg))
	}

	body.WriteString("\n" + DimStyle.Render("Enter: confirm  Esc: cancel  Tab: next field"))

	overlay := FocusedBorder.
		Width(50).
		Padding(1, 2).
		Render(body.String())

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		overlay,
		lipgloss.WithWhitespaceChars(" "),
	)
}

// --- Convention editing ------------------------------------------------------

func (m EditorModel) renderConventionFields(w int) []string {
	k := m.selectedKey
	var lines []string
	lines = append(lines, DimStyle.Render("Conventions:"))
	scopeVal := k.scope
	if scopeVal == "" {
		scopeVal = "(any)"
	}
	lines = append(lines, TextStyle.Render(fmt.Sprintf("  Scope:       %s", scopeVal)))
	interviewVal := k.interview
	if interviewVal == "" {
		interviewVal = "propose"
	}
	lines = append(lines, TextStyle.Render(fmt.Sprintf("  Interview:   %s", interviewVal)))
	exclVal := k.exclusionGroup
	if exclVal == "" {
		exclVal = "(none)"
	}
	lines = append(lines, TextStyle.Render(fmt.Sprintf("  Excl. group: %s", exclVal)))
	extractVal := k.autoExtract
	if extractVal == "" {
		extractVal = "(none)"
	}
	lines = append(lines, TextStyle.Render(fmt.Sprintf("  Auto-extract: %s", extractVal)))
	return lines
}

var scopeValues = []string{"", "outcome", "workunit"}
var interviewValues = []string{"propose", "auto", "skip"}
var autoExtractValues = []string{"", "git", "env"}

func cycleValue(current string, options []string) string {
	for i, v := range options {
		if v == current {
			return options[(i+1)%len(options)]
		}
	}
	return options[0]
}

func (m *EditorModel) updateConventionCmd(tagKey, scope, exclusionGroup, autoExtract, interview string) tea.Cmd {
	st := m.st
	return func() tea.Msg {
		err := st.UpdateTagConventions(context.Background(), tagKey, scope, exclusionGroup, autoExtract, interview)
		return dbWriteDone{err: err}
	}
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

func countDigits(n int) int {
	if n == 0 {
		return 1
	}
	count := 0
	for n > 0 {
		n /= 10
		count++
	}
	return count
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

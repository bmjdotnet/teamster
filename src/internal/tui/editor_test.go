package tui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

type fakeKey struct {
	summary wms.TagKeySummary
	rows    []wms.Tag
}

type fakeTagStore struct {
	keys   []string // order
	data   map[string]*fakeKey
	counts []store.TagCountRow
	calls  []string
}

func (f *fakeTagStore) add(sum wms.TagKeySummary, rows ...wms.Tag) {
	for i := range rows {
		rows[i].Key = sum.Key
	}
	f.keys = append(f.keys, sum.Key)
	f.data[sum.Key] = &fakeKey{summary: sum, rows: rows}
}

func newFakeStore() *fakeTagStore {
	f := &fakeTagStore{data: map[string]*fakeKey{}}
	f.add(wms.TagKeySummary{Key: "priority", Category: "context", Cardinality: "single", Required: true,
		Description: "Urgency: p0=critical."},
		wms.Tag{Value: "p0", Description: "Critical drop-everything", IsSeed: true},
		wms.Tag{Value: "p1", Description: "High"},
		wms.Tag{Value: "p2", Description: "Normal"},
		wms.Tag{Value: "old", Retired: true})
	f.add(wms.TagKeySummary{Key: "product", Category: "context", Cardinality: "single"},
		wms.Tag{Value: "teamster"})
	f.add(wms.TagKeySummary{Key: "github.pr", Category: "context", Cardinality: "single", Description: "PR number"},
		wms.Tag{Value: ""})
	f.add(wms.TagKeySummary{Key: "phase", Category: "lifecycle", Cardinality: "single", Description: "Lifecycle phase"},
		wms.Tag{Value: "build"})
	f.counts = []store.TagCountRow{
		{EntityType: "outcome", TagKey: "priority", TagValue: "p0", Count: 3},
		{EntityType: "workunit", TagKey: "priority", TagValue: "p1", Count: 6},
	}
	return f
}

func (f *fakeTagStore) rec(s string) { f.calls = append(f.calls, s) }

func (f *fakeTagStore) TagKeys(context.Context) ([]wms.TagKeySummary, error) {
	var out []wms.TagKeySummary
	for _, k := range f.keys {
		out = append(out, f.data[k].summary)
	}
	return out, nil
}

func (f *fakeTagStore) ListTags(context.Context) ([]wms.Tag, error) {
	var out []wms.Tag
	for _, k := range f.keys {
		out = append(out, f.data[k].rows...)
	}
	return out, nil
}

func (f *fakeTagStore) TagEntityCounts(context.Context) ([]store.TagCountRow, error) {
	return f.counts, nil
}

func (f *fakeTagStore) TagValueDetail(_ context.Context, key, value string) (store.TagValueDetail, error) {
	return store.TagValueDetail{Key: key, Value: value, EntityCount: 1,
		BoundEntities: []wms.EntityRef{
			{EntityType: "outcome", EntityID: "o1", Why: "Some outcome"},
			{EntityType: "workunit", EntityID: "w1", Why: "Some unit"},
		}}, nil
}

func (f *fakeTagStore) SetTagRequired(_ context.Context, key string, req bool) error {
	f.rec(fmt.Sprintf("SetTagRequired(%s,%v)", key, req))
	f.data[key].summary.Required = req
	return nil
}

func (f *fakeTagStore) UpdateTagDescription(_ context.Context, key, d string) error {
	f.rec(fmt.Sprintf("UpdateTagDescription(%s,%s)", key, d))
	f.data[key].summary.Description = d
	return nil
}

func (f *fakeTagStore) UpdateTagConventions(_ context.Context, key, scope, excl, auto, interview string) error {
	f.rec(fmt.Sprintf("UpdateTagConventions(%s,%s,%s,%s,%s)", key, scope, excl, auto, interview))
	return nil
}

func (f *fakeTagStore) UpdateTagValueDescription(_ context.Context, key, val, d string) error {
	f.rec(fmt.Sprintf("UpdateTagValueDescription(%s,%s,%s)", key, val, d))
	return nil
}

func (f *fakeTagStore) AddTagValue(_ context.Context, key, val, d string) error {
	f.rec(fmt.Sprintf("AddTagValue(%s,%s,%s)", key, val, d))
	f.data[key].rows = append(f.data[key].rows, wms.Tag{Key: key, Value: val, Description: d})
	return nil
}

func (f *fakeTagStore) DefineTag(_ context.Context, spec wms.TagSpec) error {
	f.rec(fmt.Sprintf("DefineTag(%s,%s,%s,%s,req=%v)", spec.Key, spec.Category, spec.Cardinality, spec.Description, *spec.Required))
	return nil
}

func (f *fakeTagStore) RetireTag(_ context.Context, key string) error {
	f.rec("RetireTag(" + key + ")")
	return nil
}

func (f *fakeTagStore) RetireTagValue(_ context.Context, key, val string) error {
	f.rec(fmt.Sprintf("RetireTagValue(%s,%s)", key, val))
	return nil
}

func (f *fakeTagStore) UnretireTagValue(_ context.Context, key, val string) error {
	f.rec(fmt.Sprintf("UnretireTagValue(%s,%s)", key, val))
	return nil
}

func (f *fakeTagStore) TagValues(context.Context, string) ([]wms.Tag, error)   { return nil, nil }
func (f *fakeTagStore) TagBindingCount(context.Context, string) (int64, error) { return 0, nil }
func (f *fakeTagStore) DeleteTagKey(context.Context, string) (int64, error)    { return 0, nil }
func (f *fakeTagStore) DeleteTagValue(context.Context, string, string) error   { return nil }
func (f *fakeTagStore) SeedTags(context.Context, []wms.TagSpec) error          { return nil }
func (f *fakeTagStore) SeedProductValues(context.Context, []string) error      { return nil }
func (f *fakeTagStore) SeedIntegrationKeys(context.Context, []store.IntegrationKey) error {
	return nil
}

// runCmd executes a command, flattening batches. Commands that block (status
// tick timers) are abandoned after a short wait.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		if b, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range b {
				out = append(out, runCmd(c)...)
			}
			return out
		}
		return []tea.Msg{msg}
	case <-time.After(150 * time.Millisecond):
		return nil
	}
}

// feed delivers msg and every message the resulting commands produce.
func feed(m EditorModel, msg tea.Msg) EditorModel {
	queue := []tea.Msg{msg}
	for len(queue) > 0 && len(queue) < 50 {
		next := queue[0]
		queue = queue[1:]
		mod, cmd := m.Update(next)
		m = mod.(EditorModel)
		queue = append(queue, runCmd(cmd)...)
	}
	return m
}

func press(m EditorModel, keys ...string) EditorModel {
	for _, k := range keys {
		var km tea.KeyMsg
		switch k {
		case "enter":
			km = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			km = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			km = tea.KeyMsg{Type: tea.KeyTab}
		case "down":
			km = tea.KeyMsg{Type: tea.KeyDown}
		case "up":
			km = tea.KeyMsg{Type: tea.KeyUp}
		case "left":
			km = tea.KeyMsg{Type: tea.KeyLeft}
		case "right":
			km = tea.KeyMsg{Type: tea.KeyRight}
		case "ctrl+s":
			km = tea.KeyMsg{Type: tea.KeyCtrlS}
		case " ":
			km = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		default:
			km = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		m = feed(m, km)
	}
	return m
}

func newLoaded(t *testing.T, fs *fakeTagStore) EditorModel {
	t.Helper()
	m := NewEditor(fs)
	m = feed(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = feed(m, m.Init()())
	if m.err != "" || m.loading {
		t.Fatalf("load failed: err=%q loading=%v", m.err, m.loading)
	}
	return m
}

func moveTo(t *testing.T, m EditorModel, id string) EditorModel {
	t.Helper()
	for i := 0; i < 60 && m.currentID() != id; i++ {
		m = press(m, "down")
	}
	if m.currentID() != id {
		t.Fatalf("could not reach %s; visible=%v", id, visibleIDs(m))
	}
	return m
}

func visibleIDs(m EditorModel) []string {
	var ids []string
	for _, n := range m.visible {
		ids = append(ids, n.id)
	}
	return ids
}

func TestLoadBuildsTreeAndHidesStubs(t *testing.T) {
	m := newLoaded(t, newFakeStore())

	if len(m.groups) != 3 || m.groups[0].id != "g:context" || m.groups[1].id != "g:github" || m.groups[2].id != "g:lifecycle" {
		t.Fatalf("unexpected groups: %+v", m.groups)
	}
	gh := m.keys["github.pr"]
	if gh == nil || !gh.createOnApply || len(gh.values) != 0 {
		t.Fatalf("stub-only key must be create-on-apply with no value nodes: %+v", gh)
	}
	pr := m.keys["priority"]
	if pr.valueCount != 3 || pr.retiredCount != 1 || pr.entityCount != 9 {
		t.Fatalf("priority counts: %+v", pr)
	}
	if got := pr.values[0].value; got != "p1" {
		t.Fatalf("values should sort by entity count desc, got %s first", got)
	}
	for _, id := range visibleIDs(m) {
		if strings.HasPrefix(id, "v:") {
			t.Fatalf("keys start collapsed, saw value node %s", id)
		}
	}
	for _, id := range visibleIDs(m) {
		if id == "k:phase" {
			t.Fatalf("lifecycle group starts collapsed")
		}
	}
}

func TestKeyNodeShowsKeyDescriptionNotFirstValue(t *testing.T) {
	m := newLoaded(t, newFakeStore())
	m = moveTo(t, m, "k:priority")
	v := m.View()
	if !strings.Contains(v, "Key description") || !strings.Contains(v, "Urgency: p0=critical.") {
		t.Fatalf("key description missing:\n%s", v)
	}
	if strings.Contains(v, "Critical drop-everything") {
		t.Fatalf("key node leaked a value description:\n%s", v)
	}
}

func TestValueNodeShowsValueThenKeyDescription(t *testing.T) {
	m := newLoaded(t, newFakeStore())
	m = moveTo(t, m, "k:priority")
	m = press(m, "right")
	m = moveTo(t, m, "v:priority=p0")
	v := m.View()
	vi := strings.Index(v, "Value description")
	ki := strings.Index(v, "Key description")
	if vi < 0 || ki < 0 || vi > ki {
		t.Fatalf("expected Value description before Key description:\n%s", v)
	}
	if !strings.Contains(v, "Critical drop-everything") || !strings.Contains(v, "Urgency: p0=critical.") {
		t.Fatalf("both descriptions should render:\n%s", v)
	}
	if !strings.Contains(v, "Some outcome") {
		t.Fatalf("bound entity detail should load:\n%s", v)
	}
}

func TestToggleRequiredCallsStore(t *testing.T) {
	fs := newFakeStore()
	m := newLoaded(t, fs)
	m = moveTo(t, m, "k:priority")
	m = press(m, "t")
	if len(fs.calls) != 1 || fs.calls[0] != "SetTagRequired(priority,false)" {
		t.Fatalf("calls = %v", fs.calls)
	}
	if m.currentID() != "k:priority" || !strings.Contains(m.statusMsg, "required off: priority") {
		t.Fatalf("cursor=%s status=%q", m.currentID(), m.statusMsg)
	}
}

func TestLifecycleVocabularyBlocked(t *testing.T) {
	fs := newFakeStore()
	m := newLoaded(t, fs)
	m = moveTo(t, m, "g:lifecycle")
	m = press(m, "right")
	m = moveTo(t, m, "k:phase")

	m = press(m, "d")
	if m.form != nil || !strings.Contains(m.statusMsg, "engine-managed") {
		t.Fatalf("d must be blocked: form=%v status=%q", m.form, m.statusMsg)
	}
	m = press(m, "a")
	if m.form != nil {
		t.Fatalf("a must be blocked")
	}
	m = press(m, "e")
	if m.form == nil || m.form.kind != formEditKey {
		t.Fatalf("e must open the key form on a lifecycle key")
	}
	if len(fs.calls) != 0 {
		t.Fatalf("no store writes expected, got %v", fs.calls)
	}
}

func TestFilterForceExpandsKeyForMatchingValue(t *testing.T) {
	m := newLoaded(t, newFakeStore())
	m = press(m, "/")
	for _, r := range "p1" {
		m = press(m, string(r))
	}
	ids := visibleIDs(m)
	want := []string{"g:context", "k:priority", "v:priority=p1"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("visible = %v, want %v", ids, want)
	}
	if m.expanded["k:priority"] {
		t.Fatalf("filter must not modify stored expansion")
	}
	m = press(m, "esc")
	if m.filter != "" || m.filterActive {
		t.Fatalf("esc should clear the filter")
	}
	for _, id := range visibleIDs(m) {
		if id == "v:priority=p1" {
			t.Fatalf("priority should be collapsed again")
		}
	}
}

func TestWriteDoneRestoresCursorBySelectID(t *testing.T) {
	fs := newFakeStore()
	m := newLoaded(t, fs)
	m = moveTo(t, m, "k:product")
	m = feed(m, writeDone{status: "added teamster2", selectID: "v:priority=p2", expand: []string{"k:priority"}})
	if m.currentID() != "v:priority=p2" {
		t.Fatalf("cursor = %s, want v:priority=p2", m.currentID())
	}
	m = feed(m, writeDone{status: "x"})
	if m.currentID() != "v:priority=p2" {
		t.Fatalf("with no selectID the cursor should stay, got %s", m.currentID())
	}
}

func keyCtrlS() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyCtrlS} }

var wordToken = regexp.MustCompile(`w\d{3}`)

func longDescription() string {
	var w []string
	for i := 0; i < 100; i++ {
		w = append(w, fmt.Sprintf("w%03d", i))
	}
	return strings.Join(w, " ")
}

func descriptionRows(view string) []string {
	var rows []string
	for _, l := range strings.Split(view, "\n") {
		if wordToken.MatchString(l) {
			rows = append(rows, l)
		}
	}
	return rows
}

func TestNoWrapAndNoCollapse(t *testing.T) {
	for _, tc := range []struct{ w, h, rows int }{{80, 30, 3}, {120, 50, 5}} {
		fs := newFakeStore()
		fs.data["priority"].summary.Description = longDescription()
		m := NewEditor(fs)
		m = feed(m, tea.WindowSizeMsg{Width: tc.w, Height: tc.h})
		m = feed(m, m.Init()())
		m = moveTo(t, m, "k:priority")
		m = press(m, "e")

		before := m.View()
		for i, l := range strings.Split(before, "\n") {
			if lipgloss.Width(l) > m.width {
				t.Fatalf("%dx%d line %d is %d wide: %q", tc.w, tc.h, i, lipgloss.Width(l), l)
			}
		}
		rows := descriptionRows(before)
		if len(rows) != tc.rows {
			t.Fatalf("%dx%d: description spans %d rows, want %d", tc.w, tc.h, len(rows), tc.rows)
		}

		m = press(m, "right")
		after := descriptionRows(m.View())
		if strings.Join(after, "\n") != strings.Join(rows, "\n") {
			t.Fatalf("%dx%d: description changed after a navigation key:\nbefore %q\nafter  %q", tc.w, tc.h, rows, after)
		}
	}
}

func TestLayoutScalesWithTerminal(t *testing.T) {
	m := newLoaded(t, newFakeStore())
	m = moveTo(t, m, "k:priority")
	m = press(m, "e")
	for _, tc := range []struct{ w, box, field int }{{80, 72, 51}, {100, 92, 71}, {120, 112, 91}, {200, 120, 99}} {
		m = feed(m, tea.WindowSizeMsg{Width: tc.w, Height: 40})
		if m.modalWidth() != tc.box || m.form.fieldW != tc.field {
			t.Errorf("width %d: modal %d fieldW %d, want %d / %d", tc.w, m.modalWidth(), m.form.fieldW, tc.box, tc.field)
		}
	}
}

func TestSuccessStatusPrefixed(t *testing.T) {
	m := newLoaded(t, newFakeStore())
	m = moveTo(t, m, "k:priority")
	m = press(m, "t")
	if !strings.Contains(m.statusMsg, "✓ required") || !strings.Contains(m.renderStatusBar(), "✓ required") {
		t.Fatalf("status = %q", m.statusMsg)
	}
}

func TestUsedByEntityTypeRendering(t *testing.T) {
	m := newLoaded(t, newFakeStore())
	m = moveTo(t, m, "k:priority")
	m = press(m, "right")
	m = moveTo(t, m, "v:priority=p0")
	v := m.View()
	o, w := strings.Index(v, "outcome  o1"), strings.Index(v, "workunit w1")
	if o < 0 || w < 0 || o > w {
		t.Fatalf("expected padded outcome then workunit rows:\n%s", v)
	}
}

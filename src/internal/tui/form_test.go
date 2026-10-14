package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestFormTabOrderSkipsLockedAndWraps(t *testing.T) {
	f := newEditValueForm(&valueEntry{key: &keyEntry{tagKey: "k"}, value: "v"})
	if f.focus != 0 {
		t.Fatalf("starts on description, got %d", f.focus)
	}
	var seen []int
	for i := 0; i < 4; i++ {
		f.move(1)
		seen = append(seen, f.focus)
	}
	want := []int{f.saveSlot(), f.discardSlot(), 0, f.saveSlot()}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("tab order = %v, want %v", seen, want)
		}
	}
	f.focus = 0
	f.move(-1)
	if f.focus != f.discardSlot() {
		t.Fatalf("shift+tab from first field should reach Discard, got %d", f.focus)
	}

	c := newConfirmRetireForm(&keyEntry{tagKey: "k"}, nil)
	if c.focus != c.discardSlot() {
		t.Fatalf("confirm form must start on Cancel")
	}
}

func TestEnumCyclesBothDirectionsAndDisplay(t *testing.T) {
	f := newEditKeyForm(&keyEntry{tagKey: "k", category: "context", cardinality: "single"})
	for f.fields[f.focus].key != "scope" {
		f.move(1)
	}
	fld := f.focused()
	if fld.enumLabel(fld.current()) != "(any)" {
		t.Fatalf("empty scope should display (any), got %q", fld.enumLabel(fld.current()))
	}
	fld.cycle(1)
	if fld.current() != "outcome" {
		t.Fatalf("forward = %q", fld.current())
	}
	fld.cycle(-1)
	fld.cycle(-1)
	if fld.current() != "workunit" {
		t.Fatalf("backward wrap = %q", fld.current())
	}
	f.layout(68, 24)
	if !strings.Contains(f.render(), "◂ workunit ▸") {
		t.Fatalf("focused enum should show chevrons")
	}
}

func TestFormDirtyFlag(t *testing.T) {
	f := newEditKeyForm(&keyEntry{tagKey: "k", category: "context", cardinality: "single", description: "d"})
	f.layout(68, 24)
	if f.dirty() || strings.Contains(f.render(), "Edit Key: k *") {
		t.Fatalf("fresh form must be clean")
	}
	f.fields[1].boolVal = true
	if !f.dirty() || !strings.Contains(f.render(), "Edit Key: k *") {
		t.Fatalf("changed form must be dirty and starred")
	}
}

func submitEditKey(t *testing.T, mutate func(f *formModel)) []string {
	t.Helper()
	fs := newFakeStore()
	m := newLoaded(t, fs)
	m = moveTo(t, m, "k:priority")
	m = press(m, "e")
	if m.form == nil {
		t.Fatalf("edit form did not open")
	}
	mutate(m.form)
	feed(m, keyCtrlS())
	return fs.calls
}

func TestEditKeySubmitDiff(t *testing.T) {
	if calls := submitEditKey(t, func(f *formModel) {}); len(calls) != 0 {
		t.Fatalf("unchanged form must make no store calls, got %v", calls)
	}

	calls := submitEditKey(t, func(f *formModel) {
		for i := range f.fields {
			if f.fields[i].key == "scope" {
				f.fields[i].cycle(2)
			}
		}
	})
	if len(calls) != 1 || calls[0] != "UpdateTagConventions(priority,workunit,,,)" {
		t.Fatalf("scope change must be one conventions call, got %v", calls)
	}

	calls = submitEditKey(t, func(f *formModel) {
		f.fields[0].area.SetValue("new rubric")
		f.fields[1].boolVal = false
	})
	want := []string{"UpdateTagDescription(priority,new rubric)", "SetTagRequired(priority,false)"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestAddKeyAndValueValidation(t *testing.T) {
	m := newLoaded(t, newFakeStore())
	f := newAddKeyForm("", "Context", m.keys)
	check := func(key, want string) {
		t.Helper()
		f.fields[0].input.SetValue(key)
		if got := f.validate(); got != want {
			t.Errorf("key %q: %q, want %q", key, got, want)
		}
	}
	check("", "key name is required")
	check("a b", "key name must not contain whitespace")
	check("priority", "key already exists")
	check("resolution", "system-managed key")
	check("fresh", "")

	vf := newAddValueForm(m.keys["priority"])
	vcheck := func(v, want string) {
		t.Helper()
		vf.fields[0].input.SetValue(v)
		if got := vf.validate(); got != want {
			t.Errorf("value %q: %q, want %q", v, got, want)
		}
	}
	vcheck("", "value is required")
	vcheck("p0", "value already exists")
	vcheck("old", "value exists (retired — press r to show)")
	vcheck("p9", "")
}

func TestTextFieldViewportMarkers(t *testing.T) {
	f := newAddValueForm(&keyEntry{tagKey: "k"})
	f.layout(68, 24)
	fld := &f.fields[0]
	inner := f.fieldW - 2
	fld.input.SetValue(strings.Repeat("x", 120))

	check := func(name string, wantLeft, wantRight bool) {
		t.Helper()
		fld.clampViewport(inner)
		line := renderTextField(fld, f.fieldW, true)
		if got := strings.Contains(line, "‹"); got != wantLeft {
			t.Errorf("%s: ‹ = %v, want %v", name, got, wantLeft)
		}
		if got := strings.Contains(line, "›"); got != wantRight {
			t.Errorf("%s: › = %v, want %v", name, got, wantRight)
		}
		if w := lipgloss.Width(line); w != f.fieldW {
			t.Errorf("%s: width %d, want %d", name, w, f.fieldW)
		}
	}
	check("end", true, false)
	fld.input.SetCursor(0)
	check("start", false, true)
	fld.input.SetCursor(60)
	check("middle", true, true)

	for _, l := range strings.Split(f.render(), "\n") {
		if lipgloss.Width(l) > f.inner {
			t.Errorf("line wider than inner (%d): %q", lipgloss.Width(l), l)
		}
	}
}

func TestHelpPanelFollowsFocus(t *testing.T) {
	f := newEditKeyForm(&keyEntry{tagKey: "k", category: "context", cardinality: "single"})
	f.layout(68, 24)
	expect := func(want string) {
		t.Helper()
		if out := f.render(); !strings.Contains(out, want) {
			t.Errorf("help panel missing %q:\n%s", want, out)
		}
	}
	expect("What this dimension is for")
	f.move(1)
	expect("every work unit must carry")
	f.focus = f.saveSlot()
	expect("Write only the fields you changed")
	f.focus = f.discardSlot()
	expect("Close without writing")

	c := newConfirmRetireForm(&keyEntry{tagKey: "k"}, nil)
	c.layout(68, 24)
	if !strings.Contains(c.render(), "Keep everything as it is") {
		t.Errorf("confirm form should show Cancel help first")
	}
}

func TestHelpPanelIsAlwaysTwoLines(t *testing.T) {
	k := &keyEntry{tagKey: "k", category: "context", cardinality: "single", values: []*valueEntry{}}
	v := &valueEntry{key: k, value: "v"}
	forms := map[string]*formModel{
		"editKey":     newEditKeyForm(k),
		"editValue":   newEditValueForm(v),
		"addKey":      newAddKeyForm("", "Context", map[string]*keyEntry{}),
		"addValue":    newAddValueForm(k),
		"retireKey":   newConfirmRetireForm(k, nil),
		"retireValue": newConfirmRetireForm(k, v),
	}
	for name, f := range forms {
		f.layout(68, 24)
		for slot := 0; slot < len(f.fields)+2; slot++ {
			if !f.slotFocusable(slot) {
				continue
			}
			f.focus = slot
			lines := strings.Split(f.render(), "\n")
			rule := -1
			for i, l := range lines {
				if strings.HasPrefix(l, "────") {
					rule = i
					break
				}
			}
			if rule < 0 || rule+4 >= len(lines) {
				t.Fatalf("%s slot %d: no rule line", name, slot)
			}
			if lines[rule+3] != "" || !strings.Contains(lines[rule+4], f.saveLabel) {
				t.Errorf("%s slot %d: help panel is not exactly two lines:\n%s", name, slot, strings.Join(lines[rule:rule+5], "\n"))
			}
		}
	}
}

func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func TestTextAreaUpDownLeavesAtBoundary(t *testing.T) {
	f := newEditKeyForm(&keyEntry{tagKey: "k", category: "context", cardinality: "single", description: "short"})
	f.layout(68, 24)
	f.handleKey(key(tea.KeyDown))
	if f.focus != 1 {
		t.Fatalf("down on a one-line description should move to the next field, focus=%d", f.focus)
	}

	f.focus = 0
	f.syncInputFocus()
	area := &f.fields[0].area
	area.SetValue("a\nb\nc")
	f.handleKey(key(tea.KeyUp))
	f.handleKey(key(tea.KeyUp))
	if f.focus != 0 || area.Line() != 0 {
		t.Fatalf("up inside the text must stay: focus=%d line=%d", f.focus, area.Line())
	}
	f.handleKey(key(tea.KeyDown))
	if f.focus != 0 || area.Line() != 1 {
		t.Fatalf("down on line 0 must stay: focus=%d line=%d", f.focus, area.Line())
	}
	f.handleKey(key(tea.KeyUp))
	f.handleKey(key(tea.KeyUp))
	if f.focus != f.discardSlot() {
		t.Fatalf("up on line 0 should wrap to Discard, focus=%d", f.focus)
	}
}

func TestTextAreaValueIsOneParagraph(t *testing.T) {
	f := newEditKeyForm(&keyEntry{tagKey: "k"})
	f.fields[0].area.SetValue("a\nb  c")
	if got := f.fields[0].current(); got != "a b c" {
		t.Fatalf("current() = %q", got)
	}
}

func TestEnterInTextAreaDoesNotInsertNewline(t *testing.T) {
	f := newEditKeyForm(&keyEntry{tagKey: "k", description: "text"})
	f.layout(68, 24)
	f.handleKey(key(tea.KeyEnter))
	if f.fields[0].area.LineCount() != 1 || f.focus != 1 {
		t.Fatalf("lines=%d focus=%d", f.fields[0].area.LineCount(), f.focus)
	}
}

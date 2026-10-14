package tui

import (
	"strings"
	"testing"
)

func TestDefaultExpansionPolicy(t *testing.T) {
	m := newLoaded(t, newFakeStore())
	cases := map[string]bool{
		"g:context": true, "g:github": true, "g:lifecycle": false,
		"k:priority": false, "k:product": false,
	}
	for id, want := range cases {
		if got := m.isExpanded(id); got != want {
			t.Errorf("isExpanded(%s) = %v, want %v", id, got, want)
		}
	}
	m.expanded["g:context"] = false
	if m.isExpanded("g:context") {
		t.Errorf("explicit map entry must override the default")
	}
}

func TestValueOrdering(t *testing.T) {
	vals := []*valueEntry{
		{value: "b", entityCount: 1}, {value: "z", entityCount: 5},
		{value: "a", entityCount: 1}, {value: "m", entityCount: 0},
	}
	sortValues(vals)
	var got []string
	for _, v := range vals {
		got = append(got, v.value)
	}
	if strings.Join(got, "") != "zabm" {
		t.Fatalf("order = %v, want z a b m", got)
	}
}

func TestRebuildVisibleRetired(t *testing.T) {
	m := newLoaded(t, newFakeStore())
	m.expanded["k:priority"] = true

	m.rebuildVisible()
	if m.indexOfVisible("v:priority=old") >= 0 {
		t.Fatalf("retired value must be hidden by default")
	}
	m.showRetired = true
	m.rebuildVisible()
	if m.indexOfVisible("v:priority=old") < 0 {
		t.Fatalf("retired value must show with showRetired")
	}
	if !strings.Contains(m.View(), "old [retired]") {
		t.Fatalf("retired row needs the [retired] suffix")
	}
}

func TestRestoreCursorExpandsCollapsedParent(t *testing.T) {
	m := newLoaded(t, newFakeStore())
	if m.indexOfVisible("v:priority=p1") >= 0 {
		t.Fatalf("precondition: priority is collapsed")
	}
	m.restoreCursor("v:priority=p1")
	if m.currentID() != "v:priority=p1" || !m.expanded["k:priority"] {
		t.Fatalf("cursor=%s expanded=%v", m.currentID(), m.expanded)
	}

	m.restoreCursor("v:nope=x", "k:product")
	if m.currentID() != "k:product" {
		t.Fatalf("should fall back to the second id, got %s", m.currentID())
	}
}

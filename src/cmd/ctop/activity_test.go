package main

import (
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/display"
	"github.com/bmjdotnet/teamster/internal/render"
)

// TestActivityViewNewestFirst is the regression for the ctop activity panel
// stacking order bug (issue #21): the most recently appended record must
// render above earlier ones, not below.
func TestActivityViewNewestFirst(t *testing.T) {
	m := newActivityModel()
	m.append(render.Record{Session: "sess1", AgentName: "@a", Tag: "EDIT", Display: "first message"})
	m.append(render.Record{Session: "sess1", AgentName: "@a", Tag: "EDIT", Display: "second message"})

	out := display.StripANSI(m.View(120, 5, true, true))
	lines := strings.Split(out, "\n")

	firstIdx, secondIdx := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "first message") {
			firstIdx = i
		}
		if strings.Contains(l, "second message") {
			secondIdx = i
		}
	}
	if firstIdx == -1 || secondIdx == -1 {
		t.Fatalf("View() output missing expected lines, got:\n%s", out)
	}
	if secondIdx >= firstIdx {
		t.Errorf("View() rendered \"second message\" (line %d) at or below \"first message\" (line %d), want the more recent entry above the older one", secondIdx, firstIdx)
	}
}

// TestActivityViewOverflowTrimsOldestLineNotNewest covers the overflow-trim
// path directly: window slicing (upstream, unchanged by this fix) already
// caps the number of *entries* to bodyH, so a test with one line per entry
// never actually exercises the `body = body[:bodyH]` trim this fix touches.
// The trim only fires when total *rendered lines* exceed bodyH even though
// entry count doesn't — here the newest entry carries both a Focus (GOAL
// line) and a Tag (tag line), so 3 entries yield 4 lines against bodyH=3.
func TestActivityViewOverflowTrimsOldestLineNotNewest(t *testing.T) {
	m := newActivityModel()
	m.append(render.Record{Session: "s", AgentName: "@a", Tag: "EDIT", Display: "oldest-single"})
	m.append(render.Record{Session: "s", AgentName: "@a", Tag: "EDIT", Display: "middle-single"})
	m.append(render.Record{Session: "s", AgentName: "@a", Tag: "EDIT", Display: "newest-tag", Focus: "newest-goal"})

	out := display.StripANSI(m.View(120, 4, true, true)) // bodyH = 3, 4 rendered lines
	lines := strings.Split(out, "\n")
	idx := func(needle string) int {
		for i, l := range lines {
			if strings.Contains(l, needle) {
				return i
			}
		}
		return -1
	}
	goalIdx, tagIdx := idx("newest-goal"), idx("newest-tag")
	middleIdx, oldIdx := idx("middle-single"), idx("oldest-single")
	if goalIdx == -1 || tagIdx == -1 {
		t.Fatalf("newest entry's lines missing entirely, got:\n%s", out)
	}
	if oldIdx != -1 {
		t.Errorf("oldest-single survived overflow trim, want it dropped; got:\n%s", out)
	}
	if middleIdx == -1 {
		t.Errorf("middle-single dropped, want it kept (only the oldest line should be trimmed); got:\n%s", out)
	}
	if !(goalIdx < tagIdx && tagIdx < middleIdx) {
		t.Errorf("wrong order: want newest-goal(%d) < newest-tag(%d) < middle-single(%d), got:\n%s", goalIdx, tagIdx, middleIdx, out)
	}
}

package store

import (
	"strings"
	"unicode"
)

// RosterDescriptionMaxRunes is the agent_roster.description column width.
const RosterDescriptionMaxRunes = 255

// SanitizeRosterDescription makes s safe to store and render: invalid UTF-8
// is dropped, whitespace and control characters (newline, tab, ESC...) collapse
// to single spaces, and the result is trimmed and cut to 255 runes.
func SanitizeRosterDescription(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > RosterDescriptionMaxRunes {
		s = strings.TrimSpace(string(r[:RosterDescriptionMaxRunes]))
	}
	return s
}

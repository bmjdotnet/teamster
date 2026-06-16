// Package display contains feed rendering logic: entity colors, tag formatting, and layout.
package display

import (
	"crypto/md5"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Tag color constants (truecolor RGB).
var tagColors = map[string][3]int{
	"GOAL": {255, 200, 60},  // warm gold — mission declaration
	"THNK": {140, 170, 130}, // sage — frequent, muted
	"DONE": {0, 102, 0},     // deep green — conclusive (256#28)
	"READ": {0, 0, 255},     // deep blue — passive file read (256#21)
	"EDIT": {128, 128, 0},   // olive/dark yellow — active file write (256#3)
	"GREP": {120, 140, 200}, // muted periwinkle — search variant
	" ACT": {230, 150, 50},  // amber — bash intent
	"EXEC": {180, 160, 60},  // olive dim — bash command detail
	"TEAM": {180, 120, 220}, // purple — agent lifecycle
	"COMM": {150, 140, 210}, // lavender — inter-agent messages
	"TASK": {240, 110, 170}, // vivid pink — distinct from cyan params
	" WEB": {210, 130, 180}, // dusty rose — external access
	" ASK": {255, 100, 100}, // coral red — human attention
	"PLAN": {160, 160, 220}, // cool lavender-grey — reflective
	"TOOL": {160, 160, 160}, // neutral grey — fallback
	"WARN": {255, 160, 40},  // orange — operator warning
}

// ANSI escape constants.
const (
	DIM   = "\033[2m"
	BOLD  = "\033[1m"
	RESET = "\033[0m"
)

// RGB returns an ANSI truecolor foreground escape sequence.
func RGB(r, g, b int) string {
	return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
}

// EntityColor returns a deterministic RGB color derived from MD5(salt+name).
// The dominant channel is boosted +40 (cap 230) and the weakest is dimmed -30 (floor 40).
func EntityColor(name, salt string) [3]int {
	h := md5.Sum([]byte(salt + name))
	r := 60 + int(h[0])*150/255
	g := 60 + int(h[1])*150/255
	b := 60 + int(h[2])*150/255

	ch := [3]int{r, g, b}
	mx, mn := 0, 0
	for i := 1; i < 3; i++ {
		if ch[i] > ch[mx] {
			mx = i
		}
		if ch[i] < ch[mn] {
			mn = i
		}
	}
	if ch[mx]+40 <= 230 {
		ch[mx] += 40
	} else {
		ch[mx] = 230
	}
	if ch[mn]-30 >= 40 {
		ch[mn] -= 30
	} else {
		ch[mn] = 40
	}
	return ch
}

// TagColor returns the fixed RGB color for a known tag, or grey for unknown.
func TagColor(tag string) [3]int {
	if c, ok := tagColors[tag]; ok {
		return c
	}
	return [3]int{180, 180, 180}
}

var ansiRe = regexp.MustCompile(`\033\[[^m]*m`)

// VisibleLen returns the length of s excluding ANSI escape sequences.
func VisibleLen(s string) int {
	return len([]rune(ansiRe.ReplaceAllString(s, "")))
}

// StripANSI removes all ANSI escape sequences from s.
func StripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// TruncateLine truncates line to fit within width visible characters,
// appending "…" + RESET when truncation occurs.
func TruncateLine(line string, width int) string {
	if width <= 0 || VisibleLen(line) <= width {
		return line
	}
	chunks := ansiRe.Split(line, -1)
	escapes := ansiRe.FindAllString(line, -1)

	// Interleave: chunk[0], esc[0], chunk[1], esc[1], ...
	var out strings.Builder
	vis := 0
	ei := 0
	for ci, chunk := range chunks {
		remaining := width - vis - 1 // room for ellipsis
		if remaining <= 0 {
			break
		}
		runes := []rune(chunk)
		if len(runes) > remaining {
			out.WriteString(string(runes[:remaining]))
			out.WriteString("…")
			vis += remaining + 1
			break
		}
		out.WriteString(chunk)
		vis += len(runes)
		if ei < len(escapes) && ci < len(chunks)-1 {
			out.WriteString(escapes[ei])
			ei++
		}
	}
	out.WriteString(RESET)
	return out.String()
}

// ParamStyle is the ANSI sequence applied to __param__ markers in display text.
// Default is BOLD. Override with TEAMSTER_PARAM_STYLE env var.
var ParamStyle = RESET + "\033[38;5;51m" // cyan (256-color #51)

var paramRe = regexp.MustCompile(`__([^_]+)__`)
var nameRe = regexp.MustCompile(`([@#][\w-]+|<[\w.-]+>)`)

func init() {
	if v := os.Getenv("TEAMSTER_PARAM_STYLE"); v != "" {
		ParamStyle = v
	}
}

// RenderDisplay processes display text: applies tag color, renders __param__
// markers in ParamStyle, and colorizes @agent/#team/<model> entities.
func RenderDisplay(text string, tagColor [3]int, sessionSalt string) string {
	tc := RGB(tagColor[0], tagColor[1], tagColor[2])
	rendered := paramRe.ReplaceAllStringFunc(text, func(match string) string {
		inner := match[2 : len(match)-2]
		return ParamStyle + inner + RESET + tc
	})
	if nameRe.MatchString(rendered) {
		return ColorizeNames(rendered, tagColor, sessionSalt)
	}
	return rendered
}

// ColorizeNames applies entity-derived colors to @agents and #teams,
// dims <model> tags, and renders the rest of text in baseColor.
func ColorizeNames(text string, baseColor [3]int, salt string) string {
	parts := nameRe.Split(text, -1)
	matches := nameRe.FindAllString(text, -1)

	br := RGB(baseColor[0], baseColor[1], baseColor[2])
	var out strings.Builder
	mi := 0
	for pi, part := range parts {
		out.WriteString(br)
		out.WriteString(part)
		out.WriteString(RESET)
		if mi < len(matches) && pi < len(parts)-1 {
			m := matches[mi]
			mi++
			if m[0] == '@' || m[0] == '#' {
				nc := EntityColor(m, salt)
				out.WriteString(BOLD)
				out.WriteString(RGB(nc[0], nc[1], nc[2]))
				out.WriteString(m)
				out.WriteString(RESET)
			} else {
				// <model> tag
				out.WriteString(DIM)
				out.WriteString(br)
				out.WriteString(m)
				out.WriteString(RESET)
			}
		}
	}
	return out.String()
}

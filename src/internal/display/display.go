// Package display contains feed rendering logic: entity colors, tag formatting, and layout.
package display

import (
	"crypto/md5"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/bmjdotnet/teamster/internal/intercept"
)

// tagColorsMu guards embeddedTagColors and overlayTagColors. TagColor is
// called from every feed-rendering path (hookd, feed, ctop) and SetTagColors
// is called once at hookd/feed startup after loading interceptors.yaml — an
// RWMutex keeps the common read path cheap.
var tagColorsMu sync.RWMutex

// embeddedTagColors is populated at init from intercept.LoadDefault()'s Tags
// map — the single source of truth for tag definitions (label, color,
// description) now lives in interceptors.yaml, not a literal Go map here.
// TestEmbeddedDefaultIsValid (internal/intercept) is the build-time gate
// that guarantees LoadDefault never errors.
var embeddedTagColors = map[string][3]int{}

// overlayTagColors holds colors from a loaded interceptors.yaml, set via
// SetTagColors. nil until a caller opts in.
var overlayTagColors map[string][3]int

func init() {
	reg, err := intercept.LoadDefault()
	if err != nil {
		slog.Error("display: embedded interceptor default config failed to load — tag colors will fall back to grey", "error", err)
		return
	}
	for label, def := range reg.Tags {
		embeddedTagColors[label] = def.Color
	}
}

// SetTagColors overlays colors loaded from interceptors.yaml on top of the
// embedded defaults. Called once at startup by binaries that load the
// config file (hookd, feed); binaries that don't still render correctly
// from the embedded defaults alone (design doc §3.4/C6).
func SetTagColors(colors map[string][3]int) {
	tagColorsMu.Lock()
	defer tagColorsMu.Unlock()
	overlayTagColors = colors
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

// BGRGB returns an ANSI truecolor background escape sequence.
func BGRGB(r, g, b uint8) string {
	return fmt.Sprintf("\033[48;2;%d;%d;%dm", r, g, b)
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

// TagColor returns the color for tag: the loaded-config overlay if
// SetTagColors was called and defines it, else the embedded default, else
// grey for a genuinely unknown tag.
func TagColor(tag string) [3]int {
	tagColorsMu.RLock()
	defer tagColorsMu.RUnlock()
	if overlayTagColors != nil {
		if c, ok := overlayTagColors[tag]; ok {
			return c
		}
	}
	if c, ok := embeddedTagColors[tag]; ok {
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

var paramRe = regexp.MustCompile(`__(.+?)__`)
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

// Package web serves the Teamster htmx dashboard and formats SSE event HTML.
package web

import (
	"crypto/md5"
	"embed"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/store"
)

//go:embed dashboard.html wms.html cost_flow.html tags.html
var assets embed.FS

// HandleDashboard serves the embedded dashboard HTML.
func HandleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("dashboard.html")
	if err != nil {
		http.Error(w, "dashboard not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data) //nolint:errcheck
}

// tagCSSClass returns the CSS class for a given tag, stripping leading spaces.
func tagCSSClass(tag string) string {
	return "tag-" + strings.TrimSpace(tag)
}

var paramReHTML = regexp.MustCompile(`__(.+?)__`)

// renderDisplayHTML escapes display text for HTML and wraps __param__ markers
// in <span class="param"> for styling.
func renderDisplayHTML(s string) string {
	indices := paramReHTML.FindAllStringIndex(s, -1)
	if len(indices) == 0 {
		return html.EscapeString(s)
	}
	var sb strings.Builder
	prev := 0
	for _, loc := range indices {
		sb.WriteString(html.EscapeString(s[prev:loc[0]]))
		inner := s[loc[0]+2 : loc[1]-2]
		sb.WriteString(`<span class="param">`)
		sb.WriteString(html.EscapeString(inner))
		sb.WriteString(`</span>`)
		prev = loc[1]
	}
	sb.WriteString(html.EscapeString(s[prev:]))
	return sb.String()
}

// entityColor derives a hex color from MD5(salt+name), matching display.EntityColor logic.
func entityColor(name string) string {
	h := md5.Sum([]byte("agent" + name))
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
	return fmt.Sprintf("#%02x%02x%02x", ch[0], ch[1], ch[2])
}

// FormatEventHTML converts a JSONL record (map) into an HTML snippet for SSE.
// Returns a single-line <div> suitable for htmx sse-swap="message" insertion.
func FormatEventHTML(rec map[string]interface{}) string {
	str := func(key string) string {
		v, _ := rec[key].(string)
		return v
	}

	rawTS := str("ts")
	displayTS := rawTS
	if t, err := time.Parse("2006-01-02T15:04:05Z", rawTS); err == nil {
		displayTS = t.Local().Format("15:04:05")
	}

	tag := str("tag")
	display := str("display")
	agentName := str("agent_name")
	focus := str("focus")

	// GOAL/focus events get their own styling.
	if tag == "" && focus != "" {
		escaped := html.EscapeString(focus)
		return fmt.Sprintf(
			`<div class="focus-line"><span class="ts">%s</span> <span class="tag tag-GOAL">[GOAL]</span> %s</div>`,
			html.EscapeString(displayTS), escaped,
		)
	}

	if tag == "" {
		tag = str("event")
	}

	cssClass := tagCSSClass(tag)
	displayTag := strings.TrimSpace(tag)

	var sb strings.Builder
	sb.WriteString(`<div class="event">`)
	sb.WriteString(fmt.Sprintf(`<span class="ts">%s</span> `, html.EscapeString(displayTS)))
	sb.WriteString(fmt.Sprintf(`<span class="tag %s">[%s]</span> `, cssClass, html.EscapeString(displayTag)))

	if agentName != "" {
		color := entityColor(agentName)
		sb.WriteString(fmt.Sprintf(`<span class="agent" style="color:%s">@%s</span> `,
			color, html.EscapeString(agentName)))
	}

	if display != "" {
		sb.WriteString(renderDisplayHTML(display))
	}

	sb.WriteString("</div>")
	return sb.String()
}

// --- WMS dashboard ---

// wmsPageData is the top-level template data. V2Outcomes reuses
// store.WMSTreeOutcome directly — its field names (ID, Title, Status, Focus,
// WorkUnits, Children) already match what wms.html's "outcome-node" template
// range-accesses, so no parallel type is needed.
type wmsPageData struct {
	V2Outcomes []store.WMSTreeOutcome
	Error      string
}

var wmsTmpl = template.Must(template.New("wms").ParseFS(assets, "wms.html"))

// HandleWMS returns an http.HandlerFunc that renders the WMS state dashboard.
// rep may be nil if the WMS store is not yet available; the page shows an
// empty state.
func HandleWMS(rep store.ReportingStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := wmsPageData{}

		if rep == nil {
			data.Error = "WMS store unavailable (migration failed or DB unreachable) — check hookd logs"
		} else {
			tree, err := rep.WMSTree(r.Context(), "")
			if err != nil {
				data.Error = fmt.Sprintf("query error: %v", err)
			} else {
				data.V2Outcomes = tree.Outcomes
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := wmsTmpl.ExecuteTemplate(w, "wms.html", data); err != nil {
			http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		}
	}
}

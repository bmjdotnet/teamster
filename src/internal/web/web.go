// Package web serves the Teamster htmx dashboard and formats SSE event HTML.
package web

import (
	"crypto/md5"
	"database/sql"
	"embed"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"strings"
	"time"
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
		sb.WriteString(html.EscapeString(display))
	}

	sb.WriteString("</div>")
	return sb.String()
}

// --- WMS dashboard ---

// fmtTokens formats a token count with k/M suffixes.
// wmsWorkUnit is a v2 work unit for the template.
type wmsV2WorkUnit struct {
	ID          string
	OutcomeID   string
	Title       string
	Description string
	Status      string
	AgentID     string
	Focus       string
}

// wmsOutcome is a v2 outcome node for the template (recursive).
type wmsOutcome struct {
	ID          string
	Title       string
	Description string
	Status      string
	Focus       string
	WorkUnits   []wmsV2WorkUnit
	Children    []wmsOutcome
}

// wmsPageData is the top-level template data.
type wmsPageData struct {
	V2Outcomes []wmsOutcome
	Error      string
}

var wmsTmpl = template.Must(template.New("wms").ParseFS(assets, "wms.html"))

// HandleWMS returns an http.HandlerFunc that renders the WMS state dashboard.
// db may be nil if the WMS database is not yet available; the page shows an empty state.
func HandleWMS(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := wmsPageData{}

		if db == nil {
			data.Error = "WMS store unavailable (migration failed or DB unreachable) — check hookd logs"
		} else {
			var err error
			data.V2Outcomes, err = loadWMSV2Data(r, db)
			if err != nil {
				data.Error = fmt.Sprintf("query error: %v", err)
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := wmsTmpl.ExecuteTemplate(w, "wms.html", data); err != nil {
			http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		}
	}
}

// loadWMSV2Data queries the v2 outcomes/workunits tables and builds a tree.
// Returns nil (not an error) when the v2 tables don't exist yet.
func loadWMSV2Data(r *http.Request, db *sql.DB) ([]wmsOutcome, error) {
	ctx := r.Context()

	rootRows, err := db.QueryContext(ctx, `
		SELECT o.id, o.title, COALESCE(o.description,''), COALESCE(o.status,''),
		       COALESCE(o.focus,'')
		FROM outcomes o
		WHERE NOT EXISTS (SELECT 1 FROM outcome_edges oe WHERE oe.child_id = o.id)
		ORDER BY o.created_at ASC`)
	if err != nil {
		// v2 tables may not exist on older installs — treat as empty.
		return nil, nil //nolint:nilerr
	}
	defer rootRows.Close()

	var roots []wmsOutcome
	for rootRows.Next() {
		var o wmsOutcome
		if err := rootRows.Scan(&o.ID, &o.Title, &o.Description, &o.Status, &o.Focus); err != nil {
			return nil, err
		}
		roots = append(roots, o)
	}
	rootRows.Close()
	if err := rootRows.Err(); err != nil {
		return nil, err
	}

	for i := range roots {
		if err := populateOutcome(r, db, &roots[i]); err != nil {
			return nil, err
		}
	}
	return roots, nil
}

// populateOutcome fills Children and WorkUnits for a single outcome node.
func populateOutcome(r *http.Request, db *sql.DB, o *wmsOutcome) error {
	ctx := r.Context()

	// Children.
	childRows, err := db.QueryContext(ctx, `
		SELECT o.id, o.title, COALESCE(o.description,''), COALESCE(o.status,''),
		       COALESCE(o.focus,'')
		FROM outcomes o
		JOIN outcome_edges oe ON oe.child_id = o.id
		WHERE oe.parent_id = ?
		ORDER BY o.created_at ASC`, o.ID)
	if err != nil {
		return fmt.Errorf("children of %s: %w", o.ID, err)
	}
	defer childRows.Close()
	var children []wmsOutcome
	for childRows.Next() {
		var c wmsOutcome
		if err := childRows.Scan(&c.ID, &c.Title, &c.Description, &c.Status, &c.Focus); err != nil {
			return err
		}
		children = append(children, c)
	}
	childRows.Close()
	if err := childRows.Err(); err != nil {
		return err
	}
	for i := range children {
		if err := populateOutcome(r, db, &children[i]); err != nil {
			return err
		}
	}
	o.Children = children

	// Work units.
	wuRows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(outcome_id,''), title, COALESCE(description,''),
		       COALESCE(status,''), COALESCE(agent_id,''), COALESCE(focus,'')
		FROM workunits WHERE outcome_id = ? ORDER BY created_at ASC`, o.ID)
	if err != nil {
		return fmt.Errorf("workunits for %s: %w", o.ID, err)
	}
	defer wuRows.Close()
	var wus []wmsV2WorkUnit
	for wuRows.Next() {
		var wu wmsV2WorkUnit
		if err := wuRows.Scan(&wu.ID, &wu.OutcomeID, &wu.Title, &wu.Description,
			&wu.Status, &wu.AgentID, &wu.Focus); err != nil {
			return err
		}
		wus = append(wus, wu)
	}
	wuRows.Close()
	if err := wuRows.Err(); err != nil {
		return err
	}
	o.WorkUnits = wus
	return nil
}

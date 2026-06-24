package statusui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/store"
)

// ServiceRow is the display-ready data for one service.
type ServiceRow struct {
	Label    string
	Status   string // plain text (no ANSI)
	Mode     string
	Endpoint string
}

// renderPanel wraps content in a bordered panel with a title.
func renderPanel(title, content string, width int) string {
	inner := width - 2 // subtract border chars
	if inner < 1 {
		inner = 1
	}
	titleStr := headerStyle.Render(title)
	body := lipgloss.NewStyle().Width(inner).Render(content)
	full := titleStr + "\n" + body
	return panelStyle.Width(width).Render(full)
}

// renderServicesPanel renders the services health table.
func renderServicesPanel(rows []ServiceRow, width int) string {
	inner := width - 4 // border + padding
	if inner < 10 {
		inner = 10
	}

	// Column widths: icon(1) + space(1) + label + gap(2) + status + gap(2) + endpoint
	const labelW = 30
	statusW := 16
	endpointW := inner - 1 - 1 - labelW - 2 - statusW - 2
	if endpointW < 0 {
		endpointW = 0
	}

	var sb strings.Builder
	for _, r := range rows {
		level := classifyStatus(r.Status)
		icon := renderIcon(level)
		label := r.Label
		if len(label) > labelW {
			label = label[:labelW-1] + "…"
		}
		statusStr := r.Status
		if len(statusStr) > statusW {
			statusStr = statusStr[:statusW-1] + "…"
		}
		line := fmt.Sprintf("%s %-*s  %-*s",
			icon,
			labelW, label,
			statusW, renderStatusText(statusStr, level),
		)
		if endpointW > 0 && r.Endpoint != "" {
			ep := r.Endpoint
			if lipgloss.Width(ep) > endpointW {
				ep = ep[:endpointW]
			}
			line += "  " + dimStyle.Render(ep)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}

	return renderPanel("Services", strings.TrimRight(sb.String(), "\n"), width)
}

// renderWMSPanel renders the WMS entity counts panel.
func renderWMSPanel(summary store.StatusSummary, hasStore bool, width int) string {
	var content string
	if !hasStore {
		content = dimStyle.Render("Store unavailable")
	} else {
		dot := dimStyle.Render("·")
		content = fmt.Sprintf("%-14s %s open  %s  %s done\n%-14s %s open  %s  %s done",
			"Outcomes",
			boldStyle.Render(fmt.Sprintf("%3d", summary.OutcomesOpen)),
			dot,
			dimStyle.Render(fmt.Sprintf("%3d", summary.OutcomesDone)),
			"Work Units",
			boldStyle.Render(fmt.Sprintf("%3d", summary.WorkUnitsOpen)),
			dot,
			dimStyle.Render(fmt.Sprintf("%3d", summary.WorkUnitsDone)),
		)
	}
	return renderPanel("WMS", content, width)
}

// renderDatabasePanel renders the database stats panel.
func renderDatabasePanel(summary store.StatusSummary, width int) string {
	content := fmt.Sprintf("%-14s %s MB\n%-14s %s\n%-14s %d",
		"Size", textStyle.Render(fmt.Sprintf("%.1f", summary.DBSizeMB)),
		"Messages", textStyle.Render(formatInt(summary.TotalMessages)),
		"Models", summary.DistinctModels,
	)
	return renderPanel("Database", content, width)
}

// renderCostPanel renders the cost + attribution panel.
func renderCostPanel(summary store.StatusSummary, sweepAge string, width int) string {
	lines := []string{
		fmt.Sprintf("%-14s %s", "Today", boldStyle.Render(fmt.Sprintf("$%.2f", summary.TodayCostUSD))),
		fmt.Sprintf("%-14s %s", "All-time", boldStyle.Render(fmt.Sprintf("$%.2f", summary.TotalCostUSD))),
	}
	if summary.TotalAttributions > 0 {
		pct := float64(summary.MappedAttributions) / float64(summary.TotalAttributions) * 100
		lines = append(lines, fmt.Sprintf("%-14s %s", "Attributed", renderAttribution(pct)))
	}
	lines = append(lines, fmt.Sprintf("%-14s %s", "Last rollup", dimStyle.Render(sweepAge)))
	return renderPanel("Cost", strings.Join(lines, "\n"), width)
}

// renderSessionsPanel renders the sessions panel.
func renderSessionsPanel(summary store.StatusSummary, width int) string {
	content := fmt.Sprintf(
		"%-14s %s sessions · %s agents\n%-14s %s active · %s all-time\n%-14s %d",
		"Active",
		boldStyle.Render(fmt.Sprintf("%d", summary.ActiveSessions)),
		boldStyle.Render(fmt.Sprintf("%d", summary.ActiveAgents)),
		"Users",
		textStyle.Render(fmt.Sprintf("%d", summary.ActiveUsers)),
		dimStyle.Render(fmt.Sprintf("%d", summary.AllTimeUsers)),
		"Hosts", summary.ActiveHosts,
	)
	return renderPanel("Sessions", content, width)
}

// formatInt renders an int64 with thousands separators.
func formatInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	start := len(s) % 3
	if start > 0 {
		b.WriteString(s[:start])
	}
	for i := start; i < len(s); i += 3 {
		if i > 0 || start > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

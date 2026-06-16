package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// viewScreen1 renders the Welcome screen.
func (m WizardModel) viewScreen1(width int) string {
	var sb strings.Builder
	sb.WriteString(HeaderStyle("Teamster Tag Setup", stepLabel(1), width))
	sb.WriteString("\n\n")
	sb.WriteString(TextStyle.Render("Tags are key:value pairs that classify your work for cost attribution"))
	sb.WriteString("\n")
	sb.WriteString(TextStyle.Render("and reporting. They attach to outcomes and work units, letting you"))
	sb.WriteString("\n")
	sb.WriteString(TextStyle.Render("answer questions like \"how much did feature X cost?\" or \"what fraction"))
	sb.WriteString("\n")
	sb.WriteString(TextStyle.Render("of spend is bug fixing vs new features?\""))
	sb.WriteString("\n\n")
	sb.WriteString(TextStyle.Render("There are two categories:"))
	sb.WriteString("\n\n")
	sb.WriteString("  " + BoldAccentStyle.Render("Context tags") + "  " + TextStyle.Render("yours to define -- product, feature, priority,"))
	sb.WriteString("\n")
	sb.WriteString("               " + TextStyle.Render("ticket IDs. Durable; inherited down the entity DAG."))
	sb.WriteString("\n\n")
	sb.WriteString("  " + BoldAccentStyle.Render("Lifecycle tags") + " " + TextStyle.Render("managed by the engine -- phase, work-type,"))
	sb.WriteString("\n")
	sb.WriteString("               " + TextStyle.Render("resolution. Set automatically by the classifier"))
	sb.WriteString("\n")
	sb.WriteString("               " + TextStyle.Render("and execution loop."))
	sb.WriteString("\n\n")
	sb.WriteString(TextStyle.Render("This wizard walks you through initial vocabulary setup."))
	sb.WriteString("\n")
	sb.WriteString(TextStyle.Render("It takes about 2 minutes."))
	sb.WriteString("\n\n\n")
	sb.WriteString(statusBar(width, "Press Enter to continue", "q quit"))
	return sb.String()
}

// viewScreen2 renders the Integration Selection screen.
func (m WizardModel) viewScreen2(width int) string {
	var sb strings.Builder
	sb.WriteString(HeaderStyle("Select Integrations", stepLabel(2), width))
	sb.WriteString("\n\n")

	leftW := width*40/100 - 2
	rightW := width - leftW - 6

	// Left panel: checkbox list
	leftContent := m.integrationList.Render(leftW, m.height-10)
	leftPanel := FocusedBorder.Width(leftW).Render(
		lipgloss.NewStyle().PaddingLeft(1).Render(leftContent),
	)

	// Right panel: detail for highlighted integration
	var rightContent string
	if m.integrationList.Cursor < len(Integrations) {
		intg := Integrations[m.integrationList.Cursor]
		var detail strings.Builder
		detail.WriteString(BoldAccentStyle.Render(intg.Name))
		detail.WriteString("\n\n")
		detail.WriteString(TextStyle.Render(intg.Description))
		detail.WriteString("\n\n")
		detail.WriteString(TextStyle.Render("Keys:"))
		detail.WriteString("\n")
		for _, k := range intg.Keys {
			detail.WriteString("  " + AccentStyle.Render(k.Key) + "\n")
		}
		rightContent = detail.String()
	}
	rightPanel := UnfocusedBorder.Width(rightW).Render(
		lipgloss.NewStyle().PaddingLeft(1).Render(rightContent),
	)

	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, "  ", rightPanel))
	sb.WriteString("\n")
	sb.WriteString(statusBar(width, "↑↓ navigate  Space toggle  Enter continue  Esc back", "q quit"))
	return sb.String()
}

// viewScreen3 renders the Universal Keys Education screen.
func (m WizardModel) viewScreen3(width int) string {
	var sb strings.Builder
	sb.WriteString(HeaderStyle("Universal Context Keys", stepLabel(3), width))
	sb.WriteString("\n\n")
	sb.WriteString(TextStyle.Render("These keys are always available regardless of your integration choices."))
	sb.WriteString("\n")
	sb.WriteString(TextStyle.Render("They form the primary classification vocabulary for your work."))
	sb.WriteString("\n\n")

	for _, uk := range UniversalKeys {
		keyCol := fmt.Sprintf("  %-18s", uk.Key)
		sb.WriteString(BoldAccentStyle.Render(keyCol) + TextStyle.Render(uk.Summary))
		sb.WriteString("\n")
	}

	sb.WriteString("\n")
	sb.WriteString(TextStyle.Render("These compose into drill-down hierarchies at query time:"))
	sb.WriteString("\n\n")
	sb.WriteString("  " + AccentStyle.Render("product:teamster") + " + " + AccentStyle.Render("feature:dashboard-rework"))
	sb.WriteString("\n")
	sb.WriteString("    " + DimStyle.Render("\"this session was building dashboards for Teamster\""))
	sb.WriteString("\n\n")
	sb.WriteString("  " + AccentStyle.Render("product:scrollz") + " + " + AccentStyle.Render("bug:scrollback-corrupt-on-resize") + " + " + AccentStyle.Render("priority:p0"))
	sb.WriteString("\n")
	sb.WriteString("    " + DimStyle.Render("\"critical bug fix in the ScrollZ scrollback renderer\""))
	sb.WriteString("\n\n")
	sb.WriteString("  " + AccentStyle.Render("product:homelab") + " + " + AccentStyle.Render("component:tautulli"))
	sb.WriteString("\n")
	sb.WriteString("    " + DimStyle.Render("\"homelab maintenance on the Tautulli service\""))
	sb.WriteString("\n\n")
	sb.WriteString(statusBar(width, "Enter continue  Esc back", "q quit"))
	return sb.String()
}

// viewScreen4 renders the Product Setup screen.
func (m WizardModel) viewScreen4(width int) string {
	var sb strings.Builder
	sb.WriteString(HeaderStyle("Add Your Products", stepLabel(4), width))
	sb.WriteString("\n\n")

	leftW := width*45/100 - 2
	rightW := width - leftW - 6

	// Left: input + product list
	var leftContent strings.Builder
	leftContent.WriteString(TextStyle.Render("What products or areas do"))
	leftContent.WriteString("\n")
	leftContent.WriteString(TextStyle.Render("you work on? Type a slug"))
	leftContent.WriteString("\n")
	leftContent.WriteString(TextStyle.Render("and press Enter to add."))
	leftContent.WriteString("\n\n")

	// Text input
	inputStyle := UnfocusedBorder.Width(leftW - 4)
	if m.productModel.Focus == 0 {
		inputStyle = FocusedBorder.Width(leftW - 4)
	}
	leftContent.WriteString(inputStyle.Render(m.productModel.Input.View()))
	leftContent.WriteString("\n")

	if m.productModel.Error != "" {
		leftContent.WriteString(ErrorStyle.Render(m.productModel.Error))
		leftContent.WriteString("\n")
	}

	if len(m.productModel.Products) > 0 {
		leftContent.WriteString("\n")
		leftContent.WriteString(TextStyle.Render("Added:"))
		leftContent.WriteString("\n")
		for i, p := range m.productModel.Products {
			prefix := "  "
			line := TextStyle.Render(p)
			if m.productModel.Focus == 1 && i == m.productModel.Cursor {
				line = SelectedStyle.Render(p)
				prefix = CursorStyle.Render("▸ ")
			}
			leftContent.WriteString(prefix + line + "\n")
		}
		leftContent.WriteString("\n")
		leftContent.WriteString(DimStyle.Render("d delete selected"))
	}

	leftBorder := UnfocusedBorder
	if m.productModel.Focus == 0 {
		leftBorder = FocusedBorder
	}
	leftPanel := leftBorder.Width(leftW).Render(
		lipgloss.NewStyle().PaddingLeft(1).Render(leftContent.String()),
	)

	// Right: examples
	var rightContent strings.Builder
	rightContent.WriteString(TextStyle.Render("Solo dev:"))
	rightContent.WriteString("\n")
	rightContent.WriteString("  " + AccentStyle.Render("teamster, scrollz"))
	rightContent.WriteString("\n\n")
	rightContent.WriteString(TextStyle.Render("Homelab operator:"))
	rightContent.WriteString("\n")
	rightContent.WriteString("  " + AccentStyle.Render("homelab, teamster"))
	rightContent.WriteString("\n\n")
	rightContent.WriteString(TextStyle.Render("Team:"))
	rightContent.WriteString("\n")
	rightContent.WriteString("  " + AccentStyle.Render("platform, api-server, mobile"))
	rightContent.WriteString("\n\n")
	rightContent.WriteString(DimStyle.Render("Product slugs are lowercase"))
	rightContent.WriteString("\n")
	rightContent.WriteString(DimStyle.Render("kebab-case. They become the"))
	rightContent.WriteString("\n")
	rightContent.WriteString(DimStyle.Render("primary aggregation axis in"))
	rightContent.WriteString("\n")
	rightContent.WriteString(DimStyle.Render("your dashboards."))

	rightPanel := UnfocusedBorder.Width(rightW).Render(
		lipgloss.NewStyle().PaddingLeft(1).Render(rightContent.String()),
	)

	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, "  ", rightPanel))
	sb.WriteString("\n")
	sb.WriteString(statusBar(width, "Enter add  d delete  Tab switch focus  Esc back", "q quit"))
	return sb.String()
}

// viewScreen5 renders the Universal Key Review screen (Screen 5).
func (m WizardModel) viewScreen5(width int) string {
	var sb strings.Builder
	sb.WriteString(HeaderStyle("Universal Key Review", stepLabel(5), width))
	sb.WriteString("\n\n")
	sb.WriteString(TextStyle.Render("These context keys will be seeded for your install."))
	sb.WriteString("\n")
	sb.WriteString(DimStyle.Render("Use ↑↓ to browse. All 6 keys are always included."))
	sb.WriteString("\n\n")

	leftW := width*35/100 - 2
	rightW := width - leftW - 6

	// Left panel: navigable key list
	var leftContent strings.Builder
	for i, uk := range UniversalKeys {
		if i == m.screen5Cursor {
			leftContent.WriteString(CursorStyle.Render("▸ ") + SelectedStyle.Render(uk.Key) + "\n")
		} else {
			leftContent.WriteString("  " + AccentStyle.Render(uk.Key) + "\n")
		}
	}
	leftPanel := FocusedBorder.Width(leftW).Render(
		lipgloss.NewStyle().PaddingLeft(1).Render(leftContent.String()),
	)

	// Right panel: detail for selected key
	sel := UniversalKeys[m.screen5Cursor]
	var rightContent strings.Builder
	rightContent.WriteString(BoldAccentStyle.Render(sel.Key))
	rightContent.WriteString("\n\n")
	rightContent.WriteString(DimStyle.Render("Cardinality: ") + TextStyle.Render(sel.Cardinality))
	rightContent.WriteString("\n")
	rightContent.WriteString(DimStyle.Render("Category:    ") + TextStyle.Render("context"))
	rightContent.WriteString("\n\n")
	rightContent.WriteString(TextStyle.Render(sel.Description))
	rightContent.WriteString("\n\n")
	// Show product values for the product key
	if sel.Key == "product" && len(m.productModel.Products) > 0 {
		rightContent.WriteString(DimStyle.Render("Your values:"))
		rightContent.WriteString("\n")
		for _, p := range m.productModel.Products {
			rightContent.WriteString("  " + AccentStyle.Render(p) + "\n")
		}
	} else if sel.Key == "priority" {
		rightContent.WriteString(DimStyle.Render("Seeded values: p0, p1, p2, p3"))
		rightContent.WriteString("\n")
	}
	rightPanel := UnfocusedBorder.Width(rightW).Render(
		lipgloss.NewStyle().PaddingLeft(1).Render(rightContent.String()),
	)

	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, "  ", rightPanel))
	sb.WriteString("\n")
	sb.WriteString(statusBar(width, "↑↓ navigate  Enter continue  Esc back", "q quit"))
	return sb.String()
}

// viewScreen6 renders the Integration Keys Review screen.
func (m WizardModel) viewScreen6(width int) string {
	var sb strings.Builder
	sb.WriteString(HeaderStyle("Integration Keys Review", stepLabel(6), width))
	sb.WriteString("\n\n")
	sb.WriteString(TextStyle.Render("Based on your selections, these keys will be added to your vocabulary."))
	sb.WriteString("\n")
	sb.WriteString(TextStyle.Render("Deselect any you don't need."))
	sb.WriteString("\n\n")

	innerW := width - 4
	sb.WriteString(m.keyList.Render(innerW, m.height-12))
	sb.WriteString("\n")
	sb.WriteString(statusBar(width, "↑↓ navigate  Space toggle  Enter continue  Esc back", "q quit"))
	return sb.String()
}

// viewScreen7 renders the Lifecycle Tags Orientation screen.
func (m WizardModel) viewScreen7(width int) string {
	var sb strings.Builder
	sb.WriteString(HeaderStyle("Lifecycle Tags (Engine-Managed)", stepLabel(7), width))
	sb.WriteString("\n\n")
	sb.WriteString(TextStyle.Render("These tags are set automatically by the classifier and execution loop."))
	sb.WriteString("\n")
	sb.WriteString(DimStyle.Render("You don't configure them -- the system handles them. But you'll see"))
	sb.WriteString("\n")
	sb.WriteString(DimStyle.Render("them in reports and dashboards."))
	sb.WriteString("\n\n")

	for _, lt := range LifecycleTags {
		sb.WriteString("  " + LifecycleKeyStyle.Render(fmt.Sprintf("%-18s", lt.Key)) + TextStyle.Render(lt.Desc))
		sb.WriteString("\n")
		sb.WriteString("  " + repeat(" ", 18) + DimStyle.Render("Values: "+lt.Values))
		sb.WriteString("\n\n")
	}

	sb.WriteString(TextStyle.Render("These tags appear in your Phase Cost Waterfall (D3) and Cost by"))
	sb.WriteString("\n")
	sb.WriteString(TextStyle.Render("Work-Type panels, giving you automatic lifecycle cost breakdowns"))
	sb.WriteString("\n")
	sb.WriteString(TextStyle.Render("without manual tagging effort."))
	sb.WriteString("\n\n")
	sb.WriteString(statusBar(width, "Enter continue  Esc back", "q quit"))
	return sb.String()
}

// viewScreen8 renders the Summary + Apply screen.
func (m WizardModel) viewScreen8(width int) string {
	var sb strings.Builder
	sb.WriteString(HeaderStyle("Setup Complete", stepLabel(8), width))
	sb.WriteString("\n\n")

	if m.errMsg != "" {
		sb.WriteString(ErrorStyle.Render("Error: " + m.errMsg))
		sb.WriteString("\n\n")
	}

	if !m.applyDone {
		sb.WriteString(TextStyle.Render("Your tag vocabulary will be configured:"))
		sb.WriteString("\n\n")

		// Count integrations
		selectedIntNames := []string{}
		intKeyCount := 0
		for i, item := range m.integrationList.Items {
			if item.Checked {
				selectedIntNames = append(selectedIntNames, Integrations[i].Name)
				intKeyCount += len(Integrations[i].Keys)
			}
		}
		// Respect key deselections from screen 6
		if len(m.keyList.Items) > 0 {
			intKeyCount = 0
			for _, k := range m.keyList.Items {
				if k.Checked {
					intKeyCount++
				}
			}
		}

		sb.WriteString(fmt.Sprintf("  "+TextStyle.Render("%-22s")+AccentStyle.Render("%s"),
			"Products:", strings.Join(m.productModel.Products, ", ")))
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("  "+TextStyle.Render("%-22s")+AccentStyle.Render("%d"),
			"Universal keys:", len(UniversalKeys)))
		sb.WriteString("\n")
		if len(selectedIntNames) > 0 {
			sb.WriteString(fmt.Sprintf("  "+TextStyle.Render("%-22s")+AccentStyle.Render("%d (%s)"),
				"Integration keys:", intKeyCount, strings.Join(selectedIntNames, ", ")))
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("  "+TextStyle.Render("%-22s")+DimStyle.Render("%d (engine-managed)"),
			"Lifecycle keys:", len(LifecycleTags)))
		sb.WriteString("\n\n")

		rule := DimStyle.Render(repeat("─", width-4))
		sb.WriteString(rule)
		sb.WriteString("\n\n")

		sb.WriteString(TextStyle.Render("What's next:"))
		sb.WriteString("\n\n")
		sb.WriteString("  " + AccentStyle.Render("teamster setup tags") + "             " + DimStyle.Render("open the tag editor anytime"))
		sb.WriteString("\n")
		sb.WriteString("  " + AccentStyle.Render("teamster setup tags --interview") + "  " + DimStyle.Render("re-run this wizard"))
		sb.WriteString("\n")
		sb.WriteString("  " + AccentStyle.Render("teamster tags list") + "              " + DimStyle.Render("see your full vocabulary"))
		sb.WriteString("\n\n")
		sb.WriteString(statusBar(width, "Enter apply  Esc back", "q quit"))
	} else {
		for _, r := range m.applyResults {
			sb.WriteString(SuccessStyle.Render("✓ ") + TextStyle.Render(r))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
		sb.WriteString(SuccessStyle.Render("Done! ") + TextStyle.Render("Run ") + AccentStyle.Render("teamster setup tags") + TextStyle.Render(" to open the tag editor."))
		sb.WriteString("\n\n")
		sb.WriteString(statusBar(width, "Enter finish", ""))
	}
	return sb.String()
}

// statusBar renders the bottom hint bar.
func statusBar(width int, left, right string) string {
	l := StatusBarStyle.Render(left)
	r := StatusBarStyle.Render(right)
	gap := width - lipgloss.Width(l) - lipgloss.Width(r)
	if gap < 1 {
		gap = 1
	}
	return l + repeat(" ", gap) + r
}

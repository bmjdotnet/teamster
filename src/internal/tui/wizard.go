package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bmjdotnet/teamster/internal/store"
	"github.com/bmjdotnet/teamster/internal/wms"
)

const totalScreens = 9

// conventionEntry holds editable convention metadata for one context-tag key.
type conventionEntry struct {
	key            string
	scope          string // "outcome" | "workunit" | ""
	interview      string // "propose" | "auto" | "skip"
	exclusionGroup string // free-text slug
	autoExtract    string // "git" | "env" | ""
}

// WizardModel is the bubbletea Model for the 9-screen setup wizard.
type WizardModel struct {
	screen int
	width  int
	height int
	st     store.TagAdminStore
	done   bool
	errMsg string

	// Screen 2: integration selection
	integrationList CheckboxList

	// Screen 4: product entry
	productModel ProductListModel

	// Screen 5: universal key review cursor
	screen5Cursor int

	// Screen 5b: tag conventions
	conventions   []conventionEntry
	convCursor    int
	convCol       int  // 0=scope, 1=interview, 2=exclusionGroup, 3=autoExtract
	convEditing   bool // true when typing into exclusionGroup field
	convEditInput textinput.Model

	// Screen 6: integration key review (only if integrations selected)
	keyList FlatKeyList

	// Applied state (written on screen 9)
	applyDone    bool
	applyResults []string
}

// NewWizardModel creates a WizardModel. st may be nil for screens that don't need it.
func NewWizardModel(st store.TagAdminStore) WizardModel {
	// Build integration checkbox items
	items := make([]CheckboxItem, len(Integrations))
	for i, intg := range Integrations {
		items[i] = CheckboxItem{
			Label:       intg.Name,
			Description: intg.Description,
		}
	}
	cl := NewCheckboxList(items)
	cl.Focused = true

	cei := textinput.New()
	cei.Placeholder = "e.g. work-scope"
	cei.CharLimit = 64
	cei.Width = 20

	return WizardModel{
		screen:          0,
		st:              st,
		integrationList: cl,
		productModel:    NewProductListModel(),
		convEditInput:   cei,
	}
}

func (m WizardModel) Init() tea.Cmd {
	return nil
}

func (m WizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "q":
			if m.screen != 8 { // screen 9 uses Enter to finish
				return m, tea.Quit
			}

		case "esc":
			if m.screen == 5 && m.convEditing {
				m.convEditing = false
				m.convEditInput.Blur()
				return m, nil
			}
			if m.screen > 0 {
				m.screen--
				if m.screen == 6 && !m.hasIntegrations() {
					// skip screen 7 backwards too
					m.screen--
				}
			}
			return m, nil

		case "enter":
			return m.handleEnter()

		case "up":
			m.handleUp()
			return m, nil

		case "down":
			m.handleDown()
			return m, nil

		case "tab":
			m.handleTab(false)
			return m, nil

		case "shift+tab":
			m.handleTab(true)
			return m, nil

		case " ":
			m.handleSpace()
			return m, nil

		case "d":
			if m.screen == 3 { // product screen
				m.productModel.DeleteSelected()
			}
			return m, nil
		}

		// Route text input to product model on screen 3
		if m.screen == 3 && m.productModel.Focus == 0 {
			var cmd tea.Cmd
			m.productModel.Input, cmd = m.productModel.Input.Update(msg)
			return m, cmd
		}

		// Route text input to convention exclusion group editor on screen 5
		if m.screen == 5 && m.convEditing {
			var cmd tea.Cmd
			m.convEditInput, cmd = m.convEditInput.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func (m *WizardModel) handleEnter() (tea.Model, tea.Cmd) {
	switch m.screen {
	case 0, 1, 2, 7:
		// Advance
		m.advance()
	case 3: // product screen
		if m.productModel.Focus == 0 {
			val := strings.TrimSpace(m.productModel.Input.Value())
			if val != "" {
				m.productModel.AddProduct()
			} else if len(m.productModel.Products) > 0 {
				m.advance()
			}
		} else {
			if len(m.productModel.Products) > 0 {
				m.advance()
			}
		}
	case 4: // key editor (Screen 5) — advance
		m.advance()
	case 5: // conventions screen — advance (unless editing exclusion group)
		if m.convEditing {
			m.conventions[m.convCursor].exclusionGroup = strings.TrimSpace(m.convEditInput.Value())
			m.convEditing = false
			m.convEditInput.Blur()
		} else {
			m.advance()
		}
	case 6: // integration key review (Screen 7) — advance
		m.advance()
	case 8: // Summary screen — apply and finish
		if !m.applyDone {
			m.applySetup()
		} else {
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *WizardModel) advance() {
	m.screen++
	// Build conventions list when entering screen 5b (index 5)
	if m.screen == 5 {
		m.buildConventions()
	}
	// Skip screen 7 (index 6) if no integrations selected
	if m.screen == 6 && !m.hasIntegrations() {
		m.screen++
	}
	if m.screen >= totalScreens {
		m.screen = totalScreens - 1
	}
	// Build key list when entering screen 7
	if m.screen == 6 {
		m.buildKeyList()
	}
}

func (m *WizardModel) handleUp() {
	switch m.screen {
	case 1:
		m.integrationList.Up()
	case 4:
		if m.screen5Cursor > 0 {
			m.screen5Cursor--
		}
	case 5:
		if !m.convEditing && m.convCursor > 0 {
			m.convCursor--
		}
	case 6:
		m.keyList.Up()
	case 3:
		if m.productModel.Focus == 1 && m.productModel.Cursor > 0 {
			m.productModel.Cursor--
		}
	}
}

func (m *WizardModel) handleDown() {
	switch m.screen {
	case 1:
		m.integrationList.Down()
	case 4:
		if m.screen5Cursor < len(UniversalKeys)-1 {
			m.screen5Cursor++
		}
	case 5:
		if !m.convEditing && m.convCursor < len(m.conventions)-1 {
			m.convCursor++
		}
	case 6:
		m.keyList.Down()
	case 3:
		if m.productModel.Focus == 1 {
			if m.productModel.Cursor < len(m.productModel.Products)-1 {
				m.productModel.Cursor++
			}
		}
	}
}

func (m *WizardModel) handleTab(reverse bool) {
	if m.screen == 3 {
		if !reverse {
			if m.productModel.Focus == 0 {
				m.productModel.Focus = 1
				m.productModel.Input.Blur()
			} else {
				m.productModel.Focus = 0
				m.productModel.Input.Focus()
			}
		} else {
			if m.productModel.Focus == 1 {
				m.productModel.Focus = 0
				m.productModel.Input.Focus()
			} else {
				m.productModel.Focus = 1
				m.productModel.Input.Blur()
			}
		}
	}
	if m.screen == 5 {
		if m.convEditing {
			m.conventions[m.convCursor].exclusionGroup = strings.TrimSpace(m.convEditInput.Value())
			m.convEditing = false
			m.convEditInput.Blur()
		}
		cols := 4 // scope, interview, exclusionGroup, autoExtract
		if !reverse {
			m.convCol = (m.convCol + 1) % cols
		} else {
			m.convCol = (m.convCol + cols - 1) % cols
		}
		if m.convCol == 2 {
			m.convEditing = true
			m.convEditInput.SetValue(m.conventions[m.convCursor].exclusionGroup)
			m.convEditInput.Focus()
		}
	}
}

func (m *WizardModel) handleSpace() {
	switch m.screen {
	case 1:
		m.integrationList.Toggle()
	case 5:
		if !m.convEditing {
			m.cycleConventionValue()
		}
	case 6:
		m.keyList.Toggle()
	}
}

func (m *WizardModel) hasIntegrations() bool {
	for _, item := range m.integrationList.Items {
		if item.Checked {
			return true
		}
	}
	return false
}

func (m *WizardModel) buildConventions() {
	if len(m.conventions) > 0 {
		return // already built
	}
	for _, uk := range UniversalKeys {
		ce := conventionEntry{key: uk.Key}
		switch uk.Key {
		case "product", "priority", "product-version", "feature", "bug":
			ce.scope = "outcome"
			ce.interview = "propose"
		case "component":
			ce.scope = "workunit"
			ce.interview = "skip"
		}
		if uk.Key == "feature" || uk.Key == "bug" {
			ce.exclusionGroup = "work-scope"
		}
		m.conventions = append(m.conventions, ce)
	}
}

func (m *WizardModel) cycleConventionValue() {
	if len(m.conventions) == 0 {
		return
	}
	c := &m.conventions[m.convCursor]
	switch m.convCol {
	case 0: // scope
		c.scope = cycleValue(c.scope, scopeValues)
	case 1: // interview
		c.interview = cycleValue(c.interview, interviewValues)
	case 3: // autoExtract
		c.autoExtract = cycleValue(c.autoExtract, autoExtractValues)
	}
}

func (m *WizardModel) buildKeyList() {
	var items []IntegrationKeyItem
	for i, item := range m.integrationList.Items {
		if !item.Checked {
			continue
		}
		intg := Integrations[i]
		for _, k := range intg.Keys {
			items = append(items, IntegrationKeyItem{
				Integration: intg.Name,
				Key:         k.Key,
				Desc:        k.Desc,
				Checked:     true,
			})
		}
	}
	m.keyList = FlatKeyList{Items: items}
}

func (m *WizardModel) applySetup() {
	if m.st == nil {
		m.applyResults = []string{"(dry run — no database connection)"}
		m.applyDone = true
		return
	}
	ctx := context.Background()
	var results []string

	// Seed universal context keys.
	specs := make([]wms.TagSpec, len(UniversalKeys))
	for i, uk := range UniversalKeys {
		specs[i] = wms.TagSpec{
			Key:         uk.Key,
			Category:    "context",
			Cardinality: uk.Cardinality,
			Description: uk.Description,
		}
	}
	if err := m.st.SeedTags(ctx, specs); err != nil {
		m.errMsg = fmt.Sprintf("error seeding universal keys: %v", err)
		return
	}
	results = append(results, fmt.Sprintf("Seeded %d universal keys", len(specs)))

	// Apply conventions to universal keys.
	convApplied := 0
	for _, ce := range m.conventions {
		interview := ce.interview
		if interview == "" {
			interview = "propose"
		}
		if err := m.st.UpdateTagConventions(ctx, ce.key, ce.scope, ce.exclusionGroup, ce.autoExtract, interview); err != nil {
			m.errMsg = fmt.Sprintf("error applying convention for %s: %v", ce.key, err)
			return
		}
		convApplied++
	}
	if convApplied > 0 {
		results = append(results, fmt.Sprintf("Applied conventions to %d keys", convApplied))
	}

	// Seed product values.
	if len(m.productModel.Products) > 0 {
		if err := m.st.SeedProductValues(ctx, m.productModel.Products); err != nil {
			m.errMsg = fmt.Sprintf("error adding products: %v", err)
			return
		}
		results = append(results, fmt.Sprintf("Added products: %s", strings.Join(m.productModel.Products, ", ")))
	}

	// Seed selected integration keys.
	var intKeys []store.IntegrationKey
	for _, item := range m.keyList.Items {
		if !item.Checked {
			continue
		}
		intKeys = append(intKeys, store.IntegrationKey{Key: item.Key, Description: item.Desc})
	}
	if len(intKeys) > 0 {
		if err := m.st.SeedIntegrationKeys(ctx, intKeys); err != nil {
			m.errMsg = fmt.Sprintf("error seeding integration keys: %v", err)
			return
		}
		results = append(results, fmt.Sprintf("Seeded %d integration keys", len(intKeys)))
	}

	m.applyResults = results
	m.applyDone = true
}

func (m WizardModel) View() string {
	if m.width == 0 {
		return "Loading..."
	}
	if m.width < 80 || m.height < 24 {
		return ErrorStyle.Render("Terminal too small — resize to at least 80x24")
	}

	inner := m.width - 4 // 2 border + 2 padding each side
	var content string

	switch m.screen {
	case 0:
		content = m.viewScreen1(inner)
	case 1:
		content = m.viewScreen2(inner)
	case 2:
		content = m.viewScreen3(inner)
	case 3:
		content = m.viewScreen4(inner)
	case 4:
		content = m.viewScreen5(inner)
	case 5:
		content = m.viewScreenConventions(inner)
	case 6:
		content = m.viewScreen6(inner)
	case 7:
		content = m.viewScreen7(inner)
	case 8:
		content = m.viewScreen8(inner)
	default:
		content = "Unknown screen"
	}

	outerHeight := m.height - 2
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Width(m.width-2).
		Height(outerHeight).
		Padding(0, 1).
		Render(content)

	return box
}

func stepLabel(n int) string {
	return fmt.Sprintf("Step %d of %d", n, totalScreens)
}

// RunWizard launches the bubbletea wizard. st may be nil (dry-run mode).
func RunWizard(st store.TagAdminStore) error {
	m := NewWizardModel(st)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

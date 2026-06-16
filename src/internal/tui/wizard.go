package tui

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const totalScreens = 8

// WizardModel is the bubbletea Model for the 8-screen setup wizard.
type WizardModel struct {
	screen  int
	width   int
	height  int
	db      *sql.DB
	done    bool
	errMsg  string

	// Screen 2: integration selection
	integrationList CheckboxList

	// Screen 4: product entry
	productModel ProductListModel

	// Screen 5: universal key review cursor
	screen5Cursor int

	// Screen 6: integration key review (only if integrations selected)
	keyList FlatKeyList

	// Applied state (written on screen 8)
	applyDone    bool
	applyResults []string
}

// NewWizardModel creates a WizardModel. db may be nil for screens that don't need it.
func NewWizardModel(db *sql.DB) WizardModel {
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

	return WizardModel{
		screen:          0,
		db:              db,
		integrationList: cl,
		productModel:    NewProductListModel(),
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
			if m.screen != 7 { // screen 8 uses Enter to finish
				return m, tea.Quit
			}

		case "esc":
			if m.screen > 0 {
				m.screen--
				if m.screen == 5 && !m.hasIntegrations() {
					// skip screen 6 backwards too
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
	}

	return m, nil
}

func (m *WizardModel) handleEnter() (tea.Model, tea.Cmd) {
	switch m.screen {
	case 0, 1, 2, 6:
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
	case 5: // integration key review (Screen 6) — advance
		m.advance()
	case 7: // Summary screen — apply and finish
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
	// Skip screen 6 (index 5) if no integrations selected
	if m.screen == 5 && !m.hasIntegrations() {
		m.screen++
	}
	if m.screen >= totalScreens {
		m.screen = totalScreens - 1
	}
	// Build key list when entering screen 6
	if m.screen == 5 {
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
}

func (m *WizardModel) handleSpace() {
	switch m.screen {
	case 1:
		m.integrationList.Toggle()
	case 5:
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
	if m.db == nil {
		m.applyResults = []string{"(dry run — no database connection)"}
		m.applyDone = true
		return
	}
	ctx := context.Background()
	var results []string

	// Seed universal context keys.
	keysSeeded := 0
	for _, uk := range UniversalKeys {
		_, err := m.db.ExecContext(ctx,
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
			 VALUES (?, '', 1, 'context', ?, ?)`,
			uk.Key, uk.Cardinality, uk.Description,
		)
		if err != nil {
			m.errMsg = fmt.Sprintf("error seeding %s: %v", uk.Key, err)
			return
		}
		keysSeeded++
	}
	results = append(results, fmt.Sprintf("Seeded %d universal keys", keysSeeded))

	// Seed product values.
	for _, p := range m.productModel.Products {
		_, err := m.db.ExecContext(ctx,
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
			 VALUES ('product', ?, 0, 'context', 'single', '')`,
			p,
		)
		if err != nil {
			m.errMsg = fmt.Sprintf("error adding product %q: %v", p, err)
			return
		}
	}
	if len(m.productModel.Products) > 0 {
		results = append(results, fmt.Sprintf("Added products: %s", strings.Join(m.productModel.Products, ", ")))
	}

	// Seed selected integration keys.
	intKeysSeeded := 0
	for _, item := range m.keyList.Items {
		if !item.Checked {
			continue
		}
		_, err := m.db.ExecContext(ctx,
			`INSERT IGNORE INTO tags (tag_key, tag_value, is_seed, category, cardinality, description)
			 VALUES (?, '', 1, 'context', 'single', ?)`,
			item.Key, item.Desc,
		)
		if err != nil {
			m.errMsg = fmt.Sprintf("error seeding %s: %v", item.Key, err)
			return
		}
		intKeysSeeded++
	}
	if intKeysSeeded > 0 {
		results = append(results, fmt.Sprintf("Seeded %d integration keys", intKeysSeeded))
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
		content = m.viewScreen6(inner)
	case 6:
		content = m.viewScreen7(inner)
	case 7:
		content = m.viewScreen8(inner)
	default:
		content = "Unknown screen"
	}

	outerHeight := m.height - 2
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorBorder).
		Width(m.width - 2).
		Height(outerHeight).
		Padding(0, 1).
		Render(content)

	return box
}

func stepLabel(n int) string {
	return fmt.Sprintf("Step %d of %d", n, totalScreens)
}

// RunWizard launches the bubbletea wizard. db may be nil (dry-run mode).
func RunWizard(db *sql.DB) error {
	m := NewWizardModel(db)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

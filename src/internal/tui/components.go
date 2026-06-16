package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
)

// CheckboxItem is a selectable item with a checked state.
type CheckboxItem struct {
	Label       string
	Description string
	Checked     bool
}

// CheckboxList is a navigable list of CheckboxItems.
type CheckboxList struct {
	Items   []CheckboxItem
	Cursor  int
	Focused bool
}

func NewCheckboxList(items []CheckboxItem) CheckboxList {
	return CheckboxList{Items: items}
}

func (c *CheckboxList) Up() {
	if c.Cursor > 0 {
		c.Cursor--
	}
}

func (c *CheckboxList) Down() {
	if c.Cursor < len(c.Items)-1 {
		c.Cursor++
	}
}

func (c *CheckboxList) Toggle() {
	if c.Cursor < len(c.Items) {
		c.Items[c.Cursor].Checked = !c.Items[c.Cursor].Checked
	}
}

// Render renders the checkbox list into a string of the given height.
func (c *CheckboxList) Render(width, height int) string {
	var sb strings.Builder
	for i, item := range c.Items {
		prefix := "  "
		if i == c.Cursor && c.Focused {
			prefix = CursorStyle.Render("▸ ")
		} else {
			prefix = "  "
		}
		var checkbox string
		if item.Checked {
			checkbox = CheckedStyle.Render("[x]")
		} else {
			checkbox = UncheckedStyle.Render("[ ]")
		}
		label := item.Label
		if i == c.Cursor && c.Focused {
			label = BoldAccentStyle.Render(label)
		} else {
			label = TextStyle.Render(label)
		}
		line := prefix + checkbox + " " + label
		line = truncate(line, width)
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

// ProductListModel manages product slug entry with validation.
type ProductListModel struct {
	Input    textinput.Model
	Products []string
	Cursor   int
	Focus    int // 0=input, 1=list
	Error    string
}

func NewProductListModel() ProductListModel {
	ti := textinput.New()
	ti.Placeholder = "e.g. teamster"
	ti.CharLimit = 64
	ti.Focus()
	return ProductListModel{Input: ti, Focus: 0}
}

func (p *ProductListModel) AddProduct() {
	slug := strings.TrimSpace(p.Input.Value())
	if slug == "" {
		return
	}
	slug = strings.ToLower(slug)
	if !isValidSlug(slug) {
		p.Error = "Only a-z, 0-9, and - allowed"
		return
	}
	for _, existing := range p.Products {
		if existing == slug {
			p.Error = fmt.Sprintf("%q already added", slug)
			return
		}
	}
	p.Products = append(p.Products, slug)
	p.Input.SetValue("")
	p.Error = ""
}

func (p *ProductListModel) DeleteSelected() {
	if p.Focus != 1 || len(p.Products) == 0 {
		return
	}
	if p.Cursor >= len(p.Products) {
		p.Cursor = len(p.Products) - 1
	}
	p.Products = append(p.Products[:p.Cursor], p.Products[p.Cursor+1:]...)
	if p.Cursor >= len(p.Products) && p.Cursor > 0 {
		p.Cursor--
	}
}

func isValidSlug(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

// IntegrationKeyItem is a key within an integration for Screen 6.
type IntegrationKeyItem struct {
	Integration string
	Key         string
	Desc        string
	Checked     bool
}

// FlatKeyList is a flat navigable list of IntegrationKeyItems grouped by integration.
type FlatKeyList struct {
	Items  []IntegrationKeyItem
	Cursor int
}

func (f *FlatKeyList) Up() {
	if f.Cursor > 0 {
		f.Cursor--
	}
}

func (f *FlatKeyList) Down() {
	if f.Cursor < len(f.Items)-1 {
		f.Cursor++
	}
}

func (f *FlatKeyList) Toggle() {
	if f.Cursor < len(f.Items) {
		f.Items[f.Cursor].Checked = !f.Items[f.Cursor].Checked
	}
}

// Render renders the flat key list with group headers.
func (f *FlatKeyList) Render(width, height int) string {
	var sb strings.Builder
	currentGroup := ""
	for i, item := range f.Items {
		if item.Integration != currentGroup {
			currentGroup = item.Integration
			header := lipgloss.NewStyle().Bold(true).Foreground(ColorText).Render("  " + currentGroup)
			sb.WriteString(header + "\n")
		}
		prefix := "  "
		if i == f.Cursor {
			prefix = CursorStyle.Render("  ▸ ")
		} else {
			prefix = "    "
		}
		var checkbox string
		if item.Checked {
			checkbox = CheckedStyle.Render("[x]")
		} else {
			checkbox = UncheckedStyle.Render("[ ]")
		}
		keyStr := BoldAccentStyle.Render(item.Key)
		if i == f.Cursor {
			keyStr = SelectedStyle.Render(item.Key)
		}
		desc := DimStyle.Render(item.Desc)
		line := prefix + checkbox + " " + keyStr + "  " + desc
		line = truncate(line, width)
		sb.WriteString(line + "\n")
	}
	return sb.String()
}


package tui

import (
	"sort"
	"strings"
)

type nodeKind int

const (
	nodeGroup nodeKind = iota
	nodeKey
	nodeValue
)

type groupEntry struct {
	id, label   string
	namespace   string // "" for context, "lifecycle", or the integration namespace
	lifecycle   bool
	entityCount int // sum of its keys
}

type keyEntry struct {
	id             string // "k:" + tagKey
	tagKey         string
	category       string
	cardinality    string
	required       bool
	description    string // KEY description (TagKeySummary.Description)
	scope          string
	exclusionGroup string
	autoExtract    string
	interview      string
	facetSource    string
	entityCount    int // sum over values from TagEntityCounts
	valueCount     int // non-stub, non-retired
	retiredCount   int
	createOnApply  bool // only stub rows exist
	lifecycle      bool // category == "lifecycle"
	values         []*valueEntry
}

type valueEntry struct {
	id          string // "v:" + tagKey + "=" + value
	key         *keyEntry
	value       string
	description string // VALUE description (wms.Tag.Description)
	isSeed      bool
	retired     bool
	entityCount int
}

type treeNode struct {
	id       string
	kind     nodeKind
	depth    int
	parent   *treeNode
	children []*treeNode
	group    *groupEntry // nodeGroup
	key      *keyEntry   // nodeKey and nodeValue (value's parent key)
	value    *valueEntry // nodeValue

	// Set by rebuildVisible for the renderer.
	open    bool // children are currently shown
	visKids int  // children eligible to be shown (after retired/filter rules)
}

const (
	groupContextID   = "g:context"
	groupLifecycleID = "g:lifecycle"
)

func keyNodeID(tagKey string) string        { return "k:" + tagKey }
func valueNodeID(tagKey, val string) string { return "v:" + tagKey + "=" + val }

func groupIDForKey(tagKey string, lifecycle bool) string {
	if lifecycle {
		return groupLifecycleID
	}
	if idx := strings.Index(tagKey, "."); idx > 0 {
		return "g:" + tagKey[:idx]
	}
	return groupContextID
}

// defaultExpanded is the policy for node IDs absent from the expanded map.
func defaultExpanded(id string) bool {
	if strings.HasPrefix(id, "g:") {
		return id != groupLifecycleID
	}
	return false
}

func sortValues(vals []*valueEntry) {
	sort.SliceStable(vals, func(i, j int) bool {
		if vals[i].entityCount != vals[j].entityCount {
			return vals[i].entityCount > vals[j].entityCount
		}
		return vals[i].value < vals[j].value
	})
}

// buildTree turns the three store reads into the group → key → value tree.
func buildTree(msg treeLoaded) (groups []*groupEntry, keys map[string]*keyEntry, roots []*treeNode) {
	countByKey := map[string]int{}
	countByKeyValue := map[string]int{}
	for _, c := range msg.counts {
		countByKey[c.TagKey] += int(c.Count)
		countByKeyValue[valueNodeID(c.TagKey, c.TagValue)] += int(c.Count)
	}

	keys = map[string]*keyEntry{}
	contextG := &groupEntry{id: groupContextID, label: "Context"}
	lifecycleG := &groupEntry{id: groupLifecycleID, label: "Lifecycle (engine-managed)", namespace: "lifecycle", lifecycle: true}
	nsGroups := map[string]*groupEntry{}
	var nsOrder []*groupEntry
	groupKeys := map[*groupEntry][]*keyEntry{}

	for _, sm := range msg.keys {
		k := &keyEntry{
			id:             keyNodeID(sm.Key),
			tagKey:         sm.Key,
			category:       sm.Category,
			cardinality:    sm.Cardinality,
			required:       sm.Required,
			description:    sm.Description,
			scope:          sm.Scope,
			exclusionGroup: sm.ExclusionGroup,
			autoExtract:    sm.AutoExtract,
			interview:      sm.Interview,
			facetSource:    sm.FacetSource,
			entityCount:    countByKey[sm.Key],
			lifecycle:      sm.Category == "lifecycle",
		}
		keys[sm.Key] = k

		var g *groupEntry
		switch {
		case k.lifecycle:
			g = lifecycleG
		case strings.Index(sm.Key, ".") > 0:
			ns := sm.Key[:strings.Index(sm.Key, ".")]
			if nsGroups[ns] == nil {
				nsGroups[ns] = &groupEntry{id: "g:" + ns, label: ns, namespace: ns}
				nsOrder = append(nsOrder, nsGroups[ns])
			}
			g = nsGroups[ns]
		default:
			g = contextG
		}
		g.entityCount += k.entityCount
		groupKeys[g] = append(groupKeys[g], k)
	}

	for _, row := range msg.rows {
		if row.Value == "" {
			continue
		}
		k := keys[row.Key]
		if k == nil {
			continue
		}
		k.values = append(k.values, &valueEntry{
			id:          valueNodeID(row.Key, row.Value),
			key:         k,
			value:       row.Value,
			description: row.Description,
			isSeed:      row.IsSeed,
			retired:     row.Retired,
			entityCount: countByKeyValue[valueNodeID(row.Key, row.Value)],
		})
	}
	for _, k := range keys {
		k.createOnApply = len(k.values) == 0
		sortValues(k.values)
		for _, v := range k.values {
			if v.retired {
				k.retiredCount++
			} else {
				k.valueCount++
			}
		}
	}

	groups = append(groups, contextG)
	groups = append(groups, nsOrder...)
	if len(groupKeys[lifecycleG]) > 0 {
		groups = append(groups, lifecycleG)
	}

	for _, g := range groups {
		gn := &treeNode{id: g.id, kind: nodeGroup, group: g}
		for _, k := range groupKeys[g] {
			kn := &treeNode{id: k.id, kind: nodeKey, depth: 1, parent: gn, key: k}
			for _, v := range k.values {
				kn.children = append(kn.children, &treeNode{
					id: v.id, kind: nodeValue, depth: 2, parent: kn, key: k, value: v,
				})
			}
			gn.children = append(gn.children, kn)
		}
		roots = append(roots, gn)
	}
	return groups, keys, roots
}

func (m *EditorModel) isExpanded(id string) bool {
	if v, ok := m.expanded[id]; ok {
		return v
	}
	return defaultExpanded(id)
}

// rebuildVisible flattens the tree into m.visible applying showRetired, the
// filter and expansion state, then clamps the cursor.
func (m *EditorModel) rebuildVisible() {
	filter := strings.ToLower(m.filter)
	matches := func(s string) bool { return strings.Contains(strings.ToLower(s), filter) }
	m.visible = m.visible[:0]

	for _, gn := range m.tree {
		var keyRows [][]*treeNode
		for _, kn := range gn.children {
			k := kn.key
			keyMatch := filter == "" || matches(k.tagKey)
			var vals []*treeNode
			valueMatched := false
			for _, vn := range kn.children {
				if vn.value.retired && !m.showRetired {
					continue
				}
				vm := filter != "" && matches(vn.value.value)
				if vm {
					valueMatched = true
				}
				if filter == "" || keyMatch || vm {
					vals = append(vals, vn)
				}
			}
			if filter != "" && !keyMatch && !valueMatched {
				kn.open, kn.visKids = false, 0
				continue
			}
			kn.visKids = len(vals)
			kn.open = len(vals) > 0 && (valueMatched || m.isExpanded(kn.id))
			row := []*treeNode{kn}
			if kn.open {
				row = append(row, vals...)
			}
			keyRows = append(keyRows, row)
		}
		gn.visKids = len(keyRows)
		if filter != "" && len(keyRows) == 0 {
			gn.open = false
			continue
		}
		gn.open = filter != "" || m.isExpanded(gn.id)
		m.visible = append(m.visible, gn)
		if gn.open {
			for _, row := range keyRows {
				m.visible = append(m.visible, row...)
			}
		}
	}

	if m.cursor >= len(m.visible) {
		m.cursor = len(m.visible) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *EditorModel) cur() *treeNode {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return nil
	}
	return m.visible[m.cursor]
}

func (m *EditorModel) currentID() string {
	if n := m.cur(); n != nil {
		return n.id
	}
	return ""
}

func (m *EditorModel) findNode(id string) *treeNode {
	var walk func(ns []*treeNode) *treeNode
	walk = func(ns []*treeNode) *treeNode {
		for _, n := range ns {
			if n.id == id {
				return n
			}
			if f := walk(n.children); f != nil {
				return f
			}
		}
		return nil
	}
	return walk(m.tree)
}

func (m *EditorModel) indexOfVisible(id string) int {
	for i, n := range m.visible {
		if n.id == id {
			return i
		}
	}
	return -1
}

// restoreCursor lands on the first of ids that can be found, expanding the
// ancestors of the first id when it is hidden under a collapsed parent. When
// none is found the cursor stays clamped where rebuildVisible left it.
func (m *EditorModel) restoreCursor(ids ...string) {
	for i, id := range ids {
		if id == "" {
			continue
		}
		if i == 0 {
			if n := m.findNode(id); n != nil {
				for p := n.parent; p != nil; p = p.parent {
					m.expanded[p.id] = true
				}
				m.rebuildVisible()
			}
		}
		if idx := m.indexOfVisible(id); idx >= 0 {
			m.cursor = idx
			return
		}
	}
}

// keyOf returns the key node a key-or-value node belongs to.
func keyOf(n *treeNode) *treeNode {
	if n.kind == nodeValue {
		return n.parent
	}
	return n
}

func groupOf(n *treeNode) *treeNode {
	for n.parent != nil {
		n = n.parent
	}
	return n
}

// treeTotals counts keys, non-retired values and entities for the header.
func (m *EditorModel) treeTotals() (keys, values, entities int) {
	for _, k := range m.keys {
		keys++
		values += k.valueCount
		entities += k.entityCount
	}
	return
}

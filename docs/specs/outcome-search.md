# Outcome Search — Implementation Kit

## Problem

`wms_listOutcomes` has no keyword search — only `parentOutcomeID`, `tagFilters`,
and `status` filters. None of the start/solo/bootstrap flows search for existing
outcomes; they always create new ones. This leads to duplicate outcomes when a
session resumes work that already has a strategic Outcome.

## Design

### 1. Add `query` param to `wms_listOutcomes`

Same pattern as the `wms_listTags` search we just shipped. Add a `query` string
parameter that does case-insensitive substring search on `title` and
`description`. Composes with existing filters:

```
wms_listOutcomes(status="open", query="tag scoping")
→ open outcomes whose title or description contains "tag scoping"
```

### 2. Update `/start` flow to search-before-create

After gathering the focus slug, before creating an Outcome:
1. Search for open outcomes matching the focus keywords
2. If matches: propose them ("are you continuing one of these?") alongside "new work"
3. If no matches: confirm it's new work, proceed to create

---

## File-by-File Change List

### Store interface: `src/internal/wms/wms.go` (line 178)

Change `ListOutcomes` signature to add `query string`:

```go
// Before:
ListOutcomes(ctx context.Context, parentOutcomeID string, tagFilters map[string]string, statusFilter string) ([]*Outcome, error)
// After:
ListOutcomes(ctx context.Context, parentOutcomeID string, tagFilters map[string]string, statusFilter string, query string) ([]*Outcome, error)
```

### Store implementation: `src/internal/store/mysql/store_v2.go` (line 57)

Update `ListOutcomes` to accept `query string` and add LIKE filter:

```go
func (s *Store) ListOutcomes(ctx context.Context, parentOutcomeID string, tagFilters map[string]string, statusFilter string, query string) ([]*wms.Outcome, error) {
    // ... existing code ...

    // Add after status filter, before tagFilters:
    if query != "" {
        esc := strings.NewReplacer("%", `\%`, "_", `\_`)
        pattern := "%" + esc.Replace(query) + "%"
        sb.WriteString(` AND (o.title LIKE ? OR o.description LIKE ?)`)
        args = append(args, pattern, pattern)
    }

    // ... rest of existing code ...
}
```

Also initialize return slice as `make([]*wms.Outcome, 0)` so empty results
serialize as `[]` not `null`.

### MCP handler: `src/internal/mcp/wms/wms.go` (line 469)

Update the `ToolListOutcomes` handler to read `query`:

```go
case ToolListOutcomes:
    tagFilters := map[string]string{}
    if tf, ok := p.Arguments["tagFilters"].(map[string]interface{}); ok {
        for k, v := range tf {
            if s, ok := v.(string); ok {
                tagFilters[k] = s
            }
        }
    }
    outcomes, err := store.ListOutcomes(ctx, strArg("parentOutcomeID"), tagFilters, strArg("status"), strArg("query"))
    // ...
```

### MCP tool definition: `src/internal/mcp/wms/wms.go` (line 1333)

Update description and add `query` to inputSchema:

```go
{
    "name":        ToolListOutcomes,
    "description": "List outcomes. Omit parentOutcomeID for root outcomes; set it to list children. Use tagFilters for AND-filtered tag lookup. Use status to filter by lifecycle state; the special value \"open\" returns non-terminal outcomes (pending, active, review, blocked). Use query for case-insensitive substring search on title and description — combine with status=\"open\" to find existing outcomes matching a focus.",
    "inputSchema": map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "parentOutcomeID": ...,
            "tagFilters":      ...,
            "status":          ...,
            "query": map[string]interface{}{
                "type":        "string",
                "description": "Case-insensitive substring search on outcome title and description. Combine with status=\"open\" to find resumable outcomes.",
            },
        },
    },
},
```

### Fake store stubs

Update `ListOutcomes` signature in any fake/mock implementations:
- `src/internal/wms/closeout_test.go` (line 52)
- Any other fakes that implement the Store/Reader interface with ListOutcomes

### Skill docs: start/solo/bootstrap

**`skel/lib/plugin/skills/start/SKILL.md`** — Step 1 (context gathering):
After getting the focus slug, add a step to search for existing open outcomes:

```
Call wms_listOutcomes(status="open", query="<keywords from focus slug>").
If matching outcomes are found, note them for the interview (Step 3).
```

Step 3 (batched interview): Add a question before mode/tags if matches found:

```
Q0 (single-select, header "Outcome"):
  question: "Found open outcomes matching your focus. Resume existing or start new?"
  options:
    - label: "<outcome-id>: <title>"  (one per match, up to 3)
    - label: "New outcome"
```

If the operator picks an existing outcome, skip Outcome creation in Step 4 —
set focus on the selected outcome instead. If new, proceed as today.

**`skel/lib/plugin/skills/solo/SKILL.md`** — Step 3 (create strategic Outcome):
Before creating, search for existing open outcomes. Propose matches. If
operator picks existing, skip to Step 5 (set focus). If new, create as today.

**`skel/lib/plugin/skills/bootstrap/SKILL.md`** — Step 5 (create strategic Outcome):
Same pattern as solo.

---

## Implementation Order

1. Store interface + implementation + handler + tool definition
2. Fake store stubs
3. Skill doc updates
4. Build + test verification

## No Migration Needed

This is a read-path change only. No new columns or tables.

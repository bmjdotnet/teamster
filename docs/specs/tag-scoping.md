# Tag Scoping — Implementation Kit

This document is the plan of record for the `wms-tag-scoping` feature. It
captures the design decisions, exact file changes, and recovery procedures so
any agent (or the operator) can pick up where the last session left off.

## Problem

`wms_listTags` returns the entire system tag dictionary — 211 entries, 54.3KB,
~12.5K tokens — on every call. It is zero-argument with no filtering. The
`wms_tagEntity` tool description says "FIRST call wms_listTags," so every
tagging session pays this cost at least once. The vocabulary will only grow.

This is not a host-specific problem — it is a design problem: the tool offers
no abstraction between "nothing" and "everything."

## Design: Three Interlocking Changes

### 1. Two-tier key manifest + drill-down + search

`wms_listTags` gains two optional parameters: `tagKey` and `query`.

| `tagKey` | `query` | Response |
|----------|---------|----------|
| absent   | absent  | **Key manifest** — one `TagKeySummary` per key (~18 entries, ~3KB) |
| present  | absent  | **Drill-down** — all values for that key (`[]Tag`) |
| absent   | present | **Search all** — matching values across all keys (`[]Tag`) |
| present  | present | **Search within key** — matching values in one key (`[]Tag`) |

**Key manifest entry shape:**

```go
type TagKeySummary struct {
    Key            string   `json:"tag_key"`
    Category       string   `json:"category"`
    Cardinality    string   `json:"cardinality"`
    Required       bool     `json:"required"`
    Description    string   `json:"description"`
    ValueCount     int      `json:"value_count"`
    Values         []string `json:"values,omitempty"`
    Scope          string   `json:"scope"`
    ExclusionGroup string   `json:"exclusion_group"`
    AutoExtract    string   `json:"auto_extract"`
    Interview      string   `json:"interview"`
}
```

**Threshold rule:** Inline all values when `value_count <= 10`; omit the
`values` field when above 10. No `sample_values` — partial lists are worse
than count-only (false dedup confidence). 13 of 18 current keys have ≤10
values, so most tagging operations complete in one call.

**Search semantics:** SQL `LIKE '%query%'` on `tag_value OR description`,
case-insensitive (MySQL/MariaDB default collation). No regex. Escape `%`
and `_` literals.

**Token savings:** ~54KB → ~3KB on initial call (95% reduction). Drill-downs
add ~200-2K each but are targeted.

### 2. Tag conventions (admin-defined expectations)

Four new columns on the `tags` table, all per-key (propagated like
`required` and `cardinality`):

| Column | Type | Default | Values | Purpose |
|--------|------|---------|--------|---------|
| `scope` | VARCHAR(16) | `''` | `outcome`, `workunit`, `''` | Where the key should be applied |
| `exclusion_group` | VARCHAR(64) | `''` | any slug | Keys sharing a group are mutually exclusive |
| `auto_extract` | VARCHAR(32) | `''` | `git`, `env`, `''` | Source for auto-extraction (skip interview) |
| `interview` | VARCHAR(16) | `'propose'` | `propose`, `auto`, `skip` | How key behaves in the tag interview |

**Why the DB, not config:** Conventions are 1:1 with keys. They belong where
the vocabulary lives — the `tags` table. The TUI wizard already writes there.
A separate table or yaml section would split the admin surface for no benefit.

**Advisory, not enforced:** `scope=workunit` doesn't prevent `TagEntity` from
applying the key to an outcome — it tells skills not to propose it there. The
engine stays simple; skills get smarter by reading structured data instead of
prose. Future `TEAMSTER_ENFORCE_CONVENTIONS` can add hard enforcement later.

**Default seeds for shipped vocabulary:**

```
scope=outcome:    product, priority, product-version, feature, bug
scope=workunit:   component, phase, work-type, resolution
exclusion_group:  feature + bug → "work-scope"
auto_extract=git: github.*, gitlab.*, git.*, jira.*, linear.*
interview=propose: product, feature, bug, priority, product-version
interview=auto:    github.*, gitlab.*, git.*
interview=skip:    phase, work-type, resolution, lifecycle, component, user, source
```

### 3. Skill doc impact (future phase)

Once conventions ship, the bootstrap/start/solo skills can read them from
`wms_listTags` instead of hardcoding rules. The interview flow becomes:
- `interview=propose` → include in operator confirmation prompt
- `interview=auto` → extract and apply silently
- `interview=skip` → don't mention
- Group by `exclusion_group` → propose at most one from each group
- Respect `scope` at application time

This is **phase 3** — skill rewrites happen after the API ships and is
validated on the primary host.

---

## File-by-File Change List

All paths relative to `src/`.

### Migration: `internal/store/mysql/migrations.go`

**Add migration v44** after v43 (line 1132). Four `ALTER TABLE` statements
and `UPDATE` seeds:

```go
{
    Version: 44,
    Name:    "tag-conventions",
    Stmts: []string{
        `ALTER TABLE tags ADD COLUMN scope VARCHAR(16) NOT NULL DEFAULT ''`,
        `ALTER TABLE tags ADD COLUMN exclusion_group VARCHAR(64) NOT NULL DEFAULT ''`,
        `ALTER TABLE tags ADD COLUMN auto_extract VARCHAR(32) NOT NULL DEFAULT ''`,
        `ALTER TABLE tags ADD COLUMN interview VARCHAR(16) NOT NULL DEFAULT 'propose'`,
        // Seed shipped defaults
        `UPDATE tags SET scope = 'outcome' WHERE tag_key IN ('product','priority','product-version','feature','bug')`,
        `UPDATE tags SET scope = 'workunit' WHERE tag_key IN ('component','phase','work-type','resolution')`,
        `UPDATE tags SET exclusion_group = 'work-scope' WHERE tag_key IN ('feature','bug')`,
        `UPDATE tags SET auto_extract = 'git' WHERE tag_key LIKE 'github.%' OR tag_key LIKE 'gitlab.%' OR tag_key LIKE 'git.%' OR tag_key LIKE 'jira.%' OR tag_key LIKE 'linear.%'`,
        `UPDATE tags SET interview = 'auto' WHERE tag_key LIKE 'github.%' OR tag_key LIKE 'gitlab.%' OR tag_key LIKE 'git.%'`,
        `UPDATE tags SET interview = 'skip' WHERE tag_key IN ('phase','work-type','resolution','lifecycle','component','user','source')`,
    },
},
```

### Types: `internal/wms/wms.go`

**Tag struct** (line 80) — add 4 fields after `Required`:

```go
Scope          string `json:"scope"`
ExclusionGroup string `json:"exclusion_group"`
AutoExtract    string `json:"auto_extract"`
Interview      string `json:"interview"`
```

**TagSpec struct** (line 94) — add 4 pointer fields after `Required`:

```go
Scope          *string
ExclusionGroup *string
AutoExtract    *string
Interview      *string
```

**TagKeySummary struct** — add new type after `TagSpec`:

```go
type TagKeySummary struct {
    Key            string   `json:"tag_key"`
    Category       string   `json:"category"`
    Cardinality    string   `json:"cardinality"`
    Required       bool     `json:"required"`
    Description    string   `json:"description"`
    ValueCount     int      `json:"value_count"`
    Values         []string `json:"values,omitempty"`
    Scope          string   `json:"scope"`
    ExclusionGroup string   `json:"exclusion_group"`
    AutoExtract    string   `json:"auto_extract"`
    Interview      string   `json:"interview"`
}
```

**Reader interface** (line 126) — add method:

```go
SearchTags(ctx context.Context, tagKey, query string) ([]Tag, error)
```

### Store: `internal/store/mysql/store.go`

**ListTags** (line 881) — extend SELECT to include 4 new columns. Extend
the `Scan` call to read them.

**SearchTags** — new method. Parameterized query with optional `tag_key = ?`
and `(tag_value LIKE ? OR description LIKE ?)` filters. Always includes
`retired = 0`. Escape `%` and `_` in the query pattern.

```go
func (s *Store) SearchTags(ctx context.Context, tagKey, query string) ([]wms.Tag, error) {
    q := `SELECT tag_key, tag_value, is_seed, category, cardinality, description,
                 retired, required, scope, exclusion_group, auto_extract, interview
          FROM tags WHERE retired = 0`
    var args []interface{}
    if tagKey != "" {
        q += ` AND tag_key = ?`
        args = append(args, tagKey)
    }
    if query != "" {
        q += ` AND (tag_value LIKE ? OR description LIKE ?)`
        esc := strings.NewReplacer("%", `\%`, "_", `\_`)
        pattern := "%" + esc.Replace(query) + "%"
        args = append(args, pattern, pattern)
    }
    q += ` ORDER BY tag_key, tag_value`
    // ... same scan loop as ListTags ...
}
```

**DefineTag** (line 1118) — extend to propagate `scope`, `exclusion_group`,
`auto_extract`, `interview` per-key (same UPDATE pattern as `required`).

**ReconcileVocabulary** (line 1059) — carry convention columns from
`TagSpec` when reconciling from yaml config.

### MCP handler: `internal/mcp/wms/wms.go`

**Tool definition for `ToolListTags`** (line 1141) — replace:

```go
{
    "name":        ToolListTags,
    "description": "Discover the tag vocabulary. Default (no args): returns a compact KEY MANIFEST — one entry per tag_key with {tag_key, category, cardinality, required, description, value_count, values (when ≤10), scope, exclusion_group, auto_extract, interview}. Scan the manifest to orient, then drill down. With tagKey: returns ALL values for that key. With query: case-insensitive substring search across tag_value and description. Both compose: tagKey + query filters within one key.",
    "inputSchema": map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "tagKey": map[string]interface{}{
                "type":        "string",
                "description": "Drill into one key's values instead of the key manifest.",
            },
            "query": map[string]interface{}{
                "type":        "string",
                "description": "Case-insensitive substring search across tag_value and description. Use to find tags matching a concept or check for duplicates before creating.",
            },
        },
    },
}
```

**Tool definition for `ToolTagEntity`** (line 1127) — update description:

```
"FIRST call wms_listTags to see the key manifest; then call
wms_listTags(tagKey=<key>) for the key you intend to tag (unless the
manifest already includes its values). Reuse an existing (tagKey, tagValue)
rather than inventing near-duplicates."
```

**Handler case** (line 310) — rewrite:

```go
case ToolListTags, "wms.listTags":
    tagKey := strArg("tagKey")
    query := strArg("query")

    if tagKey == "" && query == "" {
        // Manifest mode
        tags, err := store.ListTags(ctx)
        if err != nil {
            return Result{}, &CallError{Code: -32000, Message: err.Error()}
        }
        manifest := buildTagManifest(tags) // group, count, threshold
        return JSONResult(manifest), nil
    }

    // Drill-down and/or search
    tags, err := store.SearchTags(ctx, tagKey, query)
    if err != nil {
        return Result{}, &CallError{Code: -32000, Message: err.Error()}
    }
    return JSONResult(tags), nil
```

**buildTagManifest helper** — new function in wms.go:

```go
func buildTagManifest(tags []wms.Tag) []wms.TagKeySummary {
    const inlineThreshold = 10
    type keyGroup struct {
        first  wms.Tag
        values []string
    }
    groups := make(map[string]*keyGroup)
    order := []string{}
    for _, t := range tags {
        if t.Retired { continue }
        g, ok := groups[t.Key]
        if !ok {
            g = &keyGroup{first: t}
            groups[t.Key] = g
            order = append(order, t.Key)
        }
        if t.Value != "" {
            g.values = append(g.values, t.Value)
        }
    }
    out := make([]wms.TagKeySummary, 0, len(order))
    for _, key := range order {
        g := groups[key]
        s := wms.TagKeySummary{
            Key:            key,
            Category:       g.first.Category,
            Cardinality:    g.first.Cardinality,
            Required:       g.first.Required,
            Description:    g.first.Description,
            ValueCount:     len(g.values),
            Scope:          g.first.Scope,
            ExclusionGroup: g.first.ExclusionGroup,
            AutoExtract:    g.first.AutoExtract,
            Interview:      g.first.Interview,
        }
        if len(g.values) <= inlineThreshold {
            s.Values = g.values
        }
        out = append(out, s)
    }
    return out
}
```

### DefineTag MCP extension: `internal/mcp/wms/wms.go`

Add four optional parameters to `ToolDefineTag` input schema (line 1150):

```go
"scope":          map[string]interface{}{"type": "string", "description": "'outcome' | 'workunit' | '' (default: unchanged)"},
"exclusionGroup": map[string]interface{}{"type": "string", "description": "Mutual exclusion group slug. Keys sharing a group are exclusive on an entity."},
"autoExtract":    map[string]interface{}{"type": "string", "description": "'git' | 'env' | '' (default: unchanged)"},
"interview":      map[string]interface{}{"type": "string", "description": "'propose' | 'auto' | 'skip' (default: unchanged)"},
```

Wire them into the `TagSpec` in the handler (line 323).

### Config: `internal/config/config.go`

Extend `TagConfig` struct (around line 434) with optional convention fields
so `teamster.yaml` can declare them:

```yaml
tags:
  component:
    category: context
    cardinality: single
    scope: workunit
    interview: skip
    description: "Subsystem within a product"
```

### Store integration tests

Add test for `SearchTags` with:
- tagKey only (drill-down)
- query only (cross-key search)
- tagKey + query (filtered search)
- empty results
- LIKE metacharacter escaping

---

## Implementation Order

All work is in worktree `tag-scoping` (branch `wt/tag-scoping`). Do not
fold to main.

### Step 1: Migration + types
- Add v44 migration
- Extend `wms.Tag`, `wms.TagSpec`, add `wms.TagKeySummary`
- Extend `Reader` interface
- `go build ./...` must pass

### Step 2: Store layer
- Extend `ListTags` SELECT + Scan for new columns
- Add `SearchTags` method
- Extend `DefineTag` to propagate convention columns
- Extend `ReconcileVocabulary`
- `go test ./...` must pass

### Step 3: MCP handler
- Rewrite `ToolListTags` handler (manifest + drill-down + search)
- Add `buildTagManifest` helper
- Update `ToolListTags` tool definition (description + inputSchema)
- Update `ToolTagEntity` description
- Extend `ToolDefineTag` handler + schema
- `go build ./...` must pass

### Step 4: Config
- Extend `TagConfig` for convention fields
- Wire through `TagSpecs()` to `ReconcileVocabulary`

### Step 5: Verify
- Full `go build ./... && go test ./... && go vet ./...`
- Manual smoke test: build wms-mcp, run against test DB, call listTags
  with no args / with tagKey / with query / with both

---

## Testing Strategy

**Unit/integration tests** (require `TEAMSTER_TEST_MYSQL_DSN`):
- Migration v44 applies cleanly on fresh DB and on existing v43 DB
- `ListTags` returns new columns
- `SearchTags` filters correctly (tagKey, query, both, neither)
- `SearchTags` escapes LIKE metacharacters
- `DefineTag` propagates convention columns per-key
- `ReconcileVocabulary` carries convention columns from config
- `buildTagManifest` groups correctly, respects threshold, handles
  empty values, preserves key order

**Smoke test** (manual, against running wms-mcp):
- `wms_listTags()` → returns manifest, not flat list
- `wms_listTags(tagKey="feature")` → returns 52 feature values
- `wms_listTags(query="wms")` → returns matching tags across keys
- `wms_listTags(tagKey="feature", query="wms")` → returns feature tags matching "wms"
- Convention fields visible in manifest and drill-down responses

---

## Recovery Procedures

**If migration v44 fails mid-apply:**
The migration system uses an advisory lock. If it fails partway through
the ALTER statements, the `schema_version` table won't advance to 44.
Restarting wms-mcp will retry. If ALTERs partially applied, the column
guards in the migration system (`information_schema` checks) mean
already-applied ALTERs are safe to retry (idempotent ADD COLUMN is
guarded). The UPDATE seeds are also idempotent.

**If convention seeds are wrong:**
All convention columns are mutable via `wms_defineTag` and the TUI. An
admin can correct any seed value without a migration. The seeds are
reasonable defaults, not hard constraints.

**Rolling back:**
Convention columns have no-op defaults (`scope=''`, `interview='propose'`).
If the feature is reverted, old code ignores the extra columns (they're
not in the old SELECT list). The migration is forward-only but the columns
are harmless to old code.

**Context recovery for future sessions:**
This document is the recovery artifact. Any session can read it, check
`git log` in the worktree for progress, and continue from where work
stopped. The WMS Outcome `wms-tag-scoping` tracks the strategic context.

---

## Decisions Log

| Decision | Rationale |
|----------|-----------|
| No `expand=true` param | Agents have no memory of old shape; providing it defeats purpose |
| No `category` filter | 18 keys is already tiny; `interview` field is better scoping |
| No `sample_values` | Partial lists create false dedup confidence; all-or-none via threshold |
| Threshold = 10 | Captures 13/18 keys; server-side constant, not user-facing |
| `query` on same tool, not new tool | WMS already has 20+ tools; search is a mode of listing |
| SQL LIKE not REGEXP | Simpler, no injection risk, sufficient for scale |
| Conventions in DB not config | Belongs where vocabulary lives; TUI already writes there |
| Advisory not enforced | Engine stays simple; skills read conventions as guidance |
| `SearchTags` subsumes `ListTagsByKey` | One method handles drill-down + search + combined |
| Convention columns per-key not per-value | Same propagation pattern as `required` and `cardinality` |

## Open Items (deferred)

- [ ] TUI wizard conventions screen (~230 lines Bubbletea) — phase 2
- [ ] Skill doc rewrites (bootstrap/start/solo) to be data-driven — phase 3
- [ ] `TEAMSTER_ENFORCE_CONVENTIONS` hard enforcement flag — future
- [ ] `teamster.yaml` convention fields in TagConfig — step 4 of this phase
- [ ] `semantic-conventions.md` documentation update — after API ships

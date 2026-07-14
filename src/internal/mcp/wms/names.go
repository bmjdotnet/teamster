package wms

// Canonical wms-mcp tool name constants. D1's hookd dispatch branches import
// these so any rename here is a compile-time break rather than a silent
// metric/observability regression. See SPEC §9 D2 documentation
// responsibility.
//
// Only the snake_case forms are exposed; the dotted aliases ("wms.createX")
// remain handled by HandleToolCall's case list for backwards compatibility
// with older clients but are not part of the public dispatch contract.
const (
	ToolGetHistory   = "wms_getHistory"
	ToolUpdateStatus = "wms_updateStatus"
	ToolGetTimeline  = "wms_getTimeline"
	ToolSetFocus     = "wms_setFocus"
	ToolTagEntity    = "wms_tagEntity"
	ToolListTags     = "wms_listTags"
	ToolDefineTag    = "wms_defineTag"
	ToolRetireTag    = "wms_retireTag"
	ToolSetPhase     = "wms_setPhase"

	// Tag steward: refine an existing tag value's description (the classification
	// rubric) in place — works on lifecycle keys defineTag/tagEntity won't touch.
	ToolDescribeTag = "wms_describeTag"

	// Tag steward: surgically remove one entity's tag binding(s) for a key
	// (single value, or all values when tagValue is omitted), snapshotting first
	// so the removal is reversible. Distinct from the value-wide destructive
	// `tags delete-value` CLI.
	ToolUntagEntity = "wms_untagEntity"

	// Tag steward rollback plumbing (W5). Snapshot captures pre-change tag
	// bindings to a JSONL file under $TEAMSTER_BASEDIR/var/tag-steward/;
	// rollback reverts steward-sourced bindings from that snapshot.
	ToolSnapshotEntityTags = "wms_snapshotEntityTags"
	ToolRollbackTags       = "wms_rollbackTags"

	// ToolGetEntityTags reads the tags bound to one entity: direct bindings plus
	// (for a workunit) tags inherited from its parent outcome. Read-only —
	// distinct from wms_getOutcome/wms_getWorkUnit, whose response shapes this
	// tool must not change.
	ToolGetEntityTags = "wms_getEntityTags"
)

// v2 tool name constants
const (
	ToolCreateOutcome        = "wms_createOutcome"
	ToolGetOutcome           = "wms_getOutcome"
	ToolListOutcomes         = "wms_listOutcomes"
	ToolUpdateOutcomeStatus  = "wms_updateOutcomeStatus"
	ToolCreateWorkUnit       = "wms_createWorkUnit"
	ToolGetWorkUnit          = "wms_getWorkUnit"
	ToolListWorkUnits        = "wms_listWorkUnits"
	ToolUpdateWorkUnitStatus = "wms_updateWorkUnitStatus"
	ToolAssignWorkUnit       = "wms_assignWorkUnit"
	ToolClaimWorkUnit        = "wms_claimWorkUnit"
	ToolClassifyEntity       = "wms_classifyEntity"
	ToolListRelated          = "wms_listRelated"
	ToolSearch               = "wms_search"
	ToolRenameOutcome        = "wms_renameOutcome"
	ToolRenameWorkUnit       = "wms_renameWorkUnit"
)

// v2 MCP wire names
const (
	MCPToolCreateOutcome        = "mcp__wms__" + ToolCreateOutcome
	MCPToolGetOutcome           = "mcp__wms__" + ToolGetOutcome
	MCPToolListOutcomes         = "mcp__wms__" + ToolListOutcomes
	MCPToolUpdateOutcomeStatus  = "mcp__wms__" + ToolUpdateOutcomeStatus
	MCPToolCreateWorkUnit       = "mcp__wms__" + ToolCreateWorkUnit
	MCPToolGetWorkUnit          = "mcp__wms__" + ToolGetWorkUnit
	MCPToolListWorkUnits        = "mcp__wms__" + ToolListWorkUnits
	MCPToolUpdateWorkUnitStatus = "mcp__wms__" + ToolUpdateWorkUnitStatus
	MCPToolAssignWorkUnit       = "mcp__wms__" + ToolAssignWorkUnit
	MCPToolClaimWorkUnit        = "mcp__wms__" + ToolClaimWorkUnit
	MCPToolClassifyEntity       = "mcp__wms__" + ToolClassifyEntity
)

// MCP wire names — the form Claude Code emits in PreToolUse `tool_name`
// for MCP-served tools is `mcp__<server>__<tool>`. Hook-side dispatch on
// hookd compares against these directly because the wire payload carries
// the prefixed form, not the bare tool name. Keeping both forms here keeps
// any rename a compile-time break per the SPEC §9 D2 contract.
const (
	MCPToolUpdateStatus = "mcp__wms__" + ToolUpdateStatus
	MCPToolSetFocus     = "mcp__wms__" + ToolSetFocus
	MCPToolTagEntity    = "mcp__wms__" + ToolTagEntity
)

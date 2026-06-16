package mysql

// Transition rules — opt-in role enforcement
//
// By default the transition_rules table is empty, and RoleAllowed returns true
// for every input. This preserves backward compatibility: all agents may make
// any structurally valid transition (ValidTransition still applies).
//
// Operators opt in by inserting rows. Once any row exists, only roles that
// match an explicit row (or the wildcard '*') may perform that transition.
//
// Example SQL:
//
//	-- Only 'lead' role may mark tasks complete.
//	INSERT INTO transition_rules (entity_type, old_status, new_status, required_role)
//	VALUES ('task', 'review', 'complete', 'lead');
//
//	-- Allow everyone to activate a task (wildcard).
//	INSERT INTO transition_rules (entity_type, old_status, new_status, required_role)
//	VALUES ('task', 'pending', 'active', '*');
//
//	-- Multiple roles on one transition: insert one row per allowed role.
//	INSERT INTO transition_rules (entity_type, old_status, new_status, required_role)
//	VALUES ('task', 'review', 'complete', 'opus');
//
// Note: a transition not covered by any row in a non-empty table is denied.
// Add a wildcard row ('*') to explicitly allow all roles for that transition.

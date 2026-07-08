package codexconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// hookEventSnake maps a PascalCase Codex hook event name (the config.toml
// spelling, e.g. "SessionStart") to the snake_case label
// hook_event_key_label() (codex-rs/hooks/src/lib.rs, tag rust-v0.137.0,
// commit f221438b691b8f749d98f22077c93ebe01923fbe) uses for both the
// trust-hash's "event_name" field and the state-key's event segment. Ported
// by direct source read, not guessed from a casing rule — the PascalCase
// wire enum and this snake_case label are two independently-defined tables
// that happen to agree, not one derived from the other.
func hookEventSnake(event string) (string, error) {
	switch event {
	case "PreToolUse":
		return "pre_tool_use", nil
	case "PermissionRequest":
		return "permission_request", nil
	case "PostToolUse":
		return "post_tool_use", nil
	case "PreCompact":
		return "pre_compact", nil
	case "PostCompact":
		return "post_compact", nil
	case "SessionStart":
		return "session_start", nil
	case "UserPromptSubmit":
		return "user_prompt_submit", nil
	case "SubagentStart":
		return "subagent_start", nil
	case "SubagentStop":
		return "subagent_stop", nil
	case "Stop":
		return "stop", nil
	default:
		return "", fmt.Errorf("codexconfig: unknown hook event %q", event)
	}
}

// HookDefinition is one hook handler as Codex's trust-hash algorithm sees
// it — see TrustedHash's doc comment for the full derivation.
type HookDefinition struct {
	// Event is the PascalCase config event name (e.g. "SessionStart").
	Event string
	// Matcher is the group's `matcher = "..."` value. Empty means the
	// matcher key was omitted from config.toml entirely (Codex's own
	// matcher_pattern_for_event treats an absent matcher as "match
	// everything" for SessionStart/PreToolUse/PostToolUse — see
	// codex-rs/hooks/src/events/common.rs — so omitting it is a legitimate
	// registration choice, not a workaround). Must be "" iff no `matcher`
	// line was written; a written-but-empty `matcher = ""` is a different,
	// unsupported case this package never produces.
	Matcher string
	// Command is the hook command exactly as written in config.toml,
	// BEFORE Codex's ${VAR} env substitution (irrelevant for Teamster's own
	// hooks, which never use that syntax — always a literal absolute path).
	Command string
	// TimeoutSec is the resolved `timeout` value in seconds. 0 means "no
	// `timeout` key was written" — TrustedHash applies Codex's own default
	// (600) in that case. This is NOT the same as writing `timeout = 0`
	// explicitly, which Codex clamps to 1 rather than defaulting to 600;
	// Teamster's own writer (hooks.go) always writes an explicit non-zero
	// timeout, so that distinction never actually matters in practice.
	TimeoutSec int
}

// hookHandlerCanonical and hookIdentityCanonical mirror, field for field and
// IN ALPHABETICAL FIELD-NAME ORDER, the JSON shape codex-rs's
// version_for_toml (codex-rs/config/src/fingerprint.rs) produces after its
// canonical_json pass, which recursively sorts every JSON object's keys.
// encoding/json does not sort struct fields — it emits them in declaration
// order — so this field order is itself load-bearing and must never be
// edited without re-verifying against canonical_json's own sort.
//
// command_windows and status_message (Codex's other two Option fields on a
// command hook handler) never appear here at all, rather than as
// always-empty fields: the toml crate (toml-v0.9.11, confirmed from its own
// source — crates/toml/src/ser/value/map.rs's SerializeTable/SerializeMap
// intercept the unsupported_none() error and skip the field) silently OMITS
// a struct field whenever its value is Option::None, instead of emitting
// null or erroring. Since Teamster's own hooks never set either field, they
// are correctly modeled as absent, not as zero-valued.
type hookHandlerCanonical struct {
	Async   bool   `json:"async"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
	Type    string `json:"type"`
}

type hookIdentityCanonical struct {
	EventName string                 `json:"event_name"`
	Hooks     []hookHandlerCanonical `json:"hooks"`
	Matcher   *string                `json:"matcher,omitempty"`
}

// TrustedHash computes the sha256 trust hash Codex 0.137.0 derives for one
// hook handler — the exact value that belongs in config.toml's
// [hooks.state."<path>:<event_snake>:<group>:<handler>"] trusted_hash key
// (see HookStateKey for the key itself).
//
// Ported by direct source read of openai/codex, tag rust-v0.137.0, commit
// f221438b691b8f749d98f22077c93ebe01923fbe (the tag matching the kit's
// pinned Codex CLI version):
//
//   - codex-rs/hooks/src/engine/discovery.rs command_hook_hash: builds a
//     NormalizedHookIdentity{event_name: <snake label>, matcher, hooks: [the
//     ONE handler being hashed]} — always a single-element hooks array, even
//     when the real [[hooks.Event]] group has more handlers, because each
//     handler is hashed independently against a synthetic single-handler
//     group (group.hooks is overwritten with vec![normalized_handler] before
//     hashing). timeout_sec is normalized to unwrap_or(600).max(1) BEFORE
//     hashing. command is hashed BEFORE Codex's ${VAR} env substitution.
//   - codex-rs/config/src/fingerprint.rs version_for_toml: converts the
//     identity to a toml::Value, then to serde_json::Value via
//     serde_json::to_value, recursively sorts every object's keys
//     (canonical_json), serializes to JSON bytes via serde_json::to_vec,
//     sha256s them, and formats "sha256:<hex>".
//
// Deliberately does NOT take the config file's path as an input. Read
// hook_key() (codex-rs/hooks/src/lib.rs) directly: the absolute config path
// is embedded ONLY in the trust-state TABLE NAME used to look a hash up
// (HookStateKey), never in the hash computation itself. An earlier research
// pass (teamster-codex-kit round 3) reported the opposite — identical
// definitions at different config paths producing different hashes — but
// reconstructing that comparison's own two specimens shows they actually
// used different hook *commands* (different script paths, and in one case a
// different timeout), not just different config locations: recomputing
// round 3's own "differing" hash using round 3's own command path (rather
// than round 2's) reproduces it exactly. WP8's operational rule — provision
// trust at the FINAL install path, never copy state blocks between paths —
// still holds, just for the state-key-lookup reason (a block written under
// one path's key is never looked up by a config at a different path), not
// because the hash value itself is path-sensitive.
//
// Validated two ways (see hooktrust_test.go and this task's final report):
// (1) reproduces, byte for byte, all six trusted_hash values already
// captured in the kit's evidence — two independent config paths, both a
// pre- and post- hook-definition-change state; (2) a fresh live TUI trust
// bootstrap in an isolated CODEX_HOME, at two more controlled paths, whose
// resulting trusted_hash this function's output matches exactly.
func TrustedHash(def HookDefinition) (string, error) {
	snake, err := hookEventSnake(def.Event)
	if err != nil {
		return "", err
	}

	timeout := def.TimeoutSec
	if timeout == 0 {
		timeout = 600
	}
	if timeout < 1 {
		timeout = 1
	}

	identity := hookIdentityCanonical{
		EventName: snake,
		Hooks: []hookHandlerCanonical{{
			Async:   false,
			Command: def.Command,
			Timeout: timeout,
			Type:    "command",
		}},
	}
	if def.Matcher != "" {
		m := def.Matcher
		identity.Matcher = &m
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// serde_json never HTML-escapes '<'/'>'/'&'; Go's encoding/json does by
	// default. Teamster's own hook commands are plain absolute paths that
	// never contain these characters, but disabling HTML escaping keeps this
	// an exact port rather than an incidental match.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(identity); err != nil {
		return "", fmt.Errorf("codexconfig: encode hook identity: %w", err)
	}
	// json.Encoder.Encode appends a trailing newline that serde_json::to_vec
	// never produces; strip it so the hashed bytes match exactly.
	encoded := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))

	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// HookStateKey builds the [hooks.state."..."] table name Codex looks a
// hook's trust decision up under: "<abs config path>:<event_snake>:<group
// index>:<handler index>" (codex-rs/hooks/src/lib.rs hook_key, fed
// source_path.display().to_string() as key_source — the config.toml's own
// absolute path exactly as Codex resolves it, e.g. via
// codexconfig.DefaultConfigPath, not a canonicalized or symlink-resolved
// variant). configPath must be the FINAL install-time path: a key built for
// any other path will never match what a running Codex looks up against its
// own config file.
//
// groupIndex and handlerIndex are 0-based positions of the matcher group and
// handler within it. Teamster only ever writes one [[hooks.<Event>]] group
// with one [[hooks.<Event>.hooks]] handler per event, so both are always 0
// for every hook this package registers.
func HookStateKey(configPath, event string, groupIndex, handlerIndex int) (string, error) {
	snake, err := hookEventSnake(event)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s:%d:%d", configPath, snake, groupIndex, handlerIndex), nil
}

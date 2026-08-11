// Package intercept provides the externalized MCP tool-call enrichment
// registry: a YAML-driven set of match rules that produce the display tag,
// display text, and special fields (_focus, _thought, ...) hookd/the hook
// client attach to a PreToolUse event for mcp__* tools.
package intercept

import (
	_ "embed"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// embeddedDefaultYAML is a checked-in copy of skel/etc/interceptors.yaml.
// go:embed patterns cannot ascend out of the source file's directory (skel/
// sits outside the src/ module tree), so this file must live here instead of
// being embedded directly from skel/. TestEmbeddedConfigMatchesSkelSource in
// registry_test.go fails loudly if the two drift out of sync.
//
//go:embed interceptors.yaml
var embeddedDefaultYAML []byte

// TagConfig is the YAML shape of one entry under the top-level "tags" map.
type TagConfig struct {
	Color       [3]int `yaml:"color"`
	Description string `yaml:"description"`
}

// MatchConfig is the YAML shape of a "match" block, used at both namespace
// and rule level. See MatchSpec (registry.go) for compiled-match semantics.
type MatchConfig struct {
	Prefix string            `yaml:"prefix,omitempty"`
	Method string            `yaml:"method,omitempty"`
	Suffix string            `yaml:"suffix,omitempty"`
	Exact  string            `yaml:"exact,omitempty"`
	Regex  string            `yaml:"regex,omitempty"`
	Params map[string]string `yaml:"params,omitempty"`
}

// RuleConfig is one entry under an interceptor's "rules" list.
type RuleConfig struct {
	Match    MatchConfig       `yaml:"match"`
	Tag      string            `yaml:"tag,omitempty"`
	Display  string            `yaml:"display,omitempty"`
	Fields   map[string]string `yaml:"fields,omitempty"`
	Suppress bool              `yaml:"suppress,omitempty"`
}

// InterceptorConfig is one top-level entry under "interceptors" — a
// namespace match plus either nested rules, a direct display/fields/suppress
// outcome (used when there are no rules, e.g. mcp__health__), or both (the
// direct outcome then acts as the namespace's no-rule-matched fallback).
type InterceptorConfig struct {
	Match    MatchConfig       `yaml:"match"`
	Tag      string            `yaml:"tag,omitempty"`
	Display  string            `yaml:"display,omitempty"`
	Fields   map[string]string `yaml:"fields,omitempty"`
	Suppress bool              `yaml:"suppress,omitempty"`
	Rules    []RuleConfig      `yaml:"rules,omitempty"`
}

// ConfigFile is the root YAML document shape.
type ConfigFile struct {
	Tags         map[string]TagConfig `yaml:"tags"`
	Interceptors []InterceptorConfig  `yaml:"interceptors"`
}

// LoadDefault builds a Registry from the embedded default config compiled
// into the binary. This never fails in a release build — TestEmbeddedDefaultIsValid
// (registry_test.go) is the build-time gate that enforces it.
func LoadDefault() (*Registry, error) {
	return buildFromYAML(embeddedDefaultYAML)
}

// Load builds a Registry from the YAML file at path, with no embedded
// fallback. Used by callers that want a hard failure on a bad file (e.g. a
// future `teamster check-config` CLI verb).
func Load(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("intercept: read %s: %w", path, err)
	}
	return buildFromYAML(data)
}

// LoadWithOverlay loads the embedded default, then attempts to replace it
// entirely with path's contents if the file exists and parses (operator
// ruling I4: the user's file is the full config, not a patch — there is no
// merge). If path is absent, or present but malformed, the embedded default
// is returned together with a non-nil error describing why the overlay
// didn't apply; the caller decides how loudly to warn. A missing file is not
// treated as an error worth surfacing loudly — most installs never customize
// this file.
func LoadWithOverlay(path string) (*Registry, error) {
	def, err := LoadDefault()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil
		}
		return def, fmt.Errorf("intercept: read %s: %w", path, err)
	}
	overlay, err := buildFromYAML(data)
	if err != nil {
		return def, fmt.Errorf("intercept: %s invalid, using shipped defaults: %w", path, err)
	}
	return overlay, nil
}

// buildFromYAML parses YAML bytes into a ConfigFile, validates it, and
// compiles it into a Registry.
func buildFromYAML(data []byte) (*Registry, error) {
	var cfg ConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("intercept: parse yaml: %w", err)
	}
	return buildRegistry(&cfg)
}

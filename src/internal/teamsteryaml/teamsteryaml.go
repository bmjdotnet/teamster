// Package teamsteryaml is the shared schema for teamster.yaml — the
// service-topology receipt teamster-install writes at install time and
// internal/clonetopology reads at clone time. Extracted from
// cmd/teamster-install/yaml_config.go (WP2) so both sides compile against
// the same real types instead of a hand-duplicated, drift-prone copy.
package teamsteryaml

// Hookd is the hookd service's teamster.yaml section.
type Hookd struct {
	Mode string `yaml:"mode"`
	Port int    `yaml:"port"`
}

// Store is the MySQL/MariaDB store's teamster.yaml section.
type Store struct {
	Mode string `yaml:"mode"`
	DSN  string `yaml:"dsn"`
}

// Service is the shared shape for prometheus/grafana's teamster.yaml sections.
type Service struct {
	Mode   string `yaml:"mode"`
	Port   int    `yaml:"port"`
	Health string `yaml:"health,omitempty"`
}

// Otelcol is the otelcol service's teamster.yaml section.
type Otelcol struct {
	Mode     string `yaml:"mode"`
	GRPCPort int    `yaml:"grpc_port"`
	HTTPPort int    `yaml:"http_port"`
	// CodexHTTPPort is the dedicated otlp/http receiver instance Codex's
	// [otel] export points at — never shared with HTTPPort, see
	// internal/codexconfig/otel.go's OtelSpec.MetricsEndpoint doc comment.
	CodexHTTPPort int `yaml:"codex_http_port"`
}

// TokenScraper is the token-scraper's teamster.yaml section.
type TokenScraper struct {
	Mode string `yaml:"mode"`
}

// Relay is the hub→replica relay's teamster.yaml section — dropped
// unconditionally by clonetopology.Translate (R1/I2), never remapped.
type Relay struct {
	Mode           string `yaml:"mode"`
	Target         string `yaml:"target,omitempty"`
	ReplPushRemote string `yaml:"repl_push_remote,omitempty"`
}

// Clone is the `teamster clone`'s teamster.yaml section — I5's forbidden
// basedir prefixes, operator-configured rather than hardcoded, since which
// mounts are shared between a source and its clone targets is a deployment
// fact, not a constant.
type Clone struct {
	ForbiddenBasedirs []string `yaml:"forbidden_basedirs,omitempty"`
}

// TagConfig declares one key in the work-item tag vocabulary. Field names
// and yaml tags MUST stay identical to config.TagConfig
// (src/internal/config/yaml.go) or the installer round-trip drifts lossy:
// the runtime read-side reconciles from config.TagConfig, the installer
// preserves it through this struct. Keep the two in lock-step.
type TagConfig struct {
	Category       string   `yaml:"category"`    // "context" | "lifecycle"
	Cardinality    string   `yaml:"cardinality"` // "single" | "multi"
	Values         []string `yaml:"values"`      // explicit value list; empty for create-on-apply keys
	Description    string   `yaml:"description"`
	Scope          string   `yaml:"scope"`           // "outcome" | "workunit" | ""
	ExclusionGroup string   `yaml:"exclusion_group"` // mutual exclusion group slug
	AutoExtract    string   `yaml:"auto_extract"`    // "git" | "env" | ""
	Interview      string   `yaml:"interview"`       // "propose" | "auto" | "skip"
}

// Config is the full teamster.yaml schema — the source instance's
// service-topology receipt clonetopology.Translate reads to produce a
// target flag set.
type Config struct {
	Hookd        Hookd                `yaml:"hookd"`
	Store        Store                `yaml:"store"`
	Prometheus   Service              `yaml:"prometheus"`
	Grafana      Service              `yaml:"grafana"`
	Otelcol      Otelcol              `yaml:"otelcol"`
	TokenScraper TokenScraper         `yaml:"token-scraper"`
	Relay        Relay                `yaml:"relay,omitempty"`
	Env          string               `yaml:"env"`
	Tags         map[string]TagConfig `yaml:"tags,omitempty"`
	Clone        Clone                `yaml:"clone,omitempty"`
}

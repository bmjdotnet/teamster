package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ServiceConfig describes a single service in the yaml config file.
type ServiceConfig struct {
	Mode   string `yaml:"mode"` // "systemd" | "supervisor" | "install" | "external" | "managed" | "none"
	Port   int    `yaml:"port"`
	Health string `yaml:"health"` // HTTP health URL (optional)
	DSN    string `yaml:"dsn"`    // for store only
}

// TagConfig declares one key in the work-item tag vocabulary. The yaml `tags:`
// section is a map of tag_key → TagConfig; it is reconciled into the WMS seed
// vocabulary at store open (see wms.TagSpec / ReconcileVocabulary). Field names
// and yaml tags MUST stay identical to the installer's yamlTagConfig
// (src/cmd/teamster-install/yaml_config.go) or the installer round-trip drifts.
type TagConfig struct {
	Category       string   `yaml:"category"`        // "context" | "lifecycle"
	Cardinality    string   `yaml:"cardinality"`     // "single" | "multi"
	Values         []string `yaml:"values"`          // explicit value list; empty for create-on-apply keys
	Description    string   `yaml:"description"`
	Scope          string   `yaml:"scope"`           // "outcome" | "workunit" | ""
	ExclusionGroup string   `yaml:"exclusion_group"` // mutual exclusion group slug
	AutoExtract    string   `yaml:"auto_extract"`    // "git" | "env" | ""
	Interview      string   `yaml:"interview"`       // "propose" | "auto" | "skip"
}

// FileConfig is the parsed shape of ~/teamster/etc/teamster.yaml.
type FileConfig struct {
	Hookd      ServiceConfig `yaml:"hookd"`
	Store      ServiceConfig `yaml:"store"`
	Prometheus ServiceConfig `yaml:"prometheus"`
	Grafana    ServiceConfig `yaml:"grafana"`
	Otelcol    struct {
		Mode          string `yaml:"mode"`
		GRPCPort      int    `yaml:"grpc_port"`
		HTTPPort      int    `yaml:"http_port"`
		CodexHTTPPort int    `yaml:"codex_http_port"`
	} `yaml:"otelcol"`
	TokenScraper struct {
		Mode string `yaml:"mode"`
	} `yaml:"token-scraper"`
	Env      string               `yaml:"env"`
	LogLevel string               `yaml:"log_level"`
	Tags     map[string]TagConfig `yaml:"tags"`
}

// LoadFile reads ~/teamster/etc/teamster.yaml.
// Returns zero-value FileConfig if the file doesn't exist — graceful
// degradation for installs that haven't been updated yet.
func LoadFile() FileConfig {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, "teamster", "etc", "teamster.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}
	}
	var fc FileConfig
	_ = yaml.Unmarshal(data, &fc)
	return fc
}

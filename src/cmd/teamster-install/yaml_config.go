package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type yamlHookd struct {
	Mode string `yaml:"mode"`
	Port int    `yaml:"port"`
}

type yamlStore struct {
	Mode string `yaml:"mode"`
	DSN  string `yaml:"dsn"`
}

type yamlService struct {
	Mode   string `yaml:"mode"`
	Port   int    `yaml:"port"`
	Health string `yaml:"health,omitempty"`
}

type yamlOtelcol struct {
	Mode     string `yaml:"mode"`
	GRPCPort int    `yaml:"grpc_port"`
	HTTPPort int    `yaml:"http_port"`
}

type yamlTokenScraper struct {
	Mode string `yaml:"mode"`
}

// yamlTagConfig declares one key in the work-item tag vocabulary. Field names
// and yaml tags MUST stay identical to config.TagConfig
// (src/internal/config/yaml.go) or the installer round-trip drifts lossy: the
// runtime read-side reconciles from config.TagConfig, the installer preserves
// it through this struct. Keep the two in lock-step.
type yamlTagConfig struct {
	Category    string   `yaml:"category"`    // "context" | "lifecycle"
	Cardinality string   `yaml:"cardinality"` // "single" | "multi"
	Values      []string `yaml:"values"`      // explicit value list; empty for create-on-apply keys
	Description string   `yaml:"description"`
}

type teamsterYAML struct {
	Hookd        yamlHookd                `yaml:"hookd"`
	Store        yamlStore                `yaml:"store"`
	Prometheus   yamlService              `yaml:"prometheus"`
	Grafana      yamlService              `yaml:"grafana"`
	Otelcol      yamlOtelcol              `yaml:"otelcol"`
	TokenScraper yamlTokenScraper         `yaml:"token-scraper"`
	Env          string                   `yaml:"env"`
	Tags         map[string]yamlTagConfig `yaml:"tags,omitempty"`
}

// defaultTagVocab is the starter work-item tag vocabulary written into a fresh
// teamster.yaml. It is injected ONLY on first install (no prior tags: block);
// operator edits are preserved verbatim on every subsequent upgrade install.
// There is no shipped skel/etc/teamster.yaml — this is the source of the
// default tags: section. Keep field values consistent with the migration seed
// so config reconcile is a no-op, not a fight.
func defaultTagVocab() map[string]yamlTagConfig {
	return map[string]yamlTagConfig{
		"project": {
			Category:    "context",
			Cardinality: "single",
			Description: "The project a work item belongs to.",
		},
		"priority": {
			Category:    "context",
			Cardinality: "single",
			Values:      []string{"p0", "p1", "p2", "p3"},
			Description: "Work-item priority (p0 highest).",
		},
	}
}

// yamlParams bundles the values writeYAMLConfig needs from run().
type yamlParams struct {
	basedir            string
	hookdPort          int
	hookdMode          string
	storeDSN           string
	storeMode          string
	otelcolMode        string
	promMode           string
	grafanaMode        string
	prometheusEndpoint string // --prometheus-endpoint flag value; used to derive health URL hostname
	grafanaEndpoint    string // --grafana-endpoint flag value; used to derive health URL hostname
	env                string
	ports              portConfig
	otelHTTP           int
}

func writeYAMLConfig(p yamlParams) {
	cfg := buildYAMLConfig(p)

	etcDir := filepath.Join(p.basedir, "etc")
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: creating etc dir for teamster.yaml: %v\n", err)
		return
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: marshaling teamster.yaml: %v\n", err)
		return
	}

	dest := filepath.Join(etcDir, "teamster.yaml")
	// 0600: teamster.yaml carries the store DSN (password) inline as a fallback
	// the CLIs read, so it must be owner-only — same credential-hygiene class as
	// the secrets EnvironmentFile. WriteFile honors the mode only on create, so
	// chmod narrows a pre-existing wider file (e.g. an older 0644 install) back
	// to 0600.
	if err := os.WriteFile(dest, out, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "warning: writing teamster.yaml: %v\n", err)
		return
	}
	if err := os.Chmod(dest, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "warning: chmod teamster.yaml: %v\n", err)
		return
	}
	dlog("INFO", "teamster-install.yaml", "wrote teamster.yaml", "path", dest)
}

// readExistingYAML loads an already-written teamster.yaml from basedir/etc/teamster.yaml.
// Returns a zero-value struct when the file is absent or unparseable — callers
// treat zero as "no prior value" and fall back to their computed defaults.
func readExistingYAML(basedir string) teamsterYAML {
	dest := filepath.Join(basedir, "etc", "teamster.yaml")
	data, err := os.ReadFile(dest)
	if err != nil {
		return teamsterYAML{}
	}
	var existing teamsterYAML
	if err := yaml.Unmarshal(data, &existing); err != nil {
		return teamsterYAML{}
	}
	return existing
}

// endpointHost extracts the hostname from an endpoint value (http://host:port,
// http://host:port/path, or bare host:port). Returns "localhost" when empty.
func endpointHost(endpoint string) string {
	if endpoint == "" {
		return "localhost"
	}
	v := endpoint
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		v = "http://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Hostname() == "" {
		return "localhost"
	}
	return u.Hostname()
}

// strOr returns a if non-empty, else b.
func strOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// intOr returns a if non-zero, else b.
func intOr(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

func buildYAMLConfig(p yamlParams) teamsterYAML {
	// Read the existing yaml so that upgrade installs don't clobber values that
	// weren't explicitly passed as flags (e.g. ports chosen by a prior findFreePort).
	prior := readExistingYAML(p.basedir)

	hookdMode := strOr(p.hookdMode, strOr(prior.Hookd.Mode, "systemd"))
	hookdPort := intOr(p.hookdPort, intOr(prior.Hookd.Port, 9125))

	storeMode := strOr(p.storeMode, strOr(prior.Store.Mode, "managed"))
	storeDSN := strOr(p.storeDSN, prior.Store.DSN)

	otelcolMode := strOr(p.otelcolMode, strOr(prior.Otelcol.Mode, "install"))
	otelGRPC := intOr(p.ports.otelGRPC, intOr(prior.Otelcol.GRPCPort, 4327))
	otelHTTP := intOr(p.otelHTTP, intOr(prior.Otelcol.HTTPPort, 4328))

	promMode := strOr(p.promMode, strOr(prior.Prometheus.Mode, "install"))
	promPort := intOr(p.ports.prometheus, intOr(prior.Prometheus.Port, 9190))

	grafanaMode := strOr(p.grafanaMode, strOr(prior.Grafana.Mode, "install"))
	grafanaPort := intOr(p.ports.grafana, intOr(prior.Grafana.Port, 3100))

	ccusageMode := strOr(prior.TokenScraper.Mode, "install")

	envLabel := strOr(p.env, strOr(prior.Env, "production"))

	// Round-trip the operator's tag vocabulary. readExistingYAML preserves the
	// prior tags: block (the struct now carries it), so an upgrade install never
	// clobbers operator edits. Inject the default vocabulary ONLY on first
	// install (no prior tags:). Without this, every upgrade would silently
	// delete the operator's vocabulary.
	tags := prior.Tags
	if len(tags) == 0 {
		tags = defaultTagVocab()
	}

	return teamsterYAML{
		Hookd: yamlHookd{
			Mode: hookdMode,
			Port: hookdPort,
		},
		Store: yamlStore{
			Mode: storeMode,
			DSN:  storeDSN,
		},
		Prometheus: yamlService{
			Mode:   promMode,
			Port:   promPort,
			Health: fmt.Sprintf("http://%s:%d/-/healthy", endpointHost(p.prometheusEndpoint), promPort),
		},
		Grafana: yamlService{
			Mode:   grafanaMode,
			Port:   grafanaPort,
			Health: fmt.Sprintf("http://%s:%d/api/health", endpointHost(p.grafanaEndpoint), grafanaPort),
		},
		Otelcol: yamlOtelcol{
			Mode:     otelcolMode,
			GRPCPort: otelGRPC,
			HTTPPort: otelHTTP,
		},
		TokenScraper: yamlTokenScraper{
			Mode: ccusageMode,
		},
		Env:  envLabel,
		Tags: tags,
	}
}

// Package config handles Teamster configuration loading and defaults.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/wms"
)

// StoreDSN is the parsed shape of TEAMSTER_STORE_DSN, decomposed once here and
// consumed by everyone who needs the pieces (backup, grafana, status, and the
// store registry). Parsing is scheme-agnostic — any well-formed URL parses;
// scheme *support* is a registry concern (store.Open), not this parser's.
type StoreDSN struct {
	Scheme   string
	Raw      string
	User     string
	Password string
	Host     string
	Port     int
	Database string
	Params   map[string]string
}

// Config holds all runtime configuration for Teamster components.
type Config struct {
	// Hook server settings
	HookServerURL  string
	HookServerPort int
	HookServerBind string

	// Data storage paths
	DataDir    string
	LogFile    string
	DedupDir   string
	SessionDir string

	// Store (TEAMSTER_STORE_DSN).
	StoreDSN StoreDSN

	// Atail
	AtailHistoryDefault int

	// Identity. Canonical name is TEAMSTER_HOST (renamed from
	// TEAMSTER_HOSTNAME in the D2 refactor). The bridge gauge label
	// matches the field name.
	Host string

	// User is the OS username whose ~/.claude home holds the session
	// transcripts. It is the second half of the host-local routing key
	// (with Host): focus-attribution recovery reads the transcript files
	// from this user's home on this host. Default os/user.Current().Username
	// (fallback $USER); override TEAMSTER_USER.
	User string

	// Session tracker tuning (D1 sweeper). SessionTimeout is the inactivity
	// horizon after which a (session_id, agent_name) pair is purged from the
	// in-memory tracker; SessionSweepInterval is the sweep cadence. Per
	// SPEC §4.4 the sweeper clamps SessionSweepInterval to SessionTimeout/2
	// at startup — that clamp is hookd's responsibility, not this loader's.
	SessionTimeout       time.Duration
	SessionSweepInterval time.Duration

	// Supervisor settings.
	//
	// HookdMode selects how hookd is managed. "systemd" = systemd unit owns
	// hookd; "supervisor" = supervisor manages hookd too (cleanroom / ephemeral installs);
	// "external" = remote hub, Python hook client only.
	// Corresponds to --hookd-mode install flag.
	HookdMode string
	// Env is the deployment environment label injected into prometheus external_labels.
	// Default "production"; cleanroom installs use "cleanroom".
	Env string

	// Port assignments. Defaults in Default(); overridable via env vars.
	PrometheusPort int
	GrafanaPort    int
	OtelGRPCPort   int
	OtelHTTPPort   int
	// PrometheusRetention is the --storage.tsdb.retention.time value (e.g. "365d").
	PrometheusRetention string
	// PrometheusRetentionSize is the --storage.tsdb.retention.size value (e.g. "50GB").
	// Empty means no size cap.
	PrometheusRetentionSize string

	// Per-service modes from teamster.yaml / flags.
	// install = download+run locally, external = point at remote endpoint,
	// managed = operator-managed (bring your own), none = disabled.
	// StoreMode additionally accepts "install" to mean apt-install MySQL.
	StoreMode      string // install | external | managed
	PrometheusMode string // install | external | managed | none
	GrafanaMode    string // install | external | managed | none
	OtelcolMode    string // install | external | managed | none
	CcusageMode    string // install | none

	// Per-service health URL overrides from teamster.yaml. Empty uses derived default.
	HookdHealth      string
	PrometheusHealth string
	GrafanaHealth    string

	LogLevel string

	// GCStaleHours is the reaper's Phase 3 threshold: sessions with no events
	// in this many hours are considered stale and their intervals are closed.
	// Default 0 disables Phase 3 entirely (only Phases 1+2 run).
	GCStaleHours int

	// ReaperInterval is the cadence for the interval reaper goroutine.
	// Default 15 minutes.
	ReaperInterval time.Duration

	// Solo is single-agent mode: TEAMSTER_SOLO=1. When set, the hook client
	// suppresses team-mandate ceremony (the Agent-Teams dispatch instruction,
	// the bootstrap nudge, and the bare-Agent block). Solo mode only ever
	// REMOVES injected mandate; it never adds any. The team-mode path (Solo
	// false) is byte-identical to pre-solo behavior.
	Solo bool

	// Tags is the declared work-item tag vocabulary from teamster.yaml's
	// `tags:` section (tag_key → TagConfig). wms-mcp reconciles it into the
	// WMS seed vocabulary at store open via tagSpecsFromConfig. Nil when no
	// `tags:` block is present — reconcile then leaves the seeds untouched.
	Tags map[string]TagConfig

	// RequireTagsOnDone is hard close-out enforcement: TEAMSTER_REQUIRE_TAGS_ON_DONE=1.
	// When set, the store rejects a workunit's 'done' transition if any required
	// tag key (tags.required=1) has no value bound to the entity. Default false —
	// the soft path (a close-out warning) still fires; only the gate is opt-in.
	// Outcomes are exempt; this applies to workunits only.
	RequireTagsOnDone bool

	// ReadOnly makes hookd reject write endpoints (MCP, telemetry, drain) while
	// still serving /event, reads, SSE, and dashboards. TEAMSTER_HOOKD_READ_ONLY=1
	// or --read-only flag on hookd.
	ReadOnly bool
}

// Default returns a Config with all defaults populated.
func Default() Config {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	dataDir := filepath.Join(home, "teamster", "var")
	legacyDir := filepath.Join(home, ".local", "share", "teamster")
	if _, err := os.Stat(legacyDir); err == nil {
		if _, err := os.Stat(dataDir); err != nil {
			dataDir = legacyDir
		}
	}

	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}

	username := ""
	if u, err := user.Current(); err == nil {
		username = u.Username
	}
	if username == "" {
		username = os.Getenv("USER")
	}

	return Config{
		HookServerURL:        fmt.Sprintf("http://%s:9125/event", host),
		HookServerPort:       9125,
		HookServerBind:       "0.0.0.0",
		DataDir:              dataDir,
		LogFile:              filepath.Join(dataDir, "events.jsonl"),
		DedupDir:             filepath.Join(dataDir, "dedup"),
		SessionDir:           filepath.Join(dataDir, "sessions"),
		StoreDSN:             StoreDSN{},
		AtailHistoryDefault:  20,
		Host:                 host,
		User:                 username,
		SessionTimeout:       5 * time.Minute,
		SessionSweepInterval: 30 * time.Second,
		PrometheusPort:       9190,
		GrafanaPort:          3100,
		OtelGRPCPort:         4327,
		OtelHTTPPort:         4328,
		PrometheusRetention:  "365d",
		Env:                  "production",
		HookdMode:            "systemd",
		StoreMode:            "managed",
		OtelcolMode:          "install",
		PrometheusMode:       "install",
		GrafanaMode:          "install",
		CcusageMode:          "install",
		LogLevel:             "info",
		GCStaleHours:         0,
		ReaperInterval:       15 * time.Minute,
	}
}

// Load returns a Config populated from TEAMSTER_* env vars, falling back to
// defaults. TEAMSTER_BASEDIR is the master override — if set, DataDir
// becomes BASEDIR/var and all paths derive from it. TEAMSTER_DATA_DIR
// overrides just the data directory. It ensures DataDir exists before
// returning.
//
// The legacy DB env vars (TEAMSTER_DB_PATH, TEAMSTER_WMS_DB,
// TEAMSTER_DB_DRIVER) are NOT read — setting them has no effect. The
// store is configured solely by TEAMSTER_STORE_DSN (SPEC §6.2).
//
// TEAMSTER_HOSTNAME is similarly ignored; the canonical identity env is
// TEAMSTER_HOST.
func Load() (Config, error) {
	cfg := Default()

	// Seed from yaml file before env vars (yaml < env < flags precedence).
	fc := LoadFile()
	if fc.Hookd.Mode != "" {
		cfg.HookdMode = fc.Hookd.Mode
	}
	if fc.Hookd.Port != 0 {
		cfg.HookServerPort = fc.Hookd.Port
	}
	if fc.Store.DSN != "" {
		parsed, err := ParseStoreDSN(fc.Store.DSN)
		if err == nil {
			cfg.StoreDSN = parsed
		}
	}
	if fc.Prometheus.Port != 0 {
		cfg.PrometheusPort = fc.Prometheus.Port
	}
	if fc.Grafana.Port != 0 {
		cfg.GrafanaPort = fc.Grafana.Port
	}
	if fc.Otelcol.GRPCPort != 0 {
		cfg.OtelGRPCPort = fc.Otelcol.GRPCPort
	}
	if fc.Otelcol.HTTPPort != 0 {
		cfg.OtelHTTPPort = fc.Otelcol.HTTPPort
	}
	if fc.Env != "" {
		cfg.Env = fc.Env
	}
	if fc.LogLevel != "" {
		cfg.LogLevel = fc.LogLevel
	}
	if len(fc.Tags) > 0 {
		cfg.Tags = fc.Tags
	}
	if fc.Prometheus.Mode != "" {
		cfg.PrometheusMode = fc.Prometheus.Mode
	}
	if fc.Prometheus.Health != "" {
		cfg.PrometheusHealth = fc.Prometheus.Health
	}
	if fc.Grafana.Mode != "" {
		cfg.GrafanaMode = fc.Grafana.Mode
	}
	if fc.Grafana.Health != "" {
		cfg.GrafanaHealth = fc.Grafana.Health
	}
	if fc.Otelcol.Mode != "" {
		cfg.OtelcolMode = fc.Otelcol.Mode
	}
	if fc.Hookd.Health != "" {
		cfg.HookdHealth = fc.Hookd.Health
	}
	if fc.Store.Mode != "" {
		cfg.StoreMode = fc.Store.Mode
	}
	if fc.TokenScraper.Mode != "" {
		cfg.CcusageMode = fc.TokenScraper.Mode
	}

	if v := os.Getenv("TEAMSTER_BASEDIR"); v != "" {
		setDataDir(&cfg, filepath.Join(v, "var"))
	} else {
		home, _ := os.UserHomeDir()
		for _, candidate := range []string{
			filepath.Join(home, "teamster"),
			"/usr/local/teamster",
		} {
			if fi, err := os.Stat(filepath.Join(candidate, "var")); err == nil && fi.IsDir() {
				setDataDir(&cfg, filepath.Join(candidate, "var"))
				break
			}
		}
	}
	if v := os.Getenv("TEAMSTER_DATA_DIR"); v != "" {
		setDataDir(&cfg, v)
	}
	if v := os.Getenv("TEAMSTER_HOOK_SERVER_URL"); v != "" {
		cfg.HookServerURL = v
	}
	if v := os.Getenv("TEAMSTER_HOOK_SERVER_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_HOOK_SERVER_PORT: %w", err)
		}
		cfg.HookServerPort = n
	}
	if v := os.Getenv("TEAMSTER_HOOK_SERVER_BIND"); v != "" {
		cfg.HookServerBind = v
	}
	if v := os.Getenv("TEAMSTER_LOG_FILE"); v != "" {
		cfg.LogFile = v
	}
	if v := os.Getenv("TEAMSTER_DEDUP_DIR"); v != "" {
		cfg.DedupDir = v
	}
	if v := os.Getenv("TEAMSTER_SESSION_DIR"); v != "" {
		cfg.SessionDir = v
	}
	if v := os.Getenv("TEAMSTER_STORE_DSN"); v != "" {
		parsed, err := ParseStoreDSN(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_STORE_DSN: %w", err)
		}
		cfg.StoreDSN = parsed
	}
	if v := os.Getenv("TEAMSTER_ATAIL_HISTORY_DEFAULT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_ATAIL_HISTORY_DEFAULT: %w", err)
		}
		cfg.AtailHistoryDefault = n
	}
	if v := os.Getenv("TEAMSTER_HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("TEAMSTER_USER"); v != "" {
		cfg.User = v
	}
	if v := os.Getenv("TEAMSTER_SESSION_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_SESSION_TIMEOUT: %w", err)
		}
		cfg.SessionTimeout = d
	}
	if v := os.Getenv("TEAMSTER_SESSION_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_SESSION_SWEEP_INTERVAL: %w", err)
		}
		cfg.SessionSweepInterval = d
	}
	// TEAMSTER_HOOKD_MODE accepts "systemd", "supervisor", or "external".
	// --hookd-mode install flag sets this via the env file.
	if v := os.Getenv("TEAMSTER_HOOKD_MODE"); v == "systemd" || v == "supervisor" || v == "external" {
		cfg.HookdMode = v
	}
	if v := os.Getenv("TEAMSTER_ENV"); v != "" {
		cfg.Env = v
	}
	if v := os.Getenv("TEAMSTER_PROMETHEUS_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_PROMETHEUS_PORT: %w", err)
		}
		cfg.PrometheusPort = n
	}
	if v := os.Getenv("TEAMSTER_GRAFANA_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_GRAFANA_PORT: %w", err)
		}
		cfg.GrafanaPort = n
	}
	if v := os.Getenv("TEAMSTER_OTEL_GRPC_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_OTEL_GRPC_PORT: %w", err)
		}
		cfg.OtelGRPCPort = n
	}
	if v := os.Getenv("TEAMSTER_OTEL_HTTP_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_OTEL_HTTP_PORT: %w", err)
		}
		cfg.OtelHTTPPort = n
	}
	if v := os.Getenv("TEAMSTER_PROMETHEUS_RETENTION"); v != "" {
		cfg.PrometheusRetention = v
	}
	if v := os.Getenv("TEAMSTER_PROMETHEUS_RETENTION_SIZE"); v != "" {
		cfg.PrometheusRetentionSize = v
	}
	if v := os.Getenv("TEAMSTER_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if os.Getenv("TEAMSTER_SOLO") == "1" {
		cfg.Solo = true
	}
	if os.Getenv("TEAMSTER_REQUIRE_TAGS_ON_DONE") == "1" {
		cfg.RequireTagsOnDone = true
	}
	if os.Getenv("TEAMSTER_HOOKD_READ_ONLY") == "1" {
		cfg.ReadOnly = true
	}
	if v := os.Getenv("TEAMSTER_GC_STALE_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_GC_STALE_HOURS: %w", err)
		}
		cfg.GCStaleHours = n
	}
	if v := os.Getenv("TEAMSTER_REAPER_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("TEAMSTER_REAPER_INTERVAL: %w", err)
		}
		cfg.ReaperInterval = d
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return Config{}, fmt.Errorf("creating DataDir %s: %w", cfg.DataDir, err)
	}

	return cfg, nil
}

// setDataDir updates DataDir and re-derives all dependent paths.
func setDataDir(cfg *Config, dir string) {
	cfg.DataDir = dir
	cfg.LogFile = filepath.Join(dir, "events.jsonl")
	cfg.DedupDir = filepath.Join(dir, "dedup")
	cfg.SessionDir = filepath.Join(dir, "sessions")
}

// EventLogPath returns the path to the JSONL event log (used by feed).
func (c Config) EventLogPath() string {
	return c.LogFile
}

// TagSpecs converts the declared `tags:` vocabulary on Config into the
// []wms.TagSpec that ReconcileVocabulary consumes. It builds the specs here so
// the store stays free of config types. Keys are emitted in sorted order for a
// deterministic reconcile sweep. Returns nil when no vocabulary is declared.
func (c Config) TagSpecs() []wms.TagSpec {
	if len(c.Tags) == 0 {
		return nil
	}
	keys := make([]string, 0, len(c.Tags))
	for k := range c.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	specs := make([]wms.TagSpec, 0, len(keys))
	for _, k := range keys {
		tc := c.Tags[k]
		spec := wms.TagSpec{
			Key:         k,
			Category:    tc.Category,
			Cardinality: tc.Cardinality,
			Values:      tc.Values,
			Description: tc.Description,
		}
		if tc.Scope != "" {
			spec.Scope = &tc.Scope
		}
		if tc.ExclusionGroup != "" {
			spec.ExclusionGroup = &tc.ExclusionGroup
		}
		if tc.AutoExtract != "" {
			spec.AutoExtract = &tc.AutoExtract
		}
		if tc.Interview != "" {
			spec.Interview = &tc.Interview
		}
		specs = append(specs, spec)
	}
	return specs
}

// ParseStoreDSN parses a TEAMSTER_STORE_DSN value as a well-formed URL of any
// scheme — scheme support is a registry concern (store.Open), not this
// parser's.
func ParseStoreDSN(raw string) (StoreDSN, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return StoreDSN{}, fmt.Errorf("empty DSN")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// net/url returns a *url.Error whose string embeds the raw DSN (and
		// thus the password) via its URL field. This error wraps up through
		// config.Load and the tags/wms CLIs to stderr/the feed, and a
		// password containing a space defeats redact's userinfo masking, so
		// report only the scheme, never the raw DSN or the wrapped error.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return StoreDSN{}, fmt.Errorf("parse DSN (scheme %q): %v", dsnScheme(raw), err)
	}
	if u.Scheme == "" {
		return StoreDSN{}, fmt.Errorf("DSN missing scheme")
	}

	dsn := StoreDSN{
		Scheme:   u.Scheme,
		Raw:      raw,
		User:     u.User.Username(),
		Host:     u.Hostname(),
		Database: strings.TrimPrefix(u.Path, "/"),
	}
	if pw, ok := u.User.Password(); ok {
		dsn.Password = pw
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return StoreDSN{}, fmt.Errorf("DSN port: %w", err)
		}
		dsn.Port = n
	}
	if q := u.Query(); len(q) > 0 {
		params := make(map[string]string, len(q))
		for k, vs := range q {
			if len(vs) > 0 {
				params[k] = vs[0]
			}
		}
		dsn.Params = params
	}
	return dsn, nil
}

// dsnScheme returns the scheme portion (before "://") of a DSN, or "<none>" if
// there is no scheme separator. It deliberately never returns userinfo, so it
// is safe to print in an error regardless of password shape (a password with a
// space defeats redact.Redact's userinfo masking). Mirrors
// internal/store/mysql.dsnScheme.
func dsnScheme(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		return raw[:i]
	}
	return "<none>"
}

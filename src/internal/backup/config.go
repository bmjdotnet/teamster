package backup

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bmjdotnet/teamster/internal/config"
	"gopkg.in/yaml.v3"
)

type Config struct {
	BackupDir string          `yaml:"backup_dir"`
	Hostname  string          `yaml:"hostname"`
	Schedule  string          `yaml:"schedule"`
	Retention RetentionConfig `yaml:"retention"`
	Stores    StoresConfig    `yaml:"stores"`
}

type RetentionConfig struct {
	KeepFor  string `yaml:"keep_for"`  // "1h", "1d", "7d", "30d", "1y"
	MaxCount int    `yaml:"max_count"` // optional hard cap; 0 = no limit
}

type StoresConfig struct {
	MySQL      MySQLConfig      `yaml:"mysql"`
	Prometheus PrometheusConfig `yaml:"prometheus"`
	Grafana    GrafanaConfig    `yaml:"grafana"`
	OTel       OTelConfig       `yaml:"otel"`
	Teamster   TeamsterConfig   `yaml:"teamster"`
}

type MySQLConfig struct {
	Enabled   bool     `yaml:"enabled"`
	DSN       string   `yaml:"dsn"` // optional override; inherits from store.dsn when absent
	Databases []string `yaml:"databases"`
}

type PrometheusConfig struct {
	Enabled bool   `yaml:"enabled"`
	DataDir string `yaml:"data_dir"`
}

type GrafanaConfig struct {
	Enabled         bool   `yaml:"enabled"`
	Mode            string `yaml:"mode"` // "install" or "external"
	DataDir         string `yaml:"data_dir"`
	ProvisioningDir string `yaml:"provisioning_dir"`
	IncludePlugins  bool   `yaml:"include_plugins"`
}

type OTelConfig struct {
	Enabled bool     `yaml:"enabled"`
	Files   []string `yaml:"files"`
}

type TeamsterConfig struct {
	Enabled     bool   `yaml:"enabled"`
	BaseDir     string `yaml:"base_dir"`
	IncludeLogs bool   `yaml:"include_logs"`
}

type teamsterYAML struct {
	Backup  Config `yaml:"backup"`
	Store   struct {
		DSN string `yaml:"dsn"`
	} `yaml:"store"`
	Grafana struct {
		Mode string `yaml:"mode"`
	} `yaml:"grafana"`
}

// LoadConfig reads the backup section from teamster.yaml. When restoreMode is
// true, backup_dir is not required (the restore path comes from --restore).
func LoadConfig(path string, restoreMode bool) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var wrapper teamsterYAML
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg := &wrapper.Backup

	// Inherit store DSN for MySQL when not explicitly set in backup section.
	if cfg.Stores.MySQL.DSN == "" && wrapper.Store.DSN != "" {
		cfg.Stores.MySQL.DSN = wrapper.Store.DSN
	}

	// Inherit grafana mode from top-level grafana section when not set in backup.
	if cfg.Stores.Grafana.Mode == "" && wrapper.Grafana.Mode != "" {
		cfg.Stores.Grafana.Mode = wrapper.Grafana.Mode
	}

	// Infer basedir from the config path: ~/teamster/etc/teamster.yaml → ~/teamster/
	basedir := filepath.Dir(filepath.Dir(path))

	// Default teamster base_dir and otel files from the basedir when absent.
	if cfg.Stores.Teamster.BaseDir == "" {
		cfg.Stores.Teamster.BaseDir = basedir
	}
	if len(cfg.Stores.OTel.Files) == 0 {
		cfg.Stores.OTel.Files = []string{filepath.Join(basedir, "etc", "otelcol.yaml")}
	}

	// Default mysql databases from the DSN's database name when absent.
	if len(cfg.Stores.MySQL.Databases) == 0 && cfg.Stores.MySQL.DSN != "" {
		if parsed, err := config.ParseStoreDSN(cfg.Stores.MySQL.DSN); err == nil && parsed.Database != "" {
			cfg.Stores.MySQL.Databases = []string{parsed.Database}
		}
	}

	if !restoreMode && cfg.BackupDir != "" {
		if err := requireAbsolute("backup_dir", cfg.BackupDir); err != nil {
			return nil, err
		}
	}
	if cfg.Stores.Prometheus.Enabled && cfg.Stores.Prometheus.DataDir != "" {
		if err := requireAbsolute("prometheus.data_dir", cfg.Stores.Prometheus.DataDir); err != nil {
			return nil, err
		}
	}
	if cfg.Stores.Grafana.Enabled {
		if cfg.Stores.Grafana.DataDir != "" {
			if err := requireAbsolute("grafana.data_dir", cfg.Stores.Grafana.DataDir); err != nil {
				return nil, err
			}
		}
		if cfg.Stores.Grafana.ProvisioningDir != "" {
			if err := requireAbsolute("grafana.provisioning_dir", cfg.Stores.Grafana.ProvisioningDir); err != nil {
				return nil, err
			}
		}
	}
	if cfg.Stores.Teamster.Enabled && cfg.Stores.Teamster.BaseDir != "" {
		if err := requireAbsolute("teamster.base_dir", cfg.Stores.Teamster.BaseDir); err != nil {
			return nil, err
		}
	}
	if cfg.Schedule == "" {
		cfg.Schedule = "1h"
	}
	if cfg.Retention.KeepFor == "" {
		cfg.Retention.KeepFor = "7d"
	}
	if cfg.Hostname == "" {
		cfg.Hostname, _ = os.Hostname()
	}
	return cfg, nil
}

func requireAbsolute(field, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be an absolute path, got: %q", field, path)
	}
	return nil
}

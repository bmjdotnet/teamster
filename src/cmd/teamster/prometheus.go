package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"text/template"

	"github.com/bmjdotnet/teamster/internal/config"
)

// prometheusTemplateData is the shape fed to prometheus.yaml.tmpl.
type prometheusTemplateData struct {
	Hostname            string
	Env                 string
	HookServerHost      string
	HookServerPort      int
	PrometheusPort      int
	PrometheusDataDir   string
	PrometheusRetention string
}

// StartPrometheus renders the prometheus config template, writes it to
// <basedir>/etc/prometheus.yaml, then launches the prometheus binary.
// It returns the running *exec.Cmd so the supervisor can reap it.
func StartPrometheus(ctx context.Context, cfg config.Config) (*exec.Cmd, error) {
	basedir := prometheusBasedir(cfg)

	configPath := filepath.Join(basedir, "etc", "prometheus.yaml")
	dataDir := filepath.Join(basedir, "var", "prometheus")
	logPath := filepath.Join(basedir, "var", "logs", "prometheus.log")
	binPath := filepath.Join(basedir, "bin", "prometheus")

	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return nil, fmt.Errorf("prometheus: mkdir etc: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("prometheus: mkdir data: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("prometheus: mkdir logs: %w", err)
	}

	if err := renderPrometheusConfig(configPath, cfg, dataDir); err != nil {
		return nil, fmt.Errorf("prometheus: render config: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("prometheus: open log: %w", err)
	}

	cmd := exec.CommandContext(ctx, binPath,
		"--config.file="+configPath,
		fmt.Sprintf("--web.listen-address=0.0.0.0:%d", cfg.PrometheusPort),
		"--storage.tsdb.path="+dataDir,
		"--storage.tsdb.retention.time="+cfg.PrometheusRetention,
		"--web.enable-remote-write-receiver",
	)
	cmd.Dir = basedir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSetsid(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("prometheus: start: %w", err)
	}

	pidPath := filepath.Join(basedir, "var", "pids", "prometheus.pid")
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)

	// Close log file when process exits; crashloop in startComponent owns restart.
	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()

	return cmd, nil
}

// StopPrometheus sends SIGTERM to a running prometheus process.
func StopPrometheus(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(os.Interrupt)
}

// PrometheusPort returns the configured prometheus listen port.
func PrometheusPort(cfg config.Config) int {
	return cfg.PrometheusPort
}

func renderPrometheusConfig(dst string, cfg config.Config, dataDir string) error {
	tmplPath := prometheusTemplatePath(cfg)
	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		return fmt.Errorf("read template %s: %w", tmplPath, err)
	}

	tmpl, err := template.New("prometheus.yaml").Parse(string(tmplBytes))
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer f.Close()

	hookHost := "127.0.0.1"
	if u, err := url.Parse(cfg.HookServerURL); err == nil && u.Hostname() != "" {
		h := u.Hostname()
		if h != "localhost" {
			hookHost = h
		}
	}

	data := prometheusTemplateData{
		Hostname:            cfg.Host,
		Env:                 cfg.Env,
		HookServerHost:      hookHost,
		HookServerPort:      cfg.HookServerPort,
		PrometheusPort:      cfg.PrometheusPort,
		PrometheusDataDir:   dataDir,
		PrometheusRetention: cfg.PrometheusRetention,
	}
	return tmpl.Execute(f, data)
}

func prometheusBasedir(cfg config.Config) string {
	// DataDir is <basedir>/var; parent is basedir.
	return filepath.Dir(cfg.DataDir)
}

func prometheusTemplatePath(cfg config.Config) string {
	return filepath.Join(prometheusBasedir(cfg), "etc", "prometheus.yaml.tmpl")
}

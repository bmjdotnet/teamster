package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"text/template"

	"github.com/bmjdotnet/teamster/internal/config"
)

// otelcolTmplData is the template data struct for otelcol.yaml.tmpl rendering.
type otelcolTmplData struct {
	OtelGRPCPort    int
	OtelHTTPPort    int
	PrometheusPort  int
	Env             string
	ForwardEndpoint string // optional; empty = no forwarding pipeline
}

func otelcolDataFrom(cfg config.Config) otelcolTmplData {
	d := otelcolTmplData{
		OtelGRPCPort:    cfg.OtelGRPCPort,
		OtelHTTPPort:    cfg.OtelHTTPPort,
		PrometheusPort:  cfg.PrometheusPort,
		Env:             cfg.Env,
		ForwardEndpoint: os.Getenv("TEAMSTER_OTEL_FORWARD_ENDPOINT"),
	}
	return d
}

// OtelcolPort returns the primary listen port for processAlive liveness checks.
// The primary port is gRPC (SPEC §10 portFor table).
func OtelcolPort(cfg config.Config) int {
	return cfg.OtelGRPCPort
}

// renderOtelcolConfig renders otelcol.yaml.tmpl using cfg and writes the
// result to destPath.
func renderOtelcolConfig(cfg config.Config, tmplPath, destPath string) error {
	raw, err := os.ReadFile(tmplPath)
	if err != nil {
		return fmt.Errorf("read otelcol template %s: %w", tmplPath, err)
	}
	t, err := template.New("otelcol").Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parse otelcol template: %w", err)
	}
	data := otelcolDataFrom(cfg)
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return fmt.Errorf("render otelcol template: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(destPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write otelcol config %s: %w", destPath, err)
	}
	return nil
}

// StartOtelcol renders the otelcol config template, then starts otelcol-contrib
// as a supervised child process. The process runs in its own session (Setsid),
// with stdout/stderr redirected to <basedir>/var/logs/otelcol.log and its PID
// written to <basedir>/var/pids/otelcol.pid.
//
// @bundle wires this into start.go's supervisor loop.
func StartOtelcol(ctx context.Context, cfg config.Config) (*exec.Cmd, error) {
	basedir := prometheusBasedir(cfg) // DataDir = <basedir>/var; parent = basedir
	binPath := filepath.Join(basedir, "bin", "otelcol-contrib")
	tmplPath := filepath.Join(basedir, "etc", "otelcol.yaml.tmpl")
	cfgPath := filepath.Join(basedir, "etc", "otelcol.yaml")
	logPath := filepath.Join(basedir, "var", "logs", "otelcol.log")
	pidPath := filepath.Join(basedir, "var", "pids", "otelcol.pid")

	if err := os.MkdirAll(filepath.Join(basedir, "var", "logs"), 0o755); err != nil {
		return nil, fmt.Errorf("create logs dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(basedir, "var", "pids"), 0o755); err != nil {
		return nil, fmt.Errorf("create pids dir: %w", err)
	}

	if err := renderOtelcolConfig(cfg, tmplPath, cfgPath); err != nil {
		return nil, fmt.Errorf("render otelcol config: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open otelcol log %s: %w", logPath, err)
	}

	cmd := exec.CommandContext(ctx, binPath, "--config", cfgPath)
	cmd.Dir = basedir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSetsid(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start otelcol-contrib: %w", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		// Non-fatal: PID file failure doesn't stop the process.
		_ = err
	}

	// Close log file when process exits; crashloop in startComponent owns restart.
	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()

	return cmd, nil
}

// StopOtelcol sends SIGINT to the otelcol process.
// If cmd is nil (e.g. otelcol was not started in this session), it is a no-op.
func StopOtelcol(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(os.Interrupt)
}

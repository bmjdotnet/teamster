package main

import (
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/config"
)

func TestParseSupervisorFlags(t *testing.T) {
	orig := settingsEnvReader
	settingsEnvReader = func(string) string { return "" }
	t.Cleanup(func() { settingsEnvReader = orig })
	tests := []struct {
		name    string
		args    []string
		wantErr string
		check   func(t *testing.T, cfg config.Config)
	}{
		{
			name: "hookd-mode equals form",
			args: []string{"--hookd-mode=supervisor"},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.HookdMode != "supervisor" {
					t.Errorf("HookdMode = %q, want %q", cfg.HookdMode, "supervisor")
				}
			},
		},
		{
			name: "hookd-mode space form",
			args: []string{"--hookd-mode", "external"},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.HookdMode != "external" {
					t.Errorf("HookdMode = %q, want %q", cfg.HookdMode, "external")
				}
			},
		},
		{
			name: "env equals form",
			args: []string{"--env=staging"},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.Env != "staging" {
					t.Errorf("Env = %q, want %q", cfg.Env, "staging")
				}
			},
		},
		{
			name: "env space form",
			args: []string{"--env", "staging"},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.Env != "staging" {
					t.Errorf("Env = %q, want %q", cfg.Env, "staging")
				}
			},
		},
		{
			name: "prometheus-retention space form",
			args: []string{"--prometheus-retention", "30d"},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.PrometheusRetention != "30d" {
					t.Errorf("PrometheusRetention = %q, want %q", cfg.PrometheusRetention, "30d")
				}
			},
		},
		{
			name: "systemd-hookd alias",
			args: []string{"--systemd-hookd"},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.HookdMode != "systemd" {
					t.Errorf("HookdMode = %q, want %q", cfg.HookdMode, "systemd")
				}
			},
		},
		{
			name: "supervisor-hookd alias",
			args: []string{"--supervisor-hookd"},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.HookdMode != "supervisor" {
					t.Errorf("HookdMode = %q, want %q", cfg.HookdMode, "supervisor")
				}
			},
		},
		{
			name: "mixed forms together",
			args: []string{"--hookd-mode", "supervisor", "--env=staging", "--prometheus-retention=14d"},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.HookdMode != "supervisor" {
					t.Errorf("HookdMode = %q", cfg.HookdMode)
				}
				if cfg.Env != "staging" {
					t.Errorf("Env = %q", cfg.Env)
				}
				if cfg.PrometheusRetention != "14d" {
					t.Errorf("PrometheusRetention = %q", cfg.PrometheusRetention)
				}
			},
		},
		{
			name:    "unknown argument errors loudly",
			args:    []string{"--nonsense"},
			wantErr: "unknown argument: --nonsense",
		},
		{
			name:    "unknown positional errors loudly",
			args:    []string{"garbage"},
			wantErr: "unknown argument: garbage",
		},
		{
			name:    "service-mode flags now rejected",
			args:    []string{"--otelcol-mode=install"},
			wantErr: "unknown argument: --otelcol-mode=install",
		},
		{
			name:    "hookd-mode space form missing value",
			args:    []string{"--hookd-mode"},
			wantErr: "--hookd-mode requires a value",
		},
		{
			name:    "hookd-mode space form value eats next flag",
			args:    []string{"--hookd-mode", "--env=prod"},
			wantErr: "--hookd-mode requires a value",
		},
		{
			name:    "env missing value at end",
			args:    []string{"--env"},
			wantErr: "--env requires a value",
		},
		{
			name: "no args is fine",
			args: []string{},
			check: func(t *testing.T, cfg config.Config) {
				if cfg.HookdMode != "systemd" {
					t.Errorf("HookdMode = %q, want systemd", cfg.HookdMode)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			err := parseSupervisorFlags(tt.args, &cfg)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

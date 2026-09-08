package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bmjdotnet/teamster/internal/config"
)

// TestHookdUnitMasked mirrors scripts/test-installrunner-masking.sh's
// unit_is_masked cases exactly (absent / regular file / masked symlink /
// symlink to something else), so the Go and shell masking predicates stay
// provably equivalent (wh2-hookd-enable-on-install).
func TestHookdUnitMasked(t *testing.T) {
	orig := hookdUnitPath
	t.Cleanup(func() { hookdUnitPath = orig })

	tests := []struct {
		name       string
		setup      func(t *testing.T, path string)
		wantMasked bool
	}{
		{
			name:  "absent unit is not masked",
			setup: func(t *testing.T, path string) {},
		},
		{
			name: "regular file is not masked",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("[Unit]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink to /dev/null is masked",
			setup: func(t *testing.T, path string) {
				if err := os.Symlink("/dev/null", path); err != nil {
					t.Fatal(err)
				}
			},
			wantMasked: true,
		},
		{
			name: "symlink to a non-null target is not masked",
			setup: func(t *testing.T, path string) {
				if err := os.Symlink("/etc/hostname", path); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Each subtest gets its own directory (not a filename derived
			// from tt.name) — the case names themselves contain "/" (e.g.
			// "symlink to /dev/null"), which would otherwise try to create
			// the unit file inside a nonexistent subdirectory.
			hookdUnitPath = filepath.Join(t.TempDir(), "teamster-hookd.service")
			tt.setup(t, hookdUnitPath)
			got := hookdUnitMasked()
			if got != tt.wantMasked {
				t.Errorf("hookdUnitMasked() = %v, want %v", got, tt.wantMasked)
			}
		})
	}
}

func TestParseSupervisorFlags(t *testing.T) {
	orig := settingsEnvReader
	settingsEnvReader = func(string) string { return "" }
	t.Cleanup(func() { settingsEnvReader = orig })
	origExec := execTarget
	t.Cleanup(func() { execTarget = origExec })
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
		{
			name: "exec equals form",
			args: []string{"--exec=prometheus"},
			check: func(t *testing.T, cfg config.Config) {
				if execTarget != "prometheus" {
					t.Errorf("execTarget = %q, want %q", execTarget, "prometheus")
				}
			},
		},
		{
			name: "exec space form",
			args: []string{"--exec", "grafana"},
			check: func(t *testing.T, cfg config.Config) {
				if execTarget != "grafana" {
					t.Errorf("execTarget = %q, want %q", execTarget, "grafana")
				}
			},
		},
		{
			name:    "exec missing value at end",
			args:    []string{"--exec"},
			wantErr: "--exec requires a value",
		},
		{
			name:    "exec space form value eats next flag",
			args:    []string{"--exec", "--env=prod"},
			wantErr: "--exec requires a value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execTarget = "" // reset the package var each iteration, same as cfg being fresh
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

// TestComponentUnitMasked mirrors TestHookdUnitMasked's four-case shape,
// parameterized over the three supervisor-group component names
// (wh2-supervisor-systemd-units) — otelcol/prometheus/grafana share one
// generic predicate (componentUnitMasked), unlike hookd's own dedicated one.
func TestComponentUnitMasked(t *testing.T) {
	orig := componentUnitDir
	t.Cleanup(func() { componentUnitDir = orig })

	for _, name := range []string{"otelcol", "prometheus", "grafana"} {
		t.Run(name, func(t *testing.T) {
			tests := []struct {
				name       string
				setup      func(t *testing.T, path string)
				wantMasked bool
			}{
				{name: "absent unit is not masked", setup: func(t *testing.T, path string) {}},
				{
					name: "regular file is not masked",
					setup: func(t *testing.T, path string) {
						if err := os.WriteFile(path, []byte("[Unit]\n"), 0o644); err != nil {
							t.Fatal(err)
						}
					},
				},
				{
					name: "symlink to /dev/null is masked",
					setup: func(t *testing.T, path string) {
						if err := os.Symlink("/dev/null", path); err != nil {
							t.Fatal(err)
						}
					},
					wantMasked: true,
				},
				{
					name: "symlink to a non-null target is not masked",
					setup: func(t *testing.T, path string) {
						if err := os.Symlink("/etc/hostname", path); err != nil {
							t.Fatal(err)
						}
					},
				},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					componentUnitDir = t.TempDir()
					unitPath := filepath.Join(componentUnitDir, "teamster-"+name+".service")
					tt.setup(t, unitPath)
					got := componentUnitMasked(name)
					if got != tt.wantMasked {
						t.Errorf("componentUnitMasked(%q) = %v, want %v", name, got, tt.wantMasked)
					}
				})
			}
		})
	}
}

// TestValidateExecFlag proves --exec is only ever accepted with the "start"
// subcommand (round-3 review MINOR-2) — parseSupervisorFlags is shared by
// start/stop/status/wms-reset, so without this gate `teamster stop
// --exec=grafana` would render config and exit 0 without stopping anything.
func TestValidateExecFlag(t *testing.T) {
	orig := execTarget
	t.Cleanup(func() { execTarget = orig })

	tests := []struct {
		name       string
		execTarget string
		subcommand string
		wantErr    bool
	}{
		{name: "no --exec, any subcommand is fine", execTarget: "", subcommand: "stop"},
		{name: "--exec with start is fine", execTarget: "prometheus", subcommand: "start"},
		{name: "--exec with stop errors", execTarget: "prometheus", subcommand: "stop", wantErr: true},
		{name: "--exec with status errors", execTarget: "grafana", subcommand: "status", wantErr: true},
		{name: "--exec with wms-reset errors", execTarget: "otelcol", subcommand: "wms-reset", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execTarget = tt.execTarget
			err := validateExecFlag(tt.subcommand)
			if tt.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// stageSupervisorTemplates copies the real shipped otelcol/prometheus/grafana
// render templates into basedir/etc/, mirroring what an install actually
// stages, so TestPrepareComponentExec exercises the real render pipeline
// (renderOtelcolConfig/preparePrometheusConfig/prepareGrafanaConfig) rather
// than a hand-written fixture that could drift from the shipped templates.
func stageSupervisorTemplates(t *testing.T, basedir string) {
	t.Helper()
	repoRoot := filepath.Join("..", "..", "..")
	copies := map[string]string{
		filepath.Join(repoRoot, "skel", "etc", "otelcol.yaml.tmpl"):    filepath.Join(basedir, "etc", "otelcol.yaml.tmpl"),
		filepath.Join(repoRoot, "skel", "etc", "prometheus.yaml.tmpl"): filepath.Join(basedir, "etc", "prometheus.yaml.tmpl"),
		filepath.Join(repoRoot, "skel", "etc", "grafana", "grafana.ini.tmpl"): filepath.Join(
			basedir, "etc", "grafana", "grafana.ini.tmpl"),
		filepath.Join(repoRoot, "skel", "etc", "grafana", "provisioning", "dashboards", "teamster.yaml.tmpl"): filepath.Join(
			basedir, "etc", "grafana", "provisioning", "dashboards", "teamster.yaml.tmpl"),
		filepath.Join(repoRoot, "skel", "etc", "grafana", "provisioning", "datasources", "teamster.yaml.tmpl"): filepath.Join(
			basedir, "etc", "grafana", "provisioning", "datasources", "teamster.yaml.tmpl"),
	}
	for src, dst := range copies {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
	}
}

// TestPrepareComponentExec proves the exec-wrapper's path+argv computation
// matches what StartOtelcol/StartPrometheus/StartGrafana themselves compute
// in supervisor mode (SUPERVISOR-UNITS-DESIGN.md §4) — the config-freshness
// invariant this WU's whole design rests on. Deliberately does not call
// syscall.Exec (prepareComponentExec stops at "compute path and args", the
// testable seam; the actual syscall.Exec call in runSupervisor is a single
// line, reviewed by inspection rather than exercised in-process, since
// calling it for real would replace the test binary's own process image).
func TestPrepareComponentExec(t *testing.T) {
	basedir := t.TempDir()
	stageSupervisorTemplates(t, basedir)

	cfg := config.Default()
	cfg.DataDir = filepath.Join(basedir, "var")
	cfg.HookServerPort = 9125
	cfg.OtelGRPCPort = 4327
	cfg.OtelHTTPPort = 4328
	cfg.OtelCodexHTTPPort = 4329
	cfg.PrometheusPort = 9190
	cfg.PrometheusRetention = "365d"
	cfg.GrafanaPort = 3100

	t.Run("otelcol", func(t *testing.T) {
		binPath, args, err := prepareComponentExec(cfg, "otelcol")
		if err != nil {
			t.Fatalf("prepareComponentExec: %v", err)
		}
		if want := filepath.Join(basedir, "bin", "otelcol-contrib"); binPath != want {
			t.Errorf("binPath = %q, want %q", binPath, want)
		}
		// Assert against otelcolArgs itself, not a restated literal slice
		// (same shape as the prometheus/grafana subtests below,
		// wh2-otelcol-argv-builder): this is the one construction StartOtelcol
		// also calls, so a flag added to one and not the other fails here
		// rather than silently diverging.
		wantArgs := otelcolArgs(filepath.Join(basedir, "etc", "otelcol.yaml"))
		if !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("args = %v, want %v", args, wantArgs)
		}
		if _, err := os.Stat(filepath.Join(basedir, "etc", "otelcol.yaml")); err != nil {
			t.Errorf("otelcol.yaml not rendered: %v", err)
		}
	})

	t.Run("prometheus", func(t *testing.T) {
		binPath, args, err := prepareComponentExec(cfg, "prometheus")
		if err != nil {
			t.Fatalf("prepareComponentExec: %v", err)
		}
		if want := filepath.Join(basedir, "bin", "prometheus"); binPath != want {
			t.Errorf("binPath = %q, want %q", binPath, want)
		}
		// Assert against prometheusArgs itself, not a restated literal slice
		// (round-4 MAJOR, VERIFY-SEXTANT.md §17): this is the one construction
		// StartPrometheus also calls, so a flag added to one and not the other
		// fails here rather than silently diverging.
		configPath := filepath.Join(basedir, "etc", "prometheus.yaml")
		dataDir := filepath.Join(basedir, "var", "prometheus")
		wantArgs := prometheusArgs(cfg, configPath, dataDir)
		if !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("args = %v, want %v", args, wantArgs)
		}
		if _, err := os.Stat(filepath.Join(basedir, "etc", "prometheus.yaml")); err != nil {
			t.Errorf("prometheus.yaml not rendered: %v", err)
		}
	})

	t.Run("prometheus with retention size", func(t *testing.T) {
		cfg2 := cfg
		cfg2.PrometheusRetentionSize = "50GB"
		_, args, err := prepareComponentExec(cfg2, "prometheus")
		if err != nil {
			t.Fatalf("prepareComponentExec: %v", err)
		}
		configPath := filepath.Join(basedir, "etc", "prometheus.yaml")
		dataDir := filepath.Join(basedir, "var", "prometheus")
		wantArgs := prometheusArgs(cfg2, configPath, dataDir)
		if !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("args = %v, want %v", args, wantArgs)
		}
		last := args[len(args)-1]
		if last != "--storage.tsdb.retention.size=50GB" {
			t.Errorf("last arg = %q, want the retention-size flag", last)
		}
	})

	t.Run("grafana", func(t *testing.T) {
		binPath, args, err := prepareComponentExec(cfg, "grafana")
		if err != nil {
			t.Fatalf("prepareComponentExec: %v", err)
		}
		if want := filepath.Join(basedir, "bin", "grafana-server"); binPath != want {
			t.Errorf("binPath = %q, want %q", binPath, want)
		}
		// Assert against grafanaArgs itself, same reasoning as prometheus above.
		iniPath := filepath.Join(basedir, "etc", "grafana", "grafana.ini")
		grafanaHomePath := filepath.Join(basedir, "var", "grafana-home")
		wantArgs := grafanaArgs(iniPath, grafanaHomePath)
		if !reflect.DeepEqual(args, wantArgs) {
			t.Errorf("args = %v, want %v", args, wantArgs)
		}
		if _, err := os.Stat(filepath.Join(basedir, "etc", "grafana", "grafana.ini")); err != nil {
			t.Errorf("grafana.ini not rendered: %v", err)
		}
	})

	t.Run("unknown component errors", func(t *testing.T) {
		if _, _, err := prepareComponentExec(cfg, "bogus"); err == nil {
			t.Fatal("expected an error for an unknown component, got nil")
		}
	})
}

// fakeExecutables writes no-op "systemctl"/"sudo" scripts into a temp dir and
// prepends it to PATH for the duration of the test, restoring PATH on
// cleanup. Used by tests that must prove a code path never shells out to
// either — real systemctl/sudo exist in some CI/dev sandboxes (this one
// included), and a bug that reaches them could hang on a sudo password
// prompt or touch this sandbox's real systemd state; the fakes make that
// structurally impossible while still exercising the exec.Command call
// itself. exitCode 1 by default (mimics "not active"/"unit not found") so a
// caller who checks the exit code sees a realistic failure, not a surprise
// success.
func fakeExecutables(t *testing.T, exitCode int, marker string) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ntouch %q\nexit %d\n", marker, exitCode)
	for _, name := range []string{"systemctl", "sudo"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	origPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPath) })
	os.Setenv("PATH", dir+":"+origPath)
}

// zeroAllPorts sets every port field in cfg to 0. portFor returns 0 for a
// zeroed field, which killByPort and processAlive both treat as "no port to
// check" and return/skip immediately — this is the only way to make it safe
// to call supervisorStop, startSupervisorGroup, or anything else that can
// reach killByPort from a test.
//
// This is not a hypothetical precaution: an earlier version of
// TestSupervisorStopSkipsSystemdManaged called supervisorStop with an
// unmodified config.Default() (HookServerPort: 9125, the real default), and
// killByPort's port-based fallback — which has no basedir/project scoping at
// all, unlike everything TEAMSTER_STORE_DSN/TEAMSTER_HOOK_SERVER_URL guard —
// found and SIGKILLed the live production hookd on this very host (the hub,
// PID 216901, 2026-09-05 15:48:48 UTC; systemd's Restart=on-failure brought
// it back 5s later with no observed lasting damage, but a live incident is a
// live incident). Any test in this package that can reach killByPort must
// zero every port field first.
func zeroAllPorts(cfg *config.Config) {
	cfg.HookServerPort = 0
	cfg.PrometheusPort = 0
	cfg.GrafanaPort = 0
	cfg.OtelGRPCPort = 0
	cfg.OtelHTTPPort = 0
	cfg.OtelCodexHTTPPort = 0
}

// assertNoRealPorts is the asserted precondition the incident above earned:
// zeroAllPorts is a hand-maintained list of config field names, which can
// drift from portFor (start.go) the moment a component's port lookup reads a
// field this list doesn't zero — silently, since nothing would fail. This
// asserts through portFor itself, for the exact five names supervisorStop's
// allComponents iterates, so it cannot drift from the function the kill path
// actually reads: add a sixth component or a new field to the lookup, and
// this still catches it, naming the port it would have killed.
func assertNoRealPorts(t *testing.T, cfg config.Config) {
	t.Helper()
	for _, name := range []string{"health-collector", "grafana", "prometheus", "otelcol", "hookd"} {
		if p := portFor(name, cfg); p != 0 {
			t.Fatalf("precondition: portFor(%q) = %d, want 0 — this test would SIGKILL whatever owns that port", name, p)
		}
	}
}

// TestStartSupervisorGroupMasked proves a masked otelcol unit stays off:
// neither the systemd branch (systemctl is-active/start) nor the supervisor
// fallback (startComponent) is reached — round-4's mask ruling
// (SUPERVISOR-UNITS-DESIGN.md §7): masking means off, no fallback. Uses
// fakeExecutables rather than relying on this sandbox lacking real
// systemctl/sudo (it doesn't).
func TestStartSupervisorGroupMasked(t *testing.T) {
	origDir := componentUnitDir
	t.Cleanup(func() { componentUnitDir = origDir })
	componentUnitDir = t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(componentUnitDir, "teamster-otelcol.service")); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "called")
	fakeExecutables(t, 1, marker)

	basedir := t.TempDir()
	cfg := config.Default()
	zeroAllPorts(&cfg)
	cfg.DataDir = filepath.Join(basedir, "var")
	cfg.HookdMode = "systemd"
	cfg.OtelcolMode = "install"
	cfg.PrometheusMode = "none" // isolate: only exercise otelcol's masked path
	cfg.GrafanaMode = "none"

	assertNoRealPorts(t, cfg)
	if err := startSupervisorGroup(context.Background(), cfg); err != nil {
		t.Fatalf("startSupervisorGroup returned an error for a masked-off component: %v", err)
	}

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("masked otelcol invoked systemctl/sudo — the mask check did not short-circuit before the systemd branch")
	}
	if _, err := os.Stat(filepath.Join(basedir, "var", "pids", "otelcol.pid")); !os.IsNotExist(err) {
		t.Fatal("masked otelcol wrote a PID file — there is no supervisor fallback under this design")
	}
}

// TestSupervisorStopSkipsSystemdManaged proves the PID-file stop path is
// skipped for a systemd-mode-install component, leaving its (fake, stale)
// PID file untouched — the fix for the killByPort/Restart=on-failure race
// (SUPERVISOR-UNITS-DESIGN.md §7). If the skip regresses, stopByPidFile would
// read the stale PID, fail to signal a nonexistent process, and delete the
// file via its own cleanup path, which this test would catch.
func TestSupervisorStopSkipsSystemdManaged(t *testing.T) {
	fakeExecutables(t, 0, filepath.Join(t.TempDir(), "called"))

	basedir := t.TempDir()
	cfg := config.Default()
	zeroAllPorts(&cfg)
	cfg.DataDir = filepath.Join(basedir, "var")
	cfg.HookdMode = "systemd"
	cfg.OtelcolMode = "install"
	cfg.PrometheusMode = "none"
	cfg.GrafanaMode = "none"

	pidDir := filepath.Join(basedir, "var", "pids")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(pidDir, "otelcol.pid")
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	assertNoRealPorts(t, cfg)
	if err := supervisorStop(cfg); err != nil {
		t.Fatalf("supervisorStop: %v", err)
	}

	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("otelcol.pid should be left untouched (PID-file loop skipped for systemd-mode-install): %v", err)
	}
}

// TestComponentSupervisorManaged asserts componentSupervisorManaged against
// every mode this repo defines (config.go: "install | external | managed |
// none") crossed with every cfg.HookdMode value, not just the one
// combination ("install" + "systemd") the original stop-side guard checked.
// That guard was wrong for every other combination — astrolabe's round-2
// finding — so this table exists specifically to make a future regression to
// that shape fail here instead of reaching killByPort.
func TestComponentSupervisorManaged(t *testing.T) {
	modes := []string{"install", "external", "managed", "none"}
	hookdModes := []string{"systemd", "supervisor", "external"}
	examined := 0
	for _, mode := range modes {
		for _, hookdMode := range hookdModes {
			examined++
			cfg := config.Default()
			cfg.OtelcolMode = mode
			cfg.HookdMode = hookdMode
			want := mode == "install" && hookdMode != "systemd"
			got := componentSupervisorManaged("otelcol", cfg)
			if got != want {
				t.Errorf("componentSupervisorManaged(otelcol, mode=%q, HookdMode=%q) = %v, want %v", mode, hookdMode, got, want)
			}
		}
	}
	if examined != len(modes)*len(hookdModes) {
		t.Fatalf("examined %d combinations, want %d", examined, len(modes)*len(hookdModes))
	}
}

// TestHookdSupervisorManaged is componentSupervisorManaged's table for
// hookd's simpler (single-field) eligibility test.
func TestHookdSupervisorManaged(t *testing.T) {
	tests := []struct {
		hookdMode string
		want      bool
	}{
		{"systemd", false},
		{"external", false},
		{"supervisor", true},
	}
	examined := 0
	for _, tt := range tests {
		examined++
		cfg := config.Default()
		cfg.HookdMode = tt.hookdMode
		if got := hookdSupervisorManaged(cfg); got != tt.want {
			t.Errorf("hookdSupervisorManaged(HookdMode=%q) = %v, want %v", tt.hookdMode, got, tt.want)
		}
	}
	if examined != len(tests) {
		t.Fatalf("examined %d cases, want %d", examined, len(tests))
	}
}

// TestSupervisorStopSkipsNonSupervisedComponents proves the round-2 gap
// itself is closed: an otelcol/prometheus/grafana component in "external"
// mode (astrolabe's example — the hub's own shared Grafana per this repo's
// CLAUDE.md) or "managed"/"none" mode never reaches stopByPidFile, under
// EVERY cfg.HookdMode value, not only "systemd" — the original guard's sole
// covered case. A stale PID file for the component survives in every case,
// same proof shape as TestSupervisorStopSkipsSystemdManaged, which continues
// to cover the "install" + "systemd" combination this test does not repeat.
func TestSupervisorStopSkipsNonSupervisedComponents(t *testing.T) {
	modes := []string{"external", "managed", "none"}
	hookdModes := []string{"systemd", "supervisor", "external"}
	examined := 0
	for _, mode := range modes {
		for _, hookdMode := range hookdModes {
			examined++
			t.Run(mode+"/"+hookdMode, func(t *testing.T) {
				fakeExecutables(t, 0, filepath.Join(t.TempDir(), "called"))

				basedir := t.TempDir()
				cfg := config.Default()
				zeroAllPorts(&cfg)
				cfg.DataDir = filepath.Join(basedir, "var")
				cfg.HookdMode = hookdMode
				cfg.OtelcolMode = mode
				cfg.PrometheusMode = "none"
				cfg.GrafanaMode = "none"

				pidDir := filepath.Join(basedir, "var", "pids")
				if err := os.MkdirAll(pidDir, 0o755); err != nil {
					t.Fatal(err)
				}
				pidPath := filepath.Join(pidDir, "otelcol.pid")
				if err := os.WriteFile(pidPath, []byte("999999\n"), 0o644); err != nil {
					t.Fatal(err)
				}

				assertNoRealPorts(t, cfg)
				if err := supervisorStop(cfg); err != nil {
					t.Fatalf("supervisorStop: %v", err)
				}

				if _, err := os.Stat(pidPath); err != nil {
					t.Fatalf("otelcol.pid should be left untouched (mode=%q, HookdMode=%q): %v", mode, hookdMode, err)
				}
			})
		}
	}
	if examined != len(modes)*len(hookdModes) {
		t.Fatalf("examined %d combinations, want %d", examined, len(modes)*len(hookdModes))
	}
}

// TestSupervisorStopSkipsHookdPidFallback proves the PID-file stop path is
// skipped for hookd whenever teamster never wrote its PID file in the first
// place — "systemd" mode (started via systemctl enable --now) and "external"
// mode (never started by teamster at all), symmetric with
// supervisorStart's own hookd handling (wh2-stop-killbyport-systemd-race).
// Without the skip, stopByPidFile falls through to killByPort's
// SIGKILL-by-port fallback — for "systemd" this is the exact incident that
// SIGKILLed production hookd on 2026-09-05 (Restart=on-failure raced the
// kill); for "external" it would kill a process teamster never started.
// Leaves a (fake, stale) hookd.pid untouched, same proof shape as
// TestSupervisorStopSkipsSystemdManaged above.
//
// "supervisor" is the negative control: hookd IS PID-file-managed under this
// mode (startComponent writes the file), so the skip must NOT fire — proven
// here by the opposite outcome, stopByPidFile's own ESRCH-on-a-dead-PID
// branch removing the file, same as it always has.
func TestSupervisorStopSkipsHookdPidFallback(t *testing.T) {
	cases := []struct {
		mode            string
		pidFileSurvives bool
	}{
		{mode: "systemd", pidFileSurvives: true},
		{mode: "external", pidFileSurvives: true},
		{mode: "supervisor", pidFileSurvives: false},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			fakeExecutables(t, 0, filepath.Join(t.TempDir(), "called"))

			basedir := t.TempDir()
			cfg := config.Default()
			zeroAllPorts(&cfg)
			cfg.DataDir = filepath.Join(basedir, "var")
			cfg.HookdMode = tc.mode
			cfg.OtelcolMode = "none"
			cfg.PrometheusMode = "none"
			cfg.GrafanaMode = "none"

			pidDir := filepath.Join(basedir, "var", "pids")
			if err := os.MkdirAll(pidDir, 0o755); err != nil {
				t.Fatal(err)
			}
			pidPath := filepath.Join(pidDir, "hookd.pid")
			if err := os.WriteFile(pidPath, []byte("999999\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			assertNoRealPorts(t, cfg)
			if err := supervisorStop(cfg); err != nil {
				t.Fatalf("supervisorStop: %v", err)
			}

			_, statErr := os.Stat(pidPath)
			survived := statErr == nil
			if survived != tc.pidFileSurvives {
				t.Fatalf("hookd.pid survived = %v, want %v (mode %q); stat err: %v", survived, tc.pidFileSurvives, tc.mode, statErr)
			}
		})
	}
}

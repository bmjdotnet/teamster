package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
)

// shutdownRequested is set per-component name before killing. The crashloop
// goroutine checks this flag and exits instead of restarting.
var shutdownRequested sync.Map // key: string component name, value: bool

// crashloopBackoffs is the delay sequence between successive restart attempts.
var crashloopBackoffs = []time.Duration{0, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}

// runSupervisor is the entry point for `teamster start|stop|status`.
// main.go calls this when os.Args[1] is one of those subcommands.
func runSupervisor(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: teamster <start|stop|status|wms-reset> [flags]\n")
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	logging.Init("teamster")

	subcommand := args[0]
	rest := args[1:]

	if err := parseSupervisorFlags(rest, &cfg); err != nil {
		slog.Error("flag parse failed", "error", err)
		os.Exit(1)
	}

	// --exec is the systemd exec-wrapper entry point (wh2-supervisor-systemd-units):
	// a unit's ExecStart runs `teamster start --exec=<name>`, which loads config,
	// renders it, and syscall.Execs the real binary in place of this process. It
	// must only ever be reached via the "start" subcommand — parseSupervisorFlags
	// is shared by start/stop/status/wms-reset, so without this check
	// `teamster stop --exec=grafana` would render config and exit 0 without
	// stopping anything (round-3 review MINOR-2; no-silent-failures, same
	// standard parseSupervisorFlags already holds itself to for unknown flags).
	if err := validateExecFlag(subcommand); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	if execTarget != "" {
		binPath, execArgs, err := prepareComponentExec(cfg, execTarget)
		if err != nil {
			slog.Error("exec prepare failed", "component", execTarget, "error", err)
			os.Exit(1)
		}
		// syscall.Exec, never exec.Command: process replacement, not fork. The
		// PID must not change — that's the entire reason systemd's Type=simple
		// MainPID, the unit's cgroup, and Restart=on-failure's view of the real
		// binary's own exit status all stay correct without any extra plumbing.
		// exec.Command().Run() here would leave this wrapper as MainPID with the
		// real binary as an untracked child, so systemd would watch the wrong
		// process and a crash of the real binary might not trigger Restart= at
		// all. Reviewed explicitly as the one line in this design most worth
		// checking (VERIFY-SEXTANT.md §7/§9).
		env := os.Environ()
		if err := syscall.Exec(binPath, append([]string{binPath}, execArgs...), env); err != nil {
			slog.Error("exec failed", "component", execTarget, "binary", binPath, "error", err)
			os.Exit(1)
		}
		// unreachable on success — syscall.Exec replaced this process's image.
		// Side effect, named rather than left for someone to discover:
		// syscall.Exec never touches stdout/stderr before replacing the
		// process image, so the real binary's output goes wherever systemd
		// sends a Type=simple unit's output by default — the journal
		// (`journalctl -u teamster-<name>`) — not var/logs/<name>.log, which
		// StartOtelcol/StartPrometheus/StartGrafana write to in supervisor
		// mode. A systemd-managed host loses that log file for these three;
		// an operator used to tailing it needs to switch to journalctl.
		return
	}

	switch subcommand {
	case "start":
		if err := supervisorStart(cfg); err != nil {
			slog.Error("start failed", "error", err)
			os.Exit(1)
		}
	case "stop":
		if err := supervisorStop(cfg); err != nil {
			slog.Error("stop failed", "error", err)
			os.Exit(1)
		}
	case "status":
		supervisorStatus(cfg)
	case "wms-reset":
		if err := wmsReset(cfg); err != nil {
			slog.Error("wms-reset failed", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (want start, stop, status, wms-reset)\n", subcommand)
		os.Exit(1)
	}
}

// settingsEnvReader is the function used to read env vars from settings.json.
// Replaced in tests to avoid host-state dependency.
var settingsEnvReader = readSettingsEnv

// hookdUnitPath is teamster-hookd.service's real systemd unit-file path. A
// var, not a const, so hookdUnitMasked is testable against a temp path
// without root or a real systemd — replaced in tests the same way
// settingsEnvReader is above.
var hookdUnitPath = "/etc/systemd/system/teamster-hookd.service"

// execTarget is set by --exec=<name> in parseSupervisorFlags — the
// wh2-supervisor-systemd-units exec-wrapper entry point (see runSupervisor).
var execTarget string

// validateExecFlag reports an error if --exec was set outside the "start"
// subcommand. parseSupervisorFlags is shared by start/stop/status/wms-reset,
// so without this check `teamster stop --exec=grafana` would render config
// and exit 0 without stopping anything (round-3 review MINOR-2). Separated
// from runSupervisor's os.Exit(1) call site so the gating logic itself is
// unit-testable.
func validateExecFlag(subcommand string) error {
	if execTarget != "" && subcommand != "start" {
		return fmt.Errorf("--exec is only valid with the start subcommand, got %q", subcommand)
	}
	return nil
}

// componentUnitDir is where teamster's per-component systemd units are
// installed. A var, not a const, so componentUnitMasked is testable against
// a temp path without root or a real systemd, the same way hookdUnitPath is
// above. Deliberately a sibling of hookdUnitPath, not a generalization of
// it — hookdUnitMasked/hookdUnitPath are already-reviewed, already-committed
// code (VERIFY-TOUCHSTONE.md §17/§18); refactoring them into this for DRY's
// sake would be unrequested cleanup on an unrelated WU.
var componentUnitDir = "/etc/systemd/system"

// componentUnitMasked reports whether teamster-<name>.service is masked — the
// same /dev/null-symlink predicate as hookdUnitMasked and lib/installrunner.sh's
// unit_is_masked, generalized to any of the three supervisor-group unit names
// (otelcol/prometheus/grafana). Masking one of these means the component is
// off on this host — no fallback to supervisor mode (operator ruling,
// SUPERVISOR-UNITS-DESIGN.md §7): a mask is the operator's instruction, and
// 2f59a23's doctrine is that nothing here ever argues with it.
func componentUnitMasked(name string) bool {
	target, err := os.Readlink(filepath.Join(componentUnitDir, "teamster-"+name+".service"))
	return err == nil && target == "/dev/null"
}

// hookdUnitMasked reports whether teamster-hookd.service is masked — a
// symlink to /dev/null at its unit-file path, the same check
// lib/installrunner.sh's unit_is_masked uses (never `systemctl is-enabled`,
// which cannot distinguish "masked" from merely "disabled"). Readlink
// itself errors on anything that isn't a symlink (absent path, regular
// file, directory), so a single call covers every non-masked case.
func hookdUnitMasked() bool {
	target, err := os.Readlink(hookdUnitPath)
	return err == nil && target == "/dev/null"
}

// parseSupervisorFlags parses flags common to start/stop/status. Accepts both
// --flag=VALUE and --flag VALUE forms for value-taking flags; errors on
// unknown args (no silent drop — see [[no-silent-failures]]).
func parseSupervisorFlags(args []string, cfg *config.Config) error {
	requireValue := func(flag, next string) error {
		if next == "" || strings.HasPrefix(next, "--") {
			return fmt.Errorf("%s requires a value", flag)
		}
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--systemd-hookd":
			cfg.HookdMode = "systemd"
		case a == "--supervisor-hookd":
			cfg.HookdMode = "supervisor"
		case strings.HasPrefix(a, "--hookd-mode="):
			cfg.HookdMode = strings.TrimPrefix(a, "--hookd-mode=")
		case a == "--hookd-mode":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", a)
			}
			if err := requireValue(a, args[i+1]); err != nil {
				return err
			}
			cfg.HookdMode = args[i+1]
			i++
		case strings.HasPrefix(a, "--env="):
			cfg.Env = strings.TrimPrefix(a, "--env=")
		case a == "--env":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", a)
			}
			if err := requireValue(a, args[i+1]); err != nil {
				return err
			}
			cfg.Env = args[i+1]
			i++
		case strings.HasPrefix(a, "--prometheus-retention="):
			cfg.PrometheusRetention = strings.TrimPrefix(a, "--prometheus-retention=")
		case a == "--prometheus-retention":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", a)
			}
			if err := requireValue(a, args[i+1]); err != nil {
				return err
			}
			cfg.PrometheusRetention = args[i+1]
			i++
		case strings.HasPrefix(a, "--prometheus-retention-size="):
			cfg.PrometheusRetentionSize = strings.TrimPrefix(a, "--prometheus-retention-size=")
		case a == "--prometheus-retention-size":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", a)
			}
			if err := requireValue(a, args[i+1]); err != nil {
				return err
			}
			cfg.PrometheusRetentionSize = args[i+1]
			i++
		case strings.HasPrefix(a, "--exec="):
			execTarget = strings.TrimPrefix(a, "--exec=")
		case a == "--exec":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", a)
			}
			if err := requireValue(a, args[i+1]); err != nil {
				return err
			}
			execTarget = args[i+1]
			i++
		case a == "--live":
			statusLive = true
		default:
			return fmt.Errorf("unknown argument: %s", a)
		}
	}

	if cfg.HookdMode != "systemd" && cfg.HookdMode != "supervisor" && cfg.HookdMode != "external" {
		if v := settingsEnvReader("TEAMSTER_HOOKD_MODE"); v != "" {
			cfg.HookdMode = v
		}
		if cfg.HookdMode == "" {
			cfg.HookdMode = "systemd"
		}
	}

	return nil
}

// supervisorStart launches all selected bundle components, then blocks until
// SIGTERM or SIGINT so crashloop goroutines stay alive for the session.
// On first invocation it re-execs itself as a background daemon so the caller
// (wizard, shell) is not blocked.
func supervisorStart(cfg config.Config) error {
	if os.Getenv("_TEAMSTER_SUPERVISOR") != "1" {
		// Parent: re-exec as a background daemon with a readiness pipe.
		readyR, readyW, err := os.Pipe()
		if err != nil {
			return fmt.Errorf("create readiness pipe: %w", err)
		}

		exe, _ := os.Executable()
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Env = append(os.Environ(), "_TEAMSTER_SUPERVISOR=1")
		cmd.ExtraFiles = []*os.File{readyW} // fd 3 in the child

		basedir := prometheusBasedir(cfg)
		if basedir == "." || basedir == "" {
			home, _ := os.UserHomeDir()
			basedir = filepath.Join(home, "teamster")
		}
		cmd.Dir = basedir
		logPath := filepath.Join(basedir, "var", "logs", "supervisor.log")
		_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
		logFile, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		cmd.Stdout = logFile
		cmd.Stderr = logFile

		setSetsid(cmd)
		if err := cmd.Start(); err != nil {
			readyR.Close()
			readyW.Close()
			return err
		}
		readyW.Close()

		readyCh := make(chan string, 1)
		go func() {
			scanner := bufio.NewScanner(readyR)
			if scanner.Scan() {
				readyCh <- scanner.Text()
			} else {
				readyCh <- ""
			}
			readyR.Close()
		}()

		select {
		case msg := <-readyCh:
			if strings.HasPrefix(msg, "ready") {
				fmt.Printf("supervisor: daemonized (pid %d, log %s)\n", cmd.Process.Pid, logPath)
				os.Exit(0)
			}
			slog.Error("supervisor child reported unexpected message", "msg", msg)
			os.Exit(1)
		case <-time.After(30 * time.Second):
			slog.Error("supervisor child did not become ready within 30s")
			os.Exit(1)
		}
	}
	// Child (supervisor) continues below.

	basedir := prometheusBasedir(cfg)
	if err := os.MkdirAll(filepath.Join(basedir, "var", "pids"), 0o755); err != nil {
		return err
	}

	// Kill any existing supervisor before starting. After a VM revert the PID
	// file may point at a dead or reused process; pgrep is the fallback.
	supervisorPidPath := filepath.Join(basedir, "var", "pids", "teamster.pid")
	if data, err := os.ReadFile(supervisorPidPath); err == nil {
		if oldPid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && oldPid != os.Getpid() {
			if err := syscall.Kill(oldPid, 0); err == nil {
				slog.Info("killing existing supervisor", "pid", oldPid)
				_ = syscall.Kill(oldPid, syscall.SIGTERM)
				for i := 0; i < 30; i++ {
					time.Sleep(100 * time.Millisecond)
					if err := syscall.Kill(oldPid, 0); err != nil {
						break
					}
				}
				_ = syscall.Kill(oldPid, syscall.SIGKILL)
			}
		}
	}
	// Fallback: pgrep for any other supervisor process we missed.
	if out, err := exec.Command("pgrep", "-f", "teamster start --supervisor").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid != os.Getpid() {
				slog.Info("killing orphan supervisor via pgrep", "pid", pid)
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}

	// Write the supervisor's own PID file so `teamster stop` can kill it.
	_ = os.WriteFile(supervisorPidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	defer os.Remove(supervisorPidPath)

	ctx := context.Background()

	if cfg.HookdMode == "systemd" {
		if hookdUnitMasked() {
			// Mirrors lib/installrunner.sh's unit_is_masked/masked_unit_notice
			// guard: `systemctl enable` refuses on a masked unit ("Unit ...
			// is masked") without ever unmasking it. `teamster clone` never
			// masks hookd itself (only sweep/backup/review-sweep, R11), so
			// the only way to reach this branch masked is an operator
			// hand-masking hookd directly — but when that happens, treating
			// it as a hard error would abort `teamster start` instead of
			// leaving the deliberately-masked unit alone.
			fmt.Println("hookd: left masked (systemd) — not enabling a unit that is deliberately masked")
		} else {
			// enable --now, not a plain start: hookd was the only systemd-managed
			// unit `teamster start` never enabled, so it did not survive a
			// reboot (wh2-hookd-enable-on-install). Idempotent — a no-op if
			// already enabled/active.
			cmd := exec.Command("sudo", "systemctl", "enable", "--now", "teamster-hookd")
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("hookd: systemctl enable --now failed: %w", err)
			}
			fmt.Println("hookd: enabled + started (systemd)")
		}
	} else if hookdSupervisorManaged(cfg) {
		if err := startComponent(ctx, cfg, "hookd"); err != nil {
			return fmt.Errorf("hookd: %w", err)
		}
	}

	if err := startSupervisorGroup(ctx, cfg); err != nil {
		return err
	}

	// token-scraper, health-collector, codex-context-subscriber: systemd-managed
	// under "systemd" hookd mode, supervisor-managed under "supervisor".
	for _, name := range []string{"token-scraper", "health-collector", "codex-context-subscriber"} {
		svc := "teamster-" + name
		if cfg.HookdMode == "systemd" {
			if err := exec.Command("systemctl", "is-active", "--quiet", svc).Run(); err == nil {
				fmt.Printf("%s: already running (systemd)\n", name)
			} else {
				cmd := exec.Command("sudo", "systemctl", "start", svc)
				if err := cmd.Run(); err != nil {
					slog.Warn("systemctl start failed", "service", svc, "error", err)
				} else {
					fmt.Printf("%s: started (systemd)\n", name)
				}
			}
		} else if cfg.HookdMode == "supervisor" {
			if processAlive(name, cfg) {
				fmt.Printf("%s: already running\n", name)
			} else if err := startComponent(ctx, cfg, name); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}

	// Final verification — confirm all expected services are actually alive.
	for _, name := range []string{"hookd", "otelcol", "prometheus", "grafana", "health-collector"} {
		if name == "hookd" && cfg.HookdMode == "systemd" {
			continue
		}
		if name == "hookd" && cfg.HookdMode == "external" {
			continue
		}
		if name == "health-collector" {
			if cfg.HookdMode != "supervisor" {
				continue
			}
		} else if name != "hookd" {
			if modeFor(name, cfg) != "install" {
				continue
			}
			// otelcol/prometheus/grafana under systemd mode: already confirmed
			// via systemctl is-active in the start loop above when unmasked, and
			// deliberately off (no PID file to check) when masked — either way,
			// PID-based processAlive below doesn't apply (wh2-supervisor-systemd-units).
			if cfg.HookdMode == "systemd" {
				continue
			}
		}
		if !processAlive(name, cfg) {
			return fmt.Errorf("post-start verification failed: %s is not running", name)
		}
	}

	// Signal readiness to the parent via fd 3 (the pipe).
	if readyPipe := os.NewFile(3, "readiness-pipe"); readyPipe != nil {
		_, _ = readyPipe.WriteString("ready\n")
		readyPipe.Close()
	}

	// Block until signaled. Crashloop goroutines continue running.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
	return nil
}

// startSupervisorGroup brings up otelcol/prometheus/grafana: systemd-managed
// under "systemd" hookd mode (wh2-supervisor-systemd-units), unless the
// operator has masked the unit, in which case the component stays off — no
// fallback to supervisor mode (ruled: a mask is the operator's instruction,
// and 2f59a23's doctrine is that nothing here ever argues with it;
// `hookd_mode: supervisor` is the actual mechanism for "run this without
// systemd", not a mask). Otherwise supervisor-managed exactly as before this
// WU. Extracted from supervisorStart as its own function so the masked/
// systemd/supervisor branch decision is unit-testable without exercising
// supervisorStart's daemonizing re-exec and final blocking wait.
func startSupervisorGroup(ctx context.Context, cfg config.Config) error {
	for _, name := range []string{"otelcol", "prometheus", "grafana"} {
		if modeFor(name, cfg) != "install" {
			continue
		}
		if !componentSupervisorManaged(name, cfg) {
			// componentSupervisorManaged is false here only because
			// cfg.HookdMode == "systemd" (mode == "install" already holds) —
			// systemd-managed, never reaches startComponent below.
			svc := "teamster-" + name
			if componentUnitMasked(name) {
				fmt.Printf("%s: left masked: %s.service — component stays off on this host; unmask to run it under systemd, or set hookd_mode: supervisor\n", name, svc)
				continue
			}
			if err := exec.Command("systemctl", "is-active", "--quiet", svc).Run(); err == nil {
				fmt.Printf("%s: already running (systemd)\n", name)
				continue
			}
			// Plain start, not enable --now: unlike hookd, these units are
			// already enabled at install time (lib/installrunner.sh), so
			// this is only "bring it up if it isn't", not hookd's historical
			// "self-heal an enable that was never done anywhere".
			if err := exec.Command("sudo", "systemctl", "start", svc).Run(); err != nil {
				return fmt.Errorf("%s: systemctl start failed: %w", name, err)
			}
			fmt.Printf("%s: started (systemd)\n", name)
			continue
		}
		if processAlive(name, cfg) {
			fmt.Printf("%s: already running\n", name)
			continue
		}
		if err := startComponent(ctx, cfg, name); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// componentSupervisorManaged reports whether name (one of otelcol/prometheus/
// grafana) is a component THIS teamster process would itself start and
// PID-file-track — mode == "install" and not systemd-managed.
// startSupervisorGroup calls this directly to decide whether to reach
// startComponent, and supervisorStop's stop-side check calls the same
// function to decide whether to reach stopByPidFile/killByPort — so the two
// cannot independently re-derive, and drift from, each other's eligibility
// test (round-3 review MAJOR: an earlier version of this comment claimed
// that guarantee while startSupervisorGroup still reimplemented the check
// inline instead of calling the predicate; the comment was aspirational, not
// true, until this fix). The shape wh2-otelcol-argv-builder closed for argv
// construction, applied here to an eligibility predicate instead
// (wh2-stop-killbyport-systemd-race, round 2 finding: the original stop-side
// guard only excluded the systemd+install combination, leaving
// "external"/"managed"/"none" modes — and "install" under every OTHER
// HookdMode value it hadn't been told about — silently exposed; portFor
// returns the configured port regardless of mode, so any of those
// combinations reaches killByPort with no ownership check at all).
func componentSupervisorManaged(name string, cfg config.Config) bool {
	return modeFor(name, cfg) == "install" && cfg.HookdMode != "systemd"
}

// hookdSupervisorManaged is componentSupervisorManaged's counterpart for
// hookd, whose eligibility is simpler (no separate per-component mode field —
// cfg.HookdMode itself is the mode). supervisorStart's own three-way hookd
// branch (systemd/supervisor/external) calls this directly to decide whether
// to reach startComponent, and supervisorStop's stop-side check calls the
// same function to decide whether to reach stopByPidFile/killByPort — same
// round-3 review MAJOR as componentSupervisorManaged above: this predicate
// used to only be claimed to "mirror" the start branch, not actually called
// by it.
func hookdSupervisorManaged(cfg config.Config) bool {
	return cfg.HookdMode == "supervisor"
}

// supervisorStop stops all supervised components. It kills the supervisor
// process first (which signals its handler to stop children), then stops
// systemd-managed services through systemd itself, then directly stops
// whatever remains by PID file — handling orphans from a dead supervisor.
func supervisorStop(cfg config.Config) error {
	// 1. Kill the supervisor process itself (signals its handler to stop children).
	supervisorPidPath := filepath.Join(prometheusBasedir(cfg), "var", "pids", "teamster.pid")
	if data, err := os.ReadFile(supervisorPidPath); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			time.Sleep(2 * time.Second)
		}
	}

	// 2. Stop systemd-managed services FIRST, through systemd's own stop path
	// — before any PID-file/killByPort fallback gets a chance to run at all
	// (wh2-stop-killbyport-systemd-race). The per-component eligibility
	// check in step 3 below is the definitive guard — it decides whether
	// killByPort is even reachable for a given component — but this loop
	// running first means a clean `systemctl stop` (which does not trigger
	// Restart=on-failure the way killByPort's ownerless SIGKILL does) is
	// what actually stops a systemd-managed unit, and any future gap in
	// step 3's eligibility check fails closed (killByPort's own port-dial
	// precondition finds nothing listening) rather than open (a live kill).
	// Unconditional and harmless where a named unit doesn't exist or isn't
	// how this host actually runs that component (`sudo systemctl stop` on
	// a nonexistent/inactive unit just errors, discarded) — may have been
	// started under a different mode, or migrated between versions.
	for _, svc := range []string{
		"teamster-hookd", "teamster-token-scraper", "teamster-codex-context-subscriber", "teamster-health-collector",
		"teamster-otelcol", "teamster-prometheus", "teamster-grafana",
	} {
		_ = exec.Command("sudo", "systemctl", "stop", svc).Run()
	}

	// 3. Directly stop whatever teamster's own supervisor actually manages,
	// by PID file — handles orphans from a dead supervisor. Skip any
	// component this process would never have started itself: portFor
	// returns a configured port regardless of mode, so falling through to
	// killByPort for a component with no PID file (every mode besides a
	// genuinely supervisor-managed "install") has no ownership check of any
	// kind and can SIGKILL an unrelated process holding that port — this
	// happened to production hookd on 2026-09-05, and the same mechanism
	// reaches an "external"-mode Grafana (e.g. the hub's own shared
	// instance, per this repo's own CLAUDE.md) exactly as easily.
	// token-scraper is systemd-managed now, not part of this list.
	allComponents := []string{"health-collector", "grafana", "prometheus", "otelcol", "hookd"}
	var errs []string
	for _, name := range allComponents {
		shutdownRequested.Store(name, true)
		if name == "hookd" {
			if !hookdSupervisorManaged(cfg) {
				// "systemd" and "external" HookdMode: no PID file for hookd
				// either way (supervisorStart only calls startComponent, which
				// writes one, in "supervisor" mode). Step 2 above is what
				// actually stops a systemd-managed hookd; an "external" hookd
				// is not stopped by this function at all beyond step 2's
				// harmless, likely-no-op attempt (teamster never started it).
				continue
			}
		} else if name == "grafana" || name == "prometheus" || name == "otelcol" {
			if !componentSupervisorManaged(name, cfg) {
				// Any mode other than a genuinely supervisor-managed "install"
				// (systemd-managed "install", "external", "managed", "none")
				// never gets a PID file from teamster — step 2 above is the
				// correct path for the systemd case, and the component is
				// simply not teamster's to stop at all in the other three.
				continue
			}
		}
		if err := stopByPidFile(name, cfg); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}

	_ = os.Remove(supervisorPidPath)

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// wmsReset stops all services, deletes the WMS database, and restarts.
func wmsReset(cfg config.Config) error {
	if err := supervisorStop(cfg); err != nil {
		slog.Warn("wms-reset: stop had errors, continuing", "error", err)
	}

	basedir := prometheusBasedir(cfg)
	for _, name := range []string{"wms.db", "wms.db-wal", "wms.db-shm"} {
		p := filepath.Join(basedir, "var", name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}

	if err := supervisorStart(cfg); err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	fmt.Println("WMS database reset")
	return nil
}

// prepareComponentExec loads config, renders it, and computes the binary
// path + argv for `teamster start --exec=<name>` (wh2-supervisor-systemd-units'
// systemd exec-wrapper) to syscall.Exec into. name is one of
// otelcol/prometheus/grafana — the same three components starterFor's
// systemd-managed branch covers.
//
// The rendering step reuses the exact same functions StartOtelcol/
// StartPrometheus/StartGrafana call in supervisor mode (renderOtelcolConfig
// directly; preparePrometheusConfig/prepareGrafanaConfig for the two that
// also need directory creation and, for grafana, credential handling — see
// prometheus.go/grafana.go). binPath/argv construction below calls
// otelcolArgs/prometheusArgs/grafanaArgs (otelcol.go/prometheus.go/grafana.go)
// — the ONE construction of each component's command line, also called by
// StartOtelcol/StartPrometheus/StartGrafana in supervisor mode, so a flag
// added to one path can't silently miss the other (wh2-otelcol-argv-builder,
// round-4 grant extension over the original prepare-step-only extraction;
// see prometheusArgs's own doc comment for why this used to be a duplicated,
// drift-prone construction and no longer is).
func prepareComponentExec(cfg config.Config, name string) (binPath string, args []string, err error) {
	basedir := prometheusBasedir(cfg) // DataDir = <basedir>/var; parent = basedir; same for all three
	switch name {
	case "otelcol":
		// otelcolArgs (otelcol.go) is the ONE construction of this argv,
		// shared with StartOtelcol — wh2-otelcol-argv-builder, follow-up to
		// wh2-supervisor-systemd-units's round-4 grant extension.
		tmplPath := filepath.Join(basedir, "etc", "otelcol.yaml.tmpl")
		cfgPath := filepath.Join(basedir, "etc", "otelcol.yaml")
		if err := renderOtelcolConfig(cfg, tmplPath, cfgPath); err != nil {
			return "", nil, fmt.Errorf("otelcol: render config: %w", err)
		}
		binPath = filepath.Join(basedir, "bin", "otelcol-contrib")
		args = otelcolArgs(cfgPath)
		return binPath, args, nil

	case "prometheus":
		// prometheusArgs (prometheus.go) is the ONE construction of this argv,
		// shared with StartPrometheus — round-4 grant extension over the
		// original prepare-step-only extraction.
		configPath := filepath.Join(basedir, "etc", "prometheus.yaml")
		dataDir, err := preparePrometheusConfig(cfg)
		if err != nil {
			return "", nil, err
		}
		binPath = filepath.Join(basedir, "bin", "prometheus")
		args = prometheusArgs(cfg, configPath, dataDir)
		return binPath, args, nil

	case "grafana":
		// grafanaArgs (grafana.go) is the ONE construction of this argv,
		// shared with StartGrafana — round-4 grant extension.
		if err := prepareGrafanaConfig(cfg); err != nil {
			return "", nil, err
		}
		iniPath := filepath.Join(basedir, "etc", "grafana", "grafana.ini")
		grafanaHomePath := filepath.Join(basedir, "var", "grafana-home")
		binPath = filepath.Join(basedir, "bin", "grafana-server")
		args = grafanaArgs(iniPath, grafanaHomePath)
		return binPath, args, nil

	default:
		return "", nil, fmt.Errorf("prepareComponentExec: unknown component %q", name)
	}
}

// starterFor returns the start function for the named component.
func starterFor(name string) func(context.Context, config.Config) (*exec.Cmd, error) {
	switch name {
	case "hookd":
		return startHookd
	case "otelcol":
		return StartOtelcol
	case "prometheus":
		return StartPrometheus
	case "grafana":
		return StartGrafana
	case "token-scraper":
		return startTokenScraper
	case "health-collector":
		return startHealthCollector
	default:
		return nil
	}
}

// startComponent starts a named component, waits for its port to bind, then
// launches a crashloop goroutine that restarts it on unexpected exit.
func startComponent(ctx context.Context, cfg config.Config, name string) error {
	starter := starterFor(name)
	if starter == nil {
		return fmt.Errorf("unknown component %q", name)
	}

	cmd, err := starter(ctx, cfg)
	if err != nil {
		return err
	}
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("no process returned")
	}

	timeout := 10 * time.Second
	if name == "grafana" {
		timeout = 60 * time.Second
	}
	if err := waitForPort(name, cfg, timeout); err != nil {
		return err
	}
	fmt.Printf("%s: started (pid %d)\n", name, cmd.Process.Pid)

	// Crashloop supervisor goroutine — restarts on unexpected exit.
	go func() {
		for attempt := 0; attempt < len(crashloopBackoffs); attempt++ {
			waitErr := cmd.Wait()
			_ = removePidFile(name, cfg)

			exitMsg := "unknown"
			if waitErr != nil {
				exitMsg = waitErr.Error()
			}
			slog.Warn("component exited", "name", name, "exit", exitMsg)

			if stopped, _ := shutdownRequested.Load(name); stopped == true {
				return
			}

			delay := crashloopBackoffs[attempt]
			if delay > 0 {
				slog.Warn("component crashed, restarting", "name", name, "delay", delay, "attempt", attempt+1, "max", len(crashloopBackoffs))
				time.Sleep(delay)
			} else {
				slog.Warn("component crashed, restarting immediately", "name", name, "attempt", attempt+1, "max", len(crashloopBackoffs))
			}

			if stopped, _ := shutdownRequested.Load(name); stopped == true {
				return
			}

			cmd, err = starter(ctx, cfg)
			if err != nil {
				slog.Error("restart failed", "name", name, "error", err)
				continue
			}
			pidPath := pidFilePath(name, cfg)
			_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)
			slog.Info("component restarted", "name", name, "pid", cmd.Process.Pid)
		}
		slog.Error("crashloop limit reached, giving up", "name", name)
	}()

	return nil
}

// startHookd launches the hookd binary directly (supervisor-hookd mode).
func startHookd(ctx context.Context, cfg config.Config) (*exec.Cmd, error) {
	basedir := prometheusBasedir(cfg)
	binPath := filepath.Join(basedir, "bin", "hookd")
	logPath := filepath.Join(basedir, "var", "logs", "hookd.log")

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir logs: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Dir = basedir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSetsid(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}

	pidPath := filepath.Join(basedir, "var", "pids", "hookd.pid")
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)

	// Close log file when process exits (crashloop goroutine in startComponent
	// owns the restart logic; this just ensures the fd is released).
	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()

	return cmd, nil
}

// startTokenScraper launches the token-scraper binary (supervisor mode).
func startTokenScraper(ctx context.Context, cfg config.Config) (*exec.Cmd, error) {
	basedir := prometheusBasedir(cfg)
	binPath := filepath.Join(basedir, "bin", "token-scraper")
	logPath := filepath.Join(basedir, "var", "logs", "token-scraper.log")

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir logs: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Dir = basedir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSetsid(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}

	pidPath := filepath.Join(basedir, "var", "pids", "token-scraper.pid")
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)

	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()

	return cmd, nil
}

// startHealthCollector launches the health-collector binary (supervisor mode).
func startHealthCollector(ctx context.Context, cfg config.Config) (*exec.Cmd, error) {
	basedir := prometheusBasedir(cfg)
	binPath := filepath.Join(basedir, "bin", "health-collector")
	logPath := filepath.Join(basedir, "var", "logs", "health-collector.log")

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir logs: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Dir = basedir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSetsid(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}

	pidPath := filepath.Join(basedir, "var", "pids", "health-collector.pid")
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)

	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()

	return cmd, nil
}

// processAlive checks both PID existence (via kill -0) and primary port bind.
// Both must pass to declare the process alive. Stale PID files (process gone
// but port unbound) return false.
func processAlive(name string, cfg config.Config) bool {
	pid, err := readPidFile(name, cfg)
	if err != nil {
		return false
	}
	// kill -0 confirms the process exists without sending a real signal.
	if err := syscall.Kill(pid, 0); err != nil {
		_ = removePidFile(name, cfg)
		return false
	}
	port := portFor(name, cfg)
	if port == 0 {
		return true // no port check for unknown components
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// portFor returns the primary listen port for a named component.
func portFor(name string, cfg config.Config) int {
	switch name {
	case "hookd":
		return cfg.HookServerPort
	case "otelcol":
		return cfg.OtelGRPCPort
	case "prometheus":
		return cfg.PrometheusPort
	case "grafana":
		return cfg.GrafanaPort
	case "token-scraper":
		return 0 // token-scraper is a poller; no listen port
	case "health-collector":
		return 0 // health-collector is a poller; no listen port
	default:
		return 0
	}
}

// systemdHookdStatus queries systemd for teamster-hookd.service status.
func systemdHookdStatus() string {
	cmd := exec.Command("systemctl", "is-active", "--quiet", "teamster-hookd")
	if err := cmd.Run(); err == nil {
		// Also verify port bind.
		return "running (systemd)"
	}
	return "not running (systemd)"
}

func pidFilePath(name string, cfg config.Config) string {
	return filepath.Join(prometheusBasedir(cfg), "var", "pids", name+".pid")
}

func readPidFile(name string, cfg config.Config) (int, error) {
	data, err := os.ReadFile(pidFilePath(name, cfg))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func removePidFile(name string, cfg config.Config) error {
	return os.Remove(pidFilePath(name, cfg))
}

func stopByPidFile(name string, cfg config.Config) error {
	shutdownRequested.Store(name, true)
	pid, err := readPidFile(name, cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return killByPort(name, cfg)
		}
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGINT); err != nil {
		_ = removePidFile(name, cfg)
		return nil
	}
	// Wait up to 3s for graceful exit.
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := syscall.Kill(pid, 0); err != nil {
			_ = removePidFile(name, cfg)
			fmt.Printf("%s: stopped\n", name)
			_ = waitForPortFree(name, cfg, 5*time.Second)
			return nil
		}
	}
	// Escalate to SIGKILL.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(200 * time.Millisecond)
	_ = removePidFile(name, cfg)
	fmt.Printf("%s: killed\n", name)
	_ = waitForPortFree(name, cfg, 5*time.Second)
	return nil
}

// killByPort kills any process bound to name's port when no PID file exists.
var ssPidRe = regexp.MustCompile(`pid=(\d+)`)

func killByPort(name string, cfg config.Config) error {
	shutdownRequested.Store(name, true)
	port := portFor(name, cfg)
	if port == 0 {
		return nil
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return nil // port not bound, nothing to kill
	}
	conn.Close()

	out, err := exec.Command("ss", "-tlnp", fmt.Sprintf("sport = :%d", port)).Output()
	if err != nil {
		return nil
	}
	matches := ssPidRe.FindSubmatch(out)
	if matches == nil {
		return nil
	}
	pid, err := strconv.Atoi(string(matches[1]))
	if err != nil {
		return nil
	}

	fmt.Printf("%s: killing orphan on port %d (pid %d)\n", name, port, pid)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	return nil
}

// waitForPort polls until name's port is accepting connections or timeout elapses.
func waitForPort(name string, cfg config.Config, timeout time.Duration) error {
	port := portFor(name, cfg)
	if port == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("%s: port %d not listening after %s", name, port, timeout)
}

// waitForPortFree polls until name's port is no longer accepting connections or timeout elapses.
func waitForPortFree(name string, cfg config.Config, timeout time.Duration) error {
	port := portFor(name, cfg)
	if port == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err != nil {
			return nil
		}
		conn.Close()
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("%s: port %d still bound after %s", name, port, timeout)
}

// readSettingsEnv reads a single env var from ~/.claude/settings.json's "env" block.
// Returns "" if the file doesn't exist, can't be parsed, or the key is absent.
func readSettingsEnv(key string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return ""
	}
	var s map[string]interface{}
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	env, _ := s["env"].(map[string]interface{})
	v, _ := env[key].(string)
	return v
}

// modeFor returns the configured mode for the named service.
func modeFor(name string, cfg config.Config) string {
	switch name {
	case "otelcol":
		return cfg.OtelcolMode
	case "prometheus":
		return cfg.PrometheusMode
	case "grafana":
		return cfg.GrafanaMode
	default:
		return ""
	}
}

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
		cmd := exec.Command("sudo", "systemctl", "start", "teamster-hookd")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("hookd: systemctl start failed: %w", err)
		}
		fmt.Println("hookd: started (systemd)")
	} else if cfg.HookdMode == "supervisor" {
		if err := startComponent(ctx, cfg, "hookd"); err != nil {
			return fmt.Errorf("hookd: %w", err)
		}
	}

	for _, name := range []string{"otelcol", "prometheus", "grafana"} {
		mode := modeFor(name, cfg)
		if mode != "install" {
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

	// Start token-scraper when mode=install (managed = BYO, skip).
	if cfg.CcusageMode == "install" {
		if processAlive("token-scraper", cfg) {
			fmt.Println("token-scraper: already running")
		} else if err := startComponent(ctx, cfg, "token-scraper"); err != nil {
			return fmt.Errorf("token-scraper: %w", err)
		}
	}

	// Final verification — confirm all expected services are actually alive.
	for _, name := range []string{"hookd", "otelcol", "prometheus", "grafana", "token-scraper"} {
		if name == "hookd" && cfg.HookdMode == "systemd" {
			continue
		}
		if name == "hookd" && cfg.HookdMode == "external" {
			continue
		}
		if name == "token-scraper" {
			if cfg.CcusageMode != "install" {
				continue
			}
		} else if name != "hookd" {
			if modeFor(name, cfg) != "install" {
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

// supervisorStop stops all supervised components. It kills the supervisor
// process first (which signals its handler to stop children), then directly
// stops all components by PID file — handling orphans from a dead supervisor.
func supervisorStop(cfg config.Config) error {
	// 1. Kill the supervisor process itself (signals its handler to stop children).
	supervisorPidPath := filepath.Join(prometheusBasedir(cfg), "var", "pids", "teamster.pid")
	if data, err := os.ReadFile(supervisorPidPath); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			time.Sleep(2 * time.Second)
		}
	}

	// 2. Directly stop ALL components — handles orphans from dead supervisors.
	allComponents := []string{"token-scraper", "grafana", "prometheus", "otelcol", "hookd"}
	var errs []string
	for _, name := range allComponents {
		shutdownRequested.Store(name, true)
		if err := stopByPidFile(name, cfg); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	// Also try systemd for hookd — may have been started under a different mode.
	_ = exec.Command("sudo", "systemctl", "stop", "teamster-hookd").Run()

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

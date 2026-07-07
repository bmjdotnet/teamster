package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/statusui"
	"github.com/bmjdotnet/teamster/internal/store"
)

// ANSI color codes — used only by buildStatusRows / checkStatus return values.
// The Bubbletea view strips these via stripANSI before applying lipgloss styles.
const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[0;32m"
	ansiYellow = "\033[0;33m"
	ansiRed    = "\033[0;31m"
	ansiDim    = "\033[2m"
)

func colorize(s, code string) string {
	if code == "" {
		return s
	}
	return code + s + ansiReset
}

// statusRow is one rendered row in the status table.
type statusRow struct {
	label    string
	status   string // may contain ANSI escape codes from checkStatus
	mode     string
	endpoint string
}

// stripANSI removes ANSI escape codes from s.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		if inEsc {
			if s[i] == 'm' {
				inEsc = false
			}
			continue
		}
		if s[i] == '\033' && i+1 < len(s) && s[i+1] == '[' {
			inEsc = true
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// statusLive is set by --live flag in parseSupervisorFlags.
var statusLive bool

// supervisorStatus prints a status snapshot by default.
// With --live, launches the Bubbletea interactive dashboard.
func supervisorStatus(cfg config.Config) {
	fetcher := func() []statusui.ServiceRow {
		return buildServiceRows(cfg)
	}

	if statusLive {
		if err := statusui.Run(cfg, fetcher); err != nil {
			fmt.Fprintf(os.Stderr, "status: %v\n", err)
		}
		return
	}

	fmt.Print(statusui.RenderOnce(cfg, fetcher))
}

// buildServiceRows converts buildStatusRows output to statusui.ServiceRow,
// stripping ANSI codes from the status field so lipgloss can style it.
func buildServiceRows(cfg config.Config) []statusui.ServiceRow {
	raw := buildStatusRows(cfg)
	out := make([]statusui.ServiceRow, len(raw))
	for i, r := range raw {
		out[i] = statusui.ServiceRow{
			Label:    r.label,
			Status:   stripANSI(r.status),
			Mode:     r.mode,
			Endpoint: r.endpoint,
		}
	}
	return out
}

// buildStatusRows computes one row per service from cfg.
func buildStatusRows(cfg config.Config) []statusRow {
	const timeout = 2 * time.Second
	var rows []statusRow

	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}

	endpointURL := func(port int) string {
		return fmt.Sprintf("http://%s:%d", host, port)
	}

	// ── hookd ────────────────────────────────────────────────────────────────
	hookdMode := cfg.HookdMode
	hookdEndpoint := endpointURL(cfg.HookServerPort)
	hookdHealthURL := cfg.HookdHealth
	if hookdHealthURL == "" {
		hookdHealthURL = fmt.Sprintf("http://localhost:%d/health", cfg.HookServerPort)
	}
	hookdStatus := checkStatus(checkParams{
		mode:       hookdMode,
		pidName:    pidIfSupervisor(hookdMode, "hookd"),
		healthURL:  hookdHealthURL,
		systemdSvc: "teamster-hookd",
		cfg:        cfg,
		timeout:    timeout,
	})
	rows = append(rows, statusRow{
		label:    "Event Server (hookd)",
		status:   hookdStatus,
		mode:     hookdMode,
		endpoint: hookdEndpoint,
	})

	// ── store ─────────────────────────────────────────────────────────────────
	storeMode := cfg.StoreMode
	if storeMode == "" {
		storeMode = "managed"
	}
	storeEndpoint := "—"
	storeStatus := colorize("Not configured", ansiDim)
	if cfg.StoreDSN.Raw != "" {
		// host:port only — never the full DSN, which carries the password.
		// Consistent with the grafana_ro row below.
		storeEndpoint = storeHostForDisplay(cfg.StoreDSN)
		storeStatus = checkStoreStatus(storeMode, cfg.StoreDSN.Raw, timeout)
	}
	rows = append(rows, statusRow{
		label:    "WMS Store (store)",
		status:   storeStatus,
		mode:     storeMode,
		endpoint: storeEndpoint,
	})

	// ── otelcol ───────────────────────────────────────────────────────────────
	otelMode := cfg.OtelcolMode
	otelEndpoint := "—"
	otelStatus := colorize("Not configured", ansiDim)
	if otelMode != "none" && otelMode != "" {
		otelEndpoint = endpointURL(cfg.OtelGRPCPort)
		otelStatus = checkStatus(checkParams{
			mode:    otelMode,
			pidName: pidIfInstall(otelMode, "otelcol"),
			port:    cfg.OtelGRPCPort,
			cfg:     cfg,
			timeout: timeout,
		})
	}
	rows = append(rows, statusRow{
		label:    "Telemetry Collector (otelcol)",
		status:   otelStatus,
		mode:     displayMode(otelMode),
		endpoint: otelEndpoint,
	})

	// ── prometheus ────────────────────────────────────────────────────────────
	promMode := cfg.PrometheusMode
	promEndpoint := "—"
	promStatus := colorize("Not configured", ansiDim)
	if promMode != "none" && promMode != "" {
		promHealthURL := cfg.PrometheusHealth
		if promHealthURL == "" {
			promHealthURL = fmt.Sprintf("http://localhost:%d/-/healthy", cfg.PrometheusPort)
		}
		promEndpoint = endpointURL(cfg.PrometheusPort)
		promStatus = checkStatus(checkParams{
			mode:      promMode,
			pidName:   pidIfInstall(promMode, "prometheus"),
			port:      cfg.PrometheusPort,
			healthURL: promHealthURL,
			cfg:       cfg,
			timeout:   timeout,
		})
	}
	rows = append(rows, statusRow{
		label:    "Monitoring Server (prometheus)",
		status:   promStatus,
		mode:     displayMode(promMode),
		endpoint: promEndpoint,
	})

	// ── grafana ───────────────────────────────────────────────────────────────
	grafanaMode := cfg.GrafanaMode
	grafanaEndpoint := "—"
	grafanaStatus := colorize("Not configured", ansiDim)
	if grafanaMode != "none" && grafanaMode != "" {
		grafanaHealthURL := cfg.GrafanaHealth
		if grafanaHealthURL == "" {
			grafanaHealthURL = fmt.Sprintf("http://localhost:%d/api/health", cfg.GrafanaPort)
		}
		grafanaEndpoint = endpointURL(cfg.GrafanaPort)
		grafanaStatus = checkStatus(checkParams{
			mode:      grafanaMode,
			pidName:   pidIfInstall(grafanaMode, "grafana"),
			port:      cfg.GrafanaPort,
			healthURL: grafanaHealthURL,
			cfg:       cfg,
			timeout:   timeout,
		})
	}
	rows = append(rows, statusRow{
		label:    "Dashboard Server (grafana)",
		status:   grafanaStatus,
		mode:     displayMode(grafanaMode),
		endpoint: grafanaEndpoint,
	})

	// ── grafana read-only DB user ──────────────────────────────────────────────
	// Surfaces whether the grafana_ro MySQL user the D1-D4 SQL panels depend on
	// is actually usable. This is AUTHORITATIVE, not a file-existence check: we
	// connect as grafana_ro with the persisted password and run SELECT 1, so the
	// row reflects whether the panels can really authorize — a stale password
	// file or an out-of-band-dropped user reads honestly as Unauthorized, never a
	// false "Provisioned". (A MySQL connect on an interactive status call is
	// acceptable.) Only relevant when this host installs Grafana over a MySQL
	// store.
	if grafanaMode == "install" && cfg.StoreDSN.Scheme == "mysql" {
		var roStatus, roEndpoint string
		pw, _ := readGrafanaReadonlyPassword(filepath.Join(grafanaBasedir(cfg), "var", "grafana"))
		ok := cfg.StoreDSN.Host != "" && cfg.StoreDSN.Database != ""
		switch {
		case pw == "":
			roStatus = colorize("Not provisioned", ansiYellow)
			roEndpoint = "run: apply etc/grafana/grafana-readonly-user.sql as a DB admin"
		case !ok:
			roStatus = colorize("Unknown", ansiDim)
			roEndpoint = "—"
		case credentialProberAuthorizes(cfg.StoreDSN.Raw, pw, timeout):
			roStatus = colorize("Provisioned", ansiGreen)
			roEndpoint = grafanaReadonlyUser + "@" + storeHostForDisplay(cfg.StoreDSN)
		default:
			roStatus = colorize("Unauthorized", ansiRed)
			roEndpoint = "grafana_ro cannot authorize — re-apply grafana-readonly-user.sql as a DB admin"
		}
		rows = append(rows, statusRow{
			label:    "Grafana DB User (grafana_ro)",
			status:   roStatus,
			mode:     displayMode(grafanaMode),
			endpoint: roEndpoint,
		})
	}

	// ── token-scraper ─────────────────────────────────────────────────────────
	scraperMode := cfg.CcusageMode
	scraperEndpoint := "—"
	scraperStatus := colorize("Not configured", ansiDim)
	if scraperMode == "install" {
		scraperEndpoint = "http://localhost:9124"
		scraperStatus = checkStatus(checkParams{
			mode:    scraperMode,
			pidName: "token-scraper",
			// no port: token-scraper is a poller, not a server
			cfg:     cfg,
			timeout: timeout,
		})
	}
	rows = append(rows, statusRow{
		label:    "Token Scraper (token-scraper)",
		status:   scraperStatus,
		mode:     displayMode(scraperMode),
		endpoint: scraperEndpoint,
	})

	return rows
}

// checkParams bundles inputs to checkStatus.
type checkParams struct {
	mode       string
	pidName    string // PID file name (empty = no PID check)
	port       int    // TCP port to probe (0 = skip)
	healthURL  string // HTTP health URL (empty = skip)
	systemdSvc string // systemd service name for systemd mode (empty = skip)
	cfg        config.Config
	timeout    time.Duration
}

// checkStatus returns a colored status string for a single service.
func checkStatus(p checkParams) string {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	running := false
	pid := 0

	switch p.mode {
	case "install":
		if p.pidName != "" {
			pid, _ = readPidFile(p.pidName, p.cfg)
			running = pid > 0 && processAlive(p.pidName, p.cfg)
		} else if p.port != 0 {
			running = portBound(p.port)
		}
	case "supervisor":
		if p.pidName != "" {
			pid, _ = readPidFile(p.pidName, p.cfg)
			running = pid > 0 && processAlive(p.pidName, p.cfg)
		} else if p.port != 0 {
			running = portBound(p.port)
		}
	case "systemd":
		// For hookd under systemd, delegate to the shared helper.
		if p.systemdSvc == "teamster-hookd" {
			return systemdHookdStatus()
		}
		if p.systemdSvc != "" {
			cmd := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", p.systemdSvc)
			running = cmd.Run() == nil
		} else if p.port != 0 {
			running = portBound(p.port)
		}
	case "managed", "external":
		if p.healthURL != "" {
			running = httpHealthy(ctx, p.healthURL)
		} else if p.port != 0 {
			running = portBound(p.port)
		}
	case "none", "":
		return colorize("Not configured", ansiDim)
	}

	if !running {
		if p.mode == "external" || p.mode == "managed" {
			return colorize("Unreachable", ansiRed)
		}
		return colorize("Not running", ansiRed)
	}

	// Running — attempt health check.
	healthy := true
	if p.healthURL != "" {
		healthy = httpHealthy(ctx, p.healthURL)
	}

	if !healthy {
		return colorize("Not responding", ansiYellow)
	}

	if p.mode == "external" && p.healthURL != "" && p.pidName == "" {
		return colorize("Connected", ansiGreen)
	}

	if pid > 0 {
		return colorize(fmt.Sprintf("Healthy (pid %d)", pid), ansiGreen)
	}
	return colorize("Healthy", ansiGreen)
}

// checkStoreStatus checks store health.
// Store is never supervisor-managed — MySQL is either apt-installed (install)
// or externally provided (managed/external). Primary check is storeReachable
// (F10: a real backend connection, not a raw port probe). For install mode, a
// failed connection is cross-checked against systemctl to distinguish
// "service up, not ready yet" from "service down."
func checkStoreStatus(mode, dsn string, timeout time.Duration) string {
	reachable := storeReachable(dsn, timeout)
	if reachable {
		return colorize("Healthy", ansiGreen)
	}
	// Unreachable. For install mode check whether systemd unit is active.
	if mode == "install" {
		for _, unit := range []string{"mariadb", "mysql", "mysqld"} {
			cmd := exec.Command("systemctl", "is-active", "--quiet", unit)
			if cmd.Run() == nil {
				// Systemd says the service is active but the store isn't ready yet.
				return colorize("Not responding", ansiYellow)
			}
		}
		return colorize("Not running", ansiRed)
	}
	return colorize("Unreachable", ansiRed)
}

// storeReachable opens dsn read-only via store.Open (skipping migrations,
// since a status check must never write schema) and pings the resulting
// handle — a construction failure or a failed ping both mean "unreachable".
// This replaces the old raw TCP-dial-against-host-port approach, which
// hard-coded MySQL's default port 3306 and couldn't distinguish a real
// outage from an unsupported/misconfigured backend.
func storeReachable(dsn string, timeout time.Duration) bool {
	if dsn == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	st, err := store.Open(ctx, dsn, store.WithSkipMigrate())
	if err != nil {
		return false
	}
	defer st.Close() //nolint:errcheck
	return st.Ping(ctx) == nil
}

// pidIfInstall returns pidName only when mode == "install" (supervisor-managed).
func pidIfInstall(mode, name string) string {
	if mode == "install" {
		return name
	}
	return ""
}

// pidIfSupervisor returns pidName when hookd is running under supervisor.
func pidIfSupervisor(mode, name string) string {
	if mode == "supervisor" {
		return name
	}
	return ""
}

// displayMode normalizes empty mode to "none" for display.
func displayMode(mode string) string {
	if mode == "" {
		return "none"
	}
	return mode
}

// httpHealthy performs a GET on healthURL and returns true if status is 200.
func httpHealthy(ctx context.Context, healthURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// credentialProberAuthorizes opens dsn read-only via store.Open (skipping
// migrations, same as storeReachable) and type-asserts store.CredentialProber,
// returning true iff the grafana_ro user actually authorizes against it. This
// is the authoritative provisioning check for the status row: it proves the
// D1-D4 datasource account can really connect, so a stale password file or an
// out-of-band-dropped user can't read as "Provisioned". A backend without a
// CredentialProber (or an unreachable store) degrades to false, rendered as
// "Unauthorized" by the caller — the safe default for a security-relevant row.
func credentialProberAuthorizes(dsn, password string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	st, err := store.Open(ctx, dsn, store.WithSkipMigrate())
	if err != nil {
		return false
	}
	defer st.Close() //nolint:errcheck
	cp, ok := st.(store.CredentialProber)
	if !ok {
		return false
	}
	return cp.PingAs(ctx, grafanaReadonlyUser, password) == nil
}

// storeHostForDisplay returns the host[:port] of the store DSN for the status
// table, dropping the credentials. Falls back to "store" when the DSN has no
// host (empty or unconfigured).
func storeHostForDisplay(dsn config.StoreDSN) string {
	if dsn.Host == "" {
		return "store"
	}
	if dsn.Port == 0 {
		return dsn.Host
	}
	return fmt.Sprintf("%s:%d", dsn.Host, dsn.Port)
}

// portBound returns true if a listener is already bound on the given TCP port.
func portBound(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

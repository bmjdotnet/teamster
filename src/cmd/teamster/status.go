package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/version"
)

// ANSI color codes for status display.
const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[0;32m"
	ansiYellow = "\033[0;33m"
	ansiRed    = "\033[0;31m"
	ansiDim    = "\033[2m"
)

func colorize(s, code string) string {
	if !isTerminal() {
		return s
	}
	return code + s + ansiReset
}

var terminalCheck = func() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func isTerminal() bool { return terminalCheck() }

// statusRow is one rendered row in the status table.
type statusRow struct {
	label    string // Col 1: "Event Server (hookd)"
	status   string // Col 2: colored status text
	mode     string // Col 3: install | managed | external | none
	endpoint string // Col 4: URL or DSN or "—"
}

// visLen returns the visible (printable) length of s, stripping ANSI escape sequences.
func visLen(s string) int {
	n := 0
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
			i++ // skip '['
			continue
		}
		n++
	}
	return n
}

// padVisible right-pads s with spaces to reach at least width visible characters.
func padVisible(s string, width int) string {
	pad := width - visLen(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

// supervisorStatus reports current liveness of each configured service as a
// 4-column ANSI-colored table. Column 2 (status) may contain ANSI codes, so
// we compute visible-length padding manually rather than relying on tabwriter,
// which counts bytes not printable characters.
func supervisorStatus(cfg config.Config) {
	rows := buildStatusRows(cfg)

	const (
		col1W = 36
		col2W = 24
		col3W = 12
		gap   = 2
	)
	sep := strings.Repeat(" ", gap)

	fmt.Println()
	fmt.Printf("  %s %s\n", padVisible("Teamster", col1W), colorize("teamster "+version.String(), ansiDim))
	for _, r := range rows {
		fmt.Printf("  %s%s%s%s%s%s%s\n",
			padVisible(r.label, col1W), sep,
			padVisible(r.status, col2W), sep,
			padVisible(r.mode, col3W), sep,
			r.endpoint,
		)
	}
	fmt.Println()
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
	if cfg.StoreDSN.Primary != "" {
		// host:port only — never the full DSN, which carries the password.
		// Consistent with the grafana_ro row below.
		storeEndpoint = storeHostForDisplay(cfg.StoreDSN.Primary)
		storeStatus = checkStoreStatus(storeMode, cfg.StoreDSN.Primary, timeout)
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
	if grafanaMode == "install" && strings.HasPrefix(cfg.StoreDSN.Primary, "mysql://") {
		var roStatus, roEndpoint string
		pw, _ := readGrafanaReadonlyPassword(filepath.Join(grafanaBasedir(cfg), "var", "grafana"))
		host, port, db, ok := decomposeStoreDSN(cfg.StoreDSN.Primary)
		switch {
		case pw == "":
			roStatus = colorize("Not provisioned", ansiYellow)
			roEndpoint = "run: apply etc/grafana/grafana-readonly-user.sql as a DB admin"
		case !ok:
			roStatus = colorize("Unknown", ansiDim)
			roEndpoint = "—"
		case grafanaReadonlyAuthorizes(host, port, db, pw, timeout):
			roStatus = colorize("Provisioned", ansiGreen)
			roEndpoint = grafanaReadonlyUser + "@" + storeHostForDisplay(cfg.StoreDSN.Primary)
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
	healthDSN  string // MySQL DSN for TCP probe (empty = skip)
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
		if p.healthDSN != "" {
			running = mysqlReachable(p.healthDSN, p.timeout)
		} else if p.healthURL != "" {
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
	if p.healthDSN != "" {
		healthy = mysqlReachable(p.healthDSN, p.timeout)
	} else if p.healthURL != "" {
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

// checkStoreStatus checks MySQL/store health.
// Store is never supervisor-managed — MySQL is either apt-installed (install)
// or externally provided (managed/external). Primary check is always TCP dial
// via mysqlReachable. For install mode, a failing TCP dial is cross-checked
// against systemctl to distinguish "service up, port not yet ready" from
// "service down."
func checkStoreStatus(mode, dsn string, timeout time.Duration) string {
	reachable := mysqlReachable(dsn, timeout)
	if reachable {
		return colorize("Healthy", ansiGreen)
	}
	// TCP failed. For install mode check whether systemd unit is active.
	if mode == "install" {
		for _, unit := range []string{"mariadb", "mysql", "mysqld"} {
			cmd := exec.Command("systemctl", "is-active", "--quiet", unit)
			if cmd.Run() == nil {
				// Systemd says the service is active but TCP not ready yet.
				return colorize("Not responding", ansiYellow)
			}
		}
		return colorize("Not running", ansiRed)
	}
	return colorize("Unreachable", ansiRed)
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

// grafanaReadonlyAuthorizes connects to MySQL as the grafana_ro user with the
// persisted password and runs SELECT 1, returning true iff the account actually
// authorizes. This is the authoritative provisioning check for the status row:
// it proves the D1-D4 datasource account can really query, so a stale password
// file or an out-of-band-dropped user can't read as "Provisioned". Builds the
// driver DSN via mysqldriver.Config (same shape as the store's convertDSN);
// importing the driver here also guarantees it is registered for sql.Open.
func grafanaReadonlyAuthorizes(host, port, db, password string, timeout time.Duration) bool {
	dc := mysqldriver.NewConfig()
	dc.Net = "tcp"
	dc.Addr = net.JoinHostPort(host, port)
	dc.User = grafanaReadonlyUser
	dc.Passwd = password
	dc.DBName = db
	dc.Timeout = timeout

	pool, err := sql.Open("mysql", dc.FormatDSN())
	if err != nil {
		return false
	}
	defer pool.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var one int
	if err := pool.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return false
	}
	return one == 1
}

// storeHostForDisplay returns the host[:port] of a mysql:// DSN for the status
// table, dropping the credentials. Falls back to the raw host on parse failure.
func storeHostForDisplay(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return "store"
	}
	return u.Host
}

// mysqlReachable TCP-dials the host:port from a mysql:// DSN.
func mysqlReachable(dsn string, timeout time.Duration) bool {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":3306"
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
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

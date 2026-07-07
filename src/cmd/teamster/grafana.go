package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/bmjdotnet/teamster/internal/config"
)

// grafanaReadonlyUser is the least-privilege MySQL account the "Teamster MySQL"
// datasource connects as. Fixed name for the single-host case; the password is
// generated and persisted (never committed). See grafana-readonly-user.sql.
const grafanaReadonlyUser = "grafana_ro"

// grafanaTemplateData is the shape fed to grafana.ini.tmpl and the
// provisioning template files.
type grafanaTemplateData struct {
	GrafanaPort          int
	GrafanaDir           string
	GrafanaStateDir      string
	GrafanaSecretKey     string
	GrafanaAdminPassword string
	Hostname             string
	PrometheusPort       int
	HookdPort            int

	// Teamster MySQL datasource connection (decomposed from cfg.StoreDSN).
	// Grafana connects as the read-only GrafanaDBUser, not the StoreDSN account.
	StoreHost         string
	StorePort         string
	StoreDB           string
	GrafanaDBUser     string
	GrafanaDBPassword string
}

// StartGrafana renders the grafana.ini template and provisioning configs,
// then launches grafana-server. Returns the running *exec.Cmd so the
// supervisor can reap it.
func StartGrafana(ctx context.Context, cfg config.Config) (*exec.Cmd, error) {
	basedir := grafanaBasedir(cfg)
	grafanaEtcDir := filepath.Join(basedir, "etc", "grafana")
	grafanaStateDir := filepath.Join(basedir, "var", "grafana")
	iniPath := filepath.Join(grafanaEtcDir, "grafana.ini")
	logPath := filepath.Join(basedir, "var", "logs", "grafana.log")
	binPath := filepath.Join(basedir, "bin", "grafana-server")

	for _, dir := range []string{
		grafanaEtcDir,
		filepath.Join(grafanaEtcDir, "provisioning", "datasources"),
		filepath.Join(grafanaEtcDir, "provisioning", "dashboards"),
		filepath.Join(grafanaEtcDir, "dashboards"),
		grafanaStateDir,
		filepath.Join(grafanaStateDir, "logs"),
		filepath.Join(grafanaStateDir, "plugins"),
		filepath.Join(basedir, "var", "logs"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("grafana: mkdir %s: %w", dir, err)
		}
	}

	secretKey, err := grafanaSecretKey(grafanaStateDir)
	if err != nil {
		return nil, fmt.Errorf("grafana: secret key: %w", err)
	}

	adminPassword := "admin"

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}

	data := grafanaTemplateData{
		GrafanaPort:          cfg.GrafanaPort,
		GrafanaDir:           grafanaEtcDir,
		GrafanaStateDir:      grafanaStateDir,
		GrafanaSecretKey:     secretKey,
		GrafanaAdminPassword: adminPassword,
		Hostname:             hostname,
		PrometheusPort:       cfg.PrometheusPort,
		HookdPort:            cfg.HookServerPort,
	}

	// Populate the Teamster MySQL datasource fields from the WMS StoreDSN.
	// Only meaningful when the store is the MySQL spine; an empty/unparseable
	// DSN leaves the fields blank (the datasource provisions but can't connect)
	// rather than failing grafana startup.
	//
	// The privileged grafana_ro CREATE USER + GRANT is NOT done here. It needs a
	// DB admin (under store-mode=managed the StoreDSN is the least-priv app
	// account, which lacks CREATE USER), so install.sh provisions the user via
	// socket-root `sudo mysql` and owns the password file. The supervisor only
	// READS that password to render the datasource — never generates it, never
	// runs SQL. If the file is absent (install.sh skipped it, or grafana isn't
	// install-mode), the datasource still provisions but with no password and
	// its panels are unauthorized; that is surfaced by install.sh and
	// `teamster status`, not by aborting grafana start.
	if cfg.StoreDSN.Scheme == "mysql" && cfg.StoreDSN.Host != "" && cfg.StoreDSN.Database != "" {
		roPassword, perr := readGrafanaReadonlyPassword(grafanaStateDir)
		if perr != nil {
			return nil, fmt.Errorf("grafana: read-only db password: %w", perr)
		}
		data.StoreHost = cfg.StoreDSN.Host
		data.StorePort = storePortString(cfg.StoreDSN.Port)
		data.StoreDB = cfg.StoreDSN.Database
		data.GrafanaDBUser = grafanaReadonlyUser
		data.GrafanaDBPassword = roPassword
		if roPassword == "" {
			slog.Warn("grafana: no grafana_ro password file; MySQL datasource will provision unauthenticated until install.sh provisions the read-only user",
				"path", filepath.Join(grafanaStateDir, grafanaReadonlyPasswordFile))
		}
	}

	if err := renderGrafanaConfigs(basedir, grafanaEtcDir, data); err != nil {
		return nil, fmt.Errorf("grafana: render configs: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("grafana: open log: %w", err)
	}

	grafanaHomePath := filepath.Join(basedir, "var", "grafana-home")
	cmd := exec.CommandContext(ctx, binPath,
		"--config="+iniPath,
		"--homepath="+grafanaHomePath,
	)
	cmd.Dir = basedir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setSetsid(cmd)

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("grafana: start: %w", err)
	}

	pidPath := filepath.Join(basedir, "var", "pids", "grafana.pid")
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644)

	// Close log file when process exits; crashloop in startComponent owns restart.
	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()

	return cmd, nil
}

// StopGrafana sends SIGTERM to a running grafana process.
func StopGrafana(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(os.Interrupt)
}

// GrafanaPort returns the configured grafana listen port.
func GrafanaPort(cfg config.Config) int {
	return cfg.GrafanaPort
}

// renderGrafanaConfigs renders grafana.ini and the provisioning templates
// from the skel templates in <basedir>/etc/grafana/.
func renderGrafanaConfigs(basedir, grafanaEtcDir string, data grafanaTemplateData) error {
	skelGrafanaDir := filepath.Join(basedir, "etc", "grafana")

	// Render grafana.ini from grafana.ini.tmpl.
	if err := renderTemplate(
		filepath.Join(skelGrafanaDir, "grafana.ini.tmpl"),
		filepath.Join(grafanaEtcDir, "grafana.ini"),
		data,
	); err != nil {
		return err
	}

	// Render datasource provisioning template.
	if err := renderTemplate(
		filepath.Join(skelGrafanaDir, "provisioning", "datasources", "teamster.yaml.tmpl"),
		filepath.Join(grafanaEtcDir, "provisioning", "datasources", "teamster.yaml"),
		data,
	); err != nil {
		return err
	}

	// Render dashboard provisioning template.
	if err := renderTemplate(
		filepath.Join(skelGrafanaDir, "provisioning", "dashboards", "teamster.yaml.tmpl"),
		filepath.Join(grafanaEtcDir, "provisioning", "dashboards", "teamster.yaml"),
		data,
	); err != nil {
		return err
	}

	// Copy dashboard JSON files (not templates — static JSON).
	dashboardSrcDir := filepath.Join(skelGrafanaDir, "dashboards")
	dashboardDstDir := filepath.Join(grafanaEtcDir, "dashboards")
	entries, err := os.ReadDir(dashboardSrcDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read dashboard src: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		src := filepath.Join(dashboardSrcDir, e.Name())
		dst := filepath.Join(dashboardDstDir, e.Name())
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read dashboard %s: %w", src, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("write dashboard %s: %w", dst, err)
		}
	}

	return nil
}

func renderTemplate(tmplPath, dst string, data any) error {
	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		return fmt.Errorf("read template %s: %w", tmplPath, err)
	}
	tmpl, err := template.New(filepath.Base(tmplPath)).Parse(string(tmplBytes))
	if err != nil {
		return fmt.Errorf("parse template %s: %w", tmplPath, err)
	}
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer f.Close()
	return tmpl.Execute(f, data)
}

// grafanaSecretKey returns a persistent random 32-byte hex secret from
// <grafanaStateDir>/secret_key. Generated once on first start; stable across
// restarts so existing sessions stay valid.
func grafanaSecretKey(stateDir string) (string, error) {
	keyPath := filepath.Join(stateDir, "secret_key")
	data, err := os.ReadFile(keyPath)
	if err == nil {
		return string(data), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := hex.EncodeToString(b)
	if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
		return "", err
	}
	return key, nil
}

// grafanaPersistentSecret returns a persistent random hex secret from
// <stateDir>/<name>. Generated once; stable across restarts.
func grafanaPersistentSecret(stateDir, name string, nBytes int) (string, error) {
	p := filepath.Join(stateDir, name)
	data, err := os.ReadFile(p)
	if err == nil {
		return string(data), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(b)
	if err := os.WriteFile(p, []byte(secret), 0o600); err != nil {
		return "", err
	}
	return secret, nil
}

// storePortString returns dsnPort as a string for datasource/status display,
// defaulting to MySQL's standard 3306 when the DSN omitted a port.
func storePortString(dsnPort int) string {
	if dsnPort == 0 {
		return "3306"
	}
	return strconv.Itoa(dsnPort)
}

// grafanaReadonlyPasswordFile is the basename, under <grafanaStateDir>, of the
// generated grafana_ro password. install.sh owns its creation (it provisions
// the user); the supervisor only reads it. 0o600, never committed.
const grafanaReadonlyPasswordFile = "grafana_ro_password"

// readGrafanaReadonlyPassword returns the persisted grafana_ro password written
// by install.sh, or "" if it does not exist yet (the read-only user has not
// been provisioned). A genuinely-broken file (permissions, etc.) is an error;
// a missing file is not — the datasource just provisions unauthenticated and
// install.sh / `teamster status` flag the gap.
func readGrafanaReadonlyPassword(stateDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, grafanaReadonlyPasswordFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func grafanaBasedir(cfg config.Config) string {
	return filepath.Dir(cfg.DataDir)
}

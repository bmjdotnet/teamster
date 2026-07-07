package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/bmjdotnet/teamster/internal/config"
)

// TestStoreDSNForGrafana exercises the host/port/db decomposition that
// StartGrafana and checkStoreStatus's grafana_ro row derive from a parsed
// config.StoreDSN — the mysql-driver-plus-non-empty-host/database gate and
// port defaulting (storePortString) that the pre-unification per-caller
// decompose helper used to compute in one shot.
func TestStoreDSNForGrafana(t *testing.T) {
	tests := []struct {
		name                       string
		raw                        string
		wantHost, wantPort, wantDB string
		wantOK                     bool
	}{
		{
			name:     "full url",
			raw:      "mysql://wms:secret@127.0.0.1:3306/teamster",
			wantHost: "127.0.0.1", wantPort: "3306", wantDB: "teamster",
			wantOK: true,
		},
		{
			name:     "default port when omitted",
			raw:      "mysql://wms:secret@db.internal/teamster",
			wantHost: "db.internal", wantPort: "3306", wantDB: "teamster",
			wantOK: true,
		},
		{
			name:     "query params ignored",
			raw:      "mysql://wms:secret@host:13306/wmsdb?parseTime=true",
			wantHost: "host", wantPort: "13306", wantDB: "wmsdb",
			wantOK: true,
		},
		{name: "empty dsn", raw: "", wantOK: false},
		{name: "non-mysql scheme", raw: "postgres://u@h:5432/d", wantOK: false},
		{name: "no database path", raw: "mysql://wms:secret@host:3306/", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn, err := config.ParseStoreDSN(tt.raw)
			ok := err == nil && dsn.Scheme == "mysql" && dsn.Host != "" && dsn.Database != ""
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			port := storePortString(dsn.Port)
			if dsn.Host != tt.wantHost || port != tt.wantPort || dsn.Database != tt.wantDB {
				t.Fatalf("got (%q,%q,%q), want (%q,%q,%q)",
					dsn.Host, port, dsn.Database, tt.wantHost, tt.wantPort, tt.wantDB)
			}
		})
	}
}

// TestDatasourceTemplateRendersAllVars renders the shipped datasource template
// with a fully-populated grafanaTemplateData and asserts every var resolves —
// no leftover {{ }} markers and every supplied value present. This is the
// install-side proof that the 5 new MySQL datasource vars wire through, the
// counterpart to @grafana's jq-validity proof.
func TestDatasourceTemplateRendersAllVars(t *testing.T) {
	tmplPath := filepath.Join("..", "..", "..", "skel", "etc", "grafana",
		"provisioning", "datasources", "teamster.yaml.tmpl")
	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	tmpl, err := template.New("ds").Option("missingkey=error").Parse(string(tmplBytes))
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}

	data := grafanaTemplateData{
		GrafanaPort:       3100,
		GrafanaDir:        "/opt/teamster/etc/grafana",
		GrafanaStateDir:   "/opt/teamster/var/grafana",
		GrafanaSecretKey:     "deadbeef",
		GrafanaAdminPassword: "deadbeefdeadbeef",
		PrometheusPort:    9190,
		HookdPort:         9125,
		StoreHost:         "127.0.0.1",
		StorePort:         "3306",
		StoreDB:           "teamster",
		GrafanaDBUser:     grafanaReadonlyUser,
		GrafanaDBPassword: "s3cr3tpw",
	}

	var sb strings.Builder
	if err := tmpl.Execute(&sb, data); err != nil {
		t.Fatalf("execute template: %v", err)
	}
	out := sb.String()

	if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
		t.Fatalf("unresolved template markers remain:\n%s", out)
	}
	for _, want := range []string{
		"127.0.0.1:3306", // StoreHost:StorePort
		"database: teamster",
		"user: " + grafanaReadonlyUser,
		"password: s3cr3tpw",
		"uid: teamster-mysql",
		"http://127.0.0.1:9190",  // prometheus url still renders
		"http://127.0.0.1:9125",  // hookd url for infinity datasource
		"uid: teamster-events",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered datasource missing %q:\n%s", want, out)
		}
	}
}

// TestReadGrafanaReadonlyPassword proves the supervisor only READS the password
// install.sh persists: a missing file yields "" with no error (datasource
// provisions unauthenticated), and a present file yields the trimmed value. The
// supervisor never generates the password — that moved to install.sh (D1).
func TestReadGrafanaReadonlyPassword(t *testing.T) {
	dir := t.TempDir()

	// Missing file → "", no error.
	pw, err := readGrafanaReadonlyPassword(dir)
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if pw != "" {
		t.Fatalf("missing file should yield empty password, got %q", pw)
	}

	// Present file → trimmed value.
	want := "abc123def456"
	if err := os.WriteFile(filepath.Join(dir, grafanaReadonlyPasswordFile), []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}
	pw, err = readGrafanaReadonlyPassword(dir)
	if err != nil {
		t.Fatalf("present file should not error, got %v", err)
	}
	if pw != want {
		t.Fatalf("password = %q, want %q", pw, want)
	}
}

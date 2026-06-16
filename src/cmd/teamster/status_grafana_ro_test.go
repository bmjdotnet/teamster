package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// TestGrafanaReadonlyAuthorizes proves the authoritative status check: it returns
// true only when grafana_ro can actually connect + SELECT, and false for a wrong
// password or an unreachable host. This is what makes `teamster status` honest —
// a stale password file no longer reads as "Provisioned".
//
// Needs the dedicated test MySQL (TEAMSTER_TEST_MYSQL_DSN, e.g.
// mysql://root:test@127.0.0.1:13306/); SKIPs otherwise, like the store tests.
func TestGrafanaReadonlyAuthorizes(t *testing.T) {
	rawDSN := os.Getenv("TEAMSTER_TEST_MYSQL_DSN")
	if rawDSN == "" {
		t.Skip("TEAMSTER_TEST_MYSQL_DSN not set")
	}
	u, err := url.Parse(rawDSN)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	adminUser := u.User.Username()
	adminPass, _ := u.User.Password()

	// Admin connection (root) to set up + tear down the grafana_ro fixture.
	adc := mysqldriver.NewConfig()
	adc.Net = "tcp"
	adc.Addr = host + ":" + port
	adc.User = adminUser
	adc.Passwd = adminPass
	adc.Timeout = 5 * time.Second
	admin, err := sql.Open("mysql", adc.FormatDSN())
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close() //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Skipf("test mysql not reachable: %v", err)
	}

	schema := fmt.Sprintf("teamster_ro_probe_%d", time.Now().UnixNano())
	roPass := "ro_probe_pw_123456"
	for _, stmt := range []string{
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", schema),
		fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", grafanaReadonlyUser, roPass),
		fmt.Sprintf("ALTER USER '%s'@'%%' IDENTIFIED BY '%s'", grafanaReadonlyUser, roPass),
		fmt.Sprintf("GRANT SELECT ON `%s`.* TO '%s'@'%%'", schema, grafanaReadonlyUser),
		"FLUSH PRIVILEGES",
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", grafanaReadonlyUser))
		_, _ = admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", schema))
	})

	timeout := 3 * time.Second

	if !grafanaReadonlyAuthorizes(host, port, schema, roPass, timeout) {
		t.Errorf("correct password should authorize")
	}
	if grafanaReadonlyAuthorizes(host, port, schema, "wrong-password", timeout) {
		t.Errorf("wrong password must NOT authorize")
	}
	if grafanaReadonlyAuthorizes("127.0.0.1", "1", schema, roPass, timeout) {
		t.Errorf("unreachable host must NOT authorize")
	}
}

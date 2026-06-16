package mysql

import (
	"strings"
	"testing"
)

// TestConvertDSN_BadDSNDoesNotLeakPassword guards the credential-leak class
// that motivated the redaction work: net/url returns a *url.Error whose string
// embeds the RAW DSN — password and all — so wrapping it with %w would print
// the secret to stderr/the feed on any malformed DSN. A space in the password
// is the canonical trigger (url.Parse rejects it as "invalid userinfo" while
// still carrying the value). The returned error must surface the cause, never
// the secret.
//
// Pure unit test — no DB, runs even without TEAMSTER_TEST_MYSQL_DSN.
func TestConvertDSN_BadDSNDoesNotLeakPassword(t *testing.T) {
	const secret = "pass word" // the space is what makes url.Parse fail

	cases := []struct {
		name string
		dsn  string
	}{
		{
			name: "url.Parse failure (space in password)",
			dsn:  "mysql://teamster:" + secret + "@127.0.0.1:3306/teamster",
		},
		{
			name: "wrong scheme carrying a password",
			dsn:  "mysqlx://teamster:" + secret + "@127.0.0.1:3306/teamster",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := convertDSN(tc.dsn)
			if err == nil {
				t.Fatalf("convertDSN(%q) = nil error, want a failure", tc.dsn)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaks password %q: %q", secret, err.Error())
			}
		})
	}
}

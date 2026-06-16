package redact

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	// Shape-identical fake (48 hex chars) standing in for a real DB password —
	// the redactor keys on the -p'<hex>' shape, not the value.
	const live = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	tests := []struct {
		name string
		in   string
		// want is the exact expected output when non-empty.
		want string
		// secret, if non-empty, must NOT appear in the output (substring check).
		secret string
		// keep, if non-empty, must appear in the output (structure preserved).
		keep []string
	}{
		// --- The exact live leak example ---
		{
			name:   "live mysql -p single-quoted hex",
			in:     `mysql -h 127.0.0.1 -P 3306 -u teamster -p'` + live + `' teamster -N -e "SELECT 1"`,
			secret: live,
			keep:   []string{"mysql -h 127.0.0.1 -P 3306 -u teamster -p" + placeholder + " teamster -N", `SELECT 1`},
		},

		// --- mysql attached password forms ---
		{
			name:   "-p attached bare",
			in:     "mysql -u root -pSuperSecret1 db",
			want:   "mysql -u root -p" + placeholder + " db",
			secret: "SuperSecret1",
		},
		{
			name:   "-p single quoted",
			in:     "mysql -u root -p'p@ss w0rd' db",
			want:   "mysql -u root -p" + placeholder + " db",
			secret: "p@ss w0rd",
		},
		{
			name:   "-p double quoted",
			in:     `mysql -u root -p"hunter2" db`,
			want:   "mysql -u root -p" + placeholder + " db",
			secret: "hunter2",
		},
		// --- S1: attached -p is mysql-family only ---
		// Non-mysql -p<x> flags must NOT be touched (ports, port-maps, GNU long).
		{name: "ssh -p port untouched", in: "ssh -p2222 host", want: "ssh -p2222 host"},
		{name: "docker port-map untouched", in: "docker run -p8080:80 img", want: "docker run -p8080:80 img"},
		{name: "rsync -progress untouched", in: "rsync -progress src dst", want: "rsync -progress src dst"},
		{name: "go build -pgo untouched", in: "go build -pgo=auto ./...", want: "go build -pgo=auto ./..."},
		{
			name: "openssl -passin untouched (no mysql token)",
			in:   "openssl req -passin pass:secret",
			want: "openssl req -passin pass:secret",
		},
		// mysql-family variants still masked.
		{
			name:   "mysql -p at start of command",
			in:     "mysql -pSecretAtStart db",
			want:   "mysql -p" + placeholder + " db",
			secret: "SecretAtStart",
		},
		{
			name:   "mariadb -p masked",
			in:     "mariadb -u root -pSEKRIT db",
			want:   "mariadb -u root -p" + placeholder + " db",
			secret: "SEKRIT",
		},
		{
			name:   "mysqldump -p masked",
			in:     "mysqldump -u root -psekrit2 db > dump.sql",
			secret: "sekrit2",
			keep:   []string{"mysqldump -u root -p" + placeholder + " db"},
		},
		{
			name:   "mysql full path -p masked",
			in:     "/usr/bin/mysql -u root -psekrit3 db",
			want:   "/usr/bin/mysql -u root -p" + placeholder + " db",
			secret: "sekrit3",
		},
		{
			name:   "env-prefixed mysql -p masked",
			in:     "MYSQL_PWD=ignore mysql -u root -psekrit4 db",
			secret: "sekrit4",
			keep:   []string{"-p" + placeholder + " db"},
		},

		// --- bare -p (interactive prompt) must be untouched ---
		{
			name: "bare -p no argument trailing",
			in:   "mysql -u root -p",
			want: "mysql -u root -p",
		},
		{
			name: "bare -p no argument followed by db",
			in:   "mysql -u root -p db",
			want: "mysql -u root -p db",
		},

		// --- --password forms ---
		{
			name:   "--password=value",
			in:     "mysql --password=topsecret db",
			want:   "mysql --password=" + placeholder + " db",
			secret: "topsecret",
		},
		{
			name:   "--password space value",
			in:     "mysql --password topsecret db",
			want:   "mysql --password " + placeholder + " db",
			secret: "topsecret",
		},
		{
			name:   "--password quoted",
			in:     `mysql --password='a b c' db`,
			want:   "mysql --password=" + placeholder + " db",
			secret: "a b c",
		},

		// --- DSN userinfo ---
		{
			name:   "mysql:// DSN",
			in:     "TEAMSTER_STORE_DSN=mysql://teamster:" + live + "@127.0.0.1:3306/teamster",
			secret: live,
			keep:   []string{"mysql://teamster:" + placeholder + "@127.0.0.1:3306/teamster"},
		},
		{
			name:   "postgres:// DSN",
			in:     "psql postgres://app:s3cr3t@db.local:5432/app",
			want:   "psql postgres://app:" + placeholder + "@db.local:5432/app",
			secret: "s3cr3t",
		},
		{
			name:   "redis:// DSN",
			in:     "redis-cli -u redis://user:passw0rd@cache:6379",
			want:   "redis-cli -u redis://user:" + placeholder + "@cache:6379",
			secret: "passw0rd",
		},

		// --- env-var secret assignments ---
		{
			name:   "MYSQL_PWD",
			in:     "MYSQL_PWD=abc123 mysql -u root db",
			want:   "MYSQL_PWD=" + placeholder + " mysql -u root db",
			secret: "abc123",
		},
		{
			name:   "PGPASSWORD quoted",
			in:     `PGPASSWORD="some pass" psql -U app`,
			want:   "PGPASSWORD=" + placeholder + " psql -U app",
			secret: "some pass",
		},
		{
			name:   "generic _TOKEN env",
			in:     "GITHUB_TOKEN=ghp_deadbeef gh pr list",
			want:   "GITHUB_TOKEN=" + placeholder + " gh pr list",
			secret: "ghp_deadbeef",
		},
		// S3: a trailing shell separator after the value must survive.
		{
			name:   "MYSQL_PWD with trailing semicolon",
			in:     "MYSQL_PWD=abc123; mysql -u root db",
			want:   "MYSQL_PWD=" + placeholder + "; mysql -u root db",
			secret: "abc123",
		},
		{
			name:   "env value with trailing ampersand",
			in:     "PGPASSWORD=sek&& echo done",
			want:   "PGPASSWORD=" + placeholder + "&& echo done",
			secret: "sek",
		},

		// --- key=value secret params ---
		{
			name:   "password= param in query string",
			in:     "curl 'https://host/db?user=app&password=hunter2&ssl=true'",
			secret: "hunter2",
			keep:   []string{"user=app", "password=" + placeholder, "ssl=true"},
		},
		{
			name:   "token= param",
			in:     "curl 'https://api/x?token=abcdef123'",
			secret: "abcdef123",
			keep:   []string{"token=" + placeholder},
		},
		{
			name:   "api_key= param",
			in:     "fetch api_key=KEY12345",
			want:   "fetch api_key=" + placeholder,
			secret: "KEY12345",
		},
		{
			name:   "access_key= param",
			in:     "aws_args access_key=AKIASECRET",
			want:   "aws_args access_key=" + placeholder,
			secret: "AKIASECRET",
		},
		// S3b: generic key=value must stop at a shell separator.
		{
			name:   "password= param with trailing semicolon",
			in:     "run password=hunter2; echo ok",
			want:   "run password=" + placeholder + "; echo ok",
			secret: "hunter2",
		},
		{
			name:   "token= param with trailing pipe",
			in:     "emit token=abc| grep x",
			want:   "emit token=" + placeholder + "| grep x",
			secret: "abc",
		},

		// --- HTTP auth ---
		{
			name:   "curl -u basic auth",
			in:     "curl -u admin:p4ssword https://host",
			want:   "curl -u admin:" + placeholder + " https://host",
			secret: "p4ssword",
		},
		// --- B1: -u password containing / or @ must be FULLY masked ---
		{
			name:   "-u password with slash",
			in:     "curl -u admin:p4ss/w0rd https://host",
			want:   "curl -u admin:" + placeholder + " https://host",
			secret: "p4ss/w0rd",
		},
		{
			name:   "-u password with at-sign",
			in:     "curl -u admin:secret@evil https://host",
			want:   "curl -u admin:" + placeholder + " https://host",
			secret: "secret@evil",
		},
		{
			name:   "--user password with multiple slashes",
			in:     "curl --user svc:a/b/c https://host",
			want:   "curl --user svc:" + placeholder + " https://host",
			secret: "a/b/c",
		},
		{
			// Regression: a DSN passed as -u <dsn> must be masked by the userinfo
			// rule, not double-mangled by the -u basic-auth rule.
			name:   "-u redis DSN not double-mangled",
			in:     "redis-cli -u redis://user:pass@host:6379",
			want:   "redis-cli -u redis://user:" + placeholder + "@host:6379",
			secret: "pass@host",
		},
		// --- B2: -u password STARTING with / or @ must be FULLY masked ---
		{
			name:   "-u password leading slash (path-like)",
			in:     "curl -u user:/etc/passwd https://host",
			want:   "curl -u user:" + placeholder + " https://host",
			secret: "/etc/passwd",
		},
		{
			name:   "-u password leading double-slash",
			in:     "curl -u user://x https://host",
			want:   "curl -u user:" + placeholder + " https://host",
			secret: "//x",
		},
		{
			name: "-u password is just a slash",
			in:   "curl -u user:/ https://host",
			want: "curl -u user:" + placeholder + " https://host",
		},
		{
			name: "-u bare username no colon untouched",
			in:   "curl -u plainuser https://host",
			want: "curl -u plainuser https://host",
		},
		{
			name:   "Authorization Bearer header",
			in:     `curl -H "Authorization: Bearer eyJhbGciOi.token.sig" https://host`,
			secret: "eyJhbGciOi.token.sig",
			keep:   []string{"Authorization: Bearer " + placeholder},
		},
		{
			name:   "Authorization Basic header",
			in:     `curl -H "Authorization: Basic dXNlcjpwYXNz" https://host`,
			secret: "dXNlcjpwYXNz",
			keep:   []string{"Authorization: Basic " + placeholder},
		},

		// --- negative: ordinary commands untouched ---
		{name: "git status", in: "git status", want: "git status"},
		{name: "ls -p", in: "ls -p src/", want: "ls -p src/"},
		{name: "grep -r", in: "grep -r foo src/", want: "grep -r foo src/"},
		{name: "go test", in: "go test ./... -p 4", want: "go test ./... -p 4"},
		{
			name: "mysql no password",
			in:   "mysql -h 127.0.0.1 -u teamster teamster -N -e 'SELECT 1'",
			want: "mysql -h 127.0.0.1 -u teamster teamster -N -e 'SELECT 1'",
		},
		{
			name: "plain url no userinfo",
			in:   "curl https://example.com/path?q=1",
			want: "curl https://example.com/path?q=1",
		},
		{name: "empty string", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)

			if tt.want != "" {
				if got != tt.want {
					t.Errorf("Redact(%q)\n  got:  %q\n  want: %q", tt.in, got, tt.want)
				}
			}
			if tt.secret != "" && strings.Contains(got, tt.secret) {
				t.Errorf("Redact(%q) leaked secret %q in output %q", tt.in, tt.secret, got)
			}
			for _, k := range tt.keep {
				if !strings.Contains(got, k) {
					t.Errorf("Redact(%q) dropped expected structure %q\n  got: %q", tt.in, k, got)
				}
			}
		})
	}
}

// TestRedactIdempotent ensures redacting an already-redacted string is a no-op,
// which matters because the relay re-POSTs hub JSONL through hookd's choke point
// a second time.
func TestRedactIdempotent(t *testing.T) {
	inputs := []string{
		"mysql -u root -p'secret' db",
		"TEAMSTER_STORE_DSN=mysql://teamster:secret@127.0.0.1:3306/teamster",
		"MYSQL_PWD=abc123 mysql -u root db",
		`curl -H "Authorization: Bearer tok" https://host`,
	}
	for _, in := range inputs {
		once := Redact(in)
		twice := Redact(once)
		if once != twice {
			t.Errorf("Redact not idempotent for %q\n  once:  %q\n  twice: %q", in, once, twice)
		}
	}
}

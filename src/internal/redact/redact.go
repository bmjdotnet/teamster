// Package redact masks credentials in shell command strings before they are
// persisted or displayed. It is a focused, defense-in-depth redactor for the
// secret shapes that show up in Teamster activity (DB passwords on argv, DSN
// userinfo, env-var secret assignments, HTTP auth headers) — not a universal
// secret scanner. The goal is that an inlined credential never reaches the
// JSONL contract (and from there feed, the dashboard, and the public relay
// mirror), while ordinary commands pass through untouched.
//
// Standalone package with no third-party deps to avoid import cycles: it is
// imported by both the hook producer (internal/hook) and the ingest choke
// point (internal/server).
package redact

import (
	"regexp"
	"strings"
)

// placeholder replaces the secret portion of a match. The surrounding flag,
// key, or structure is preserved so the command stays readable.
const placeholder = "<redacted>"

// rule pairs a compiled pattern with the replacement template. Each pattern
// uses a capture group named in the template ($1, $2…) for the structure to
// keep; the secret-bearing group is dropped in favour of placeholder.
type rule struct {
	re   *regexp.Regexp
	repl string
}

// mysqlFamily detects a mysql-family invocation anywhere in the command. The
// word boundary handles a leading path (/usr/bin/mysql) and an env-prefix
// (MYSQL_PWD=… mysql). Used to gate the attached -p<x> rules, which are only
// unambiguous for these tools — elsewhere -p means a port, progress flag, etc.
var mysqlFamily = regexp.MustCompile(`\b(?:mysql|mariadb|mysqldump|mysqladmin|mysqlshow|mysqlcheck)\b`)

// mysqlRules apply only when the command is a mysql-family invocation. The
// attached-password forms (-pSECRET, -p'SECRET', -p"SECRET") are too broad to
// run unconditionally: -p is also ssh's port, docker's port-map, etc. A bare
// -p with no attached argument (interactive prompt) is left alone — every
// pattern requires at least one secret character after -p.
var mysqlRules = []rule{
	{
		re:   regexp.MustCompile(`(^|\s)(-p)'[^']*'`),
		repl: `${1}${2}` + placeholder,
	},
	{
		re:   regexp.MustCompile(`(^|\s)(-p)"[^"]*"`),
		repl: `${1}${2}` + placeholder,
	},
	{
		re:   regexp.MustCompile(`(^|\s)(-p)[^\s'"]+`),
		repl: `${1}${2}` + placeholder,
	},
}

// userAuthRe matches a curl -u/--user argument, capturing the flag, separator,
// and the whole value up to a shell separator. The value is inspected by
// redactUserAuth — a regex alone can't both fully mask a password of any shape
// AND skip a credentialed DSN, because RE2 has no lookahead.
var userAuthRe = regexp.MustCompile(`(-u|--user)(=|\s+)([^\s;&|]+)`)

// redactUserAuth masks the password in a curl basic-auth argument, fully and
// regardless of the password's leading character (/, @, //, anything).
// Match-then-decide on the captured value:
//   - value already contains the placeholder -> a credentialed DSN that the
//     userinfo rule (which runs first) has already masked; return unchanged so
//     it is not double-mangled (e.g. -u redis://user:pass@host).
//   - value has a ":" -> basic auth user:pass; mask everything after the FIRST
//     ":" (the whole password, whatever it starts with).
//   - value has no ":" -> a bare username; not a secret; return unchanged.
//
// A credential-free DSN URL passed as -u (e.g. -u redis://host:6379, no auth)
// is over-masked to host:<redacted> — a fail-safe (cosmetic, no leak) accepted
// to keep the leak path closed for every password shape.
func redactUserAuth(s string) string {
	return userAuthRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := userAuthRe.FindStringSubmatch(m)
		flag, sep, val := sub[1], sub[2], sub[3]
		if strings.Contains(val, placeholder) {
			return m
		}
		i := strings.IndexByte(val, ':')
		if i < 0 {
			return m
		}
		return flag + sep + val[:i+1] + placeholder
	})
}

// rules are applied in order, unconditionally. Order matters where patterns can
// overlap: more specific shapes (DSN userinfo, env assignments) precede the
// generic key=value net so the structural group is preserved correctly.
//
// Unquoted value classes stop at shell separators (;, &, |, whitespace) so a
// trailing separator survives redaction (e.g. MYSQL_PWD=x; mysql … keeps the ;).
var rules = []rule{
	// DSN / URL userinfo: scheme://user:SECRET@host -> scheme://user:<redacted>@host
	// Covers mysql://, postgres://, redis://, and any scheme://user:pass@ form.
	// Runs before the -u rule so a DSN passed as -u <dsn> is masked here, not
	// mistaken for basic auth below.
	{
		re:   regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:)[^\s@/]+(@)`),
		repl: `${1}` + placeholder + `${2}`,
	},

	// --password=SECRET / --password SECRET (and -password variants). Unambiguous,
	// so it runs unconditionally for every tool.
	{
		re:   regexp.MustCompile(`(--?password)(=|\s+)('[^']*'|"[^"]*"|[^\s;&|]+)`),
		repl: `${1}${2}` + placeholder,
	},

	// Env-var secret assignments on the command line:
	// MYSQL_PWD=…, PGPASSWORD=…, and any *_PWD= / *PASSWORD= / *_TOKEN= /
	// *_SECRET= / *API_KEY=. Keep the var name, drop the value.
	{
		re:   regexp.MustCompile(`(?i)(^|\s)([A-Z][A-Z0-9_]*(?:PASSWORD|PASSWD|_PWD|_TOKEN|_SECRET|API_?KEY)=)('[^']*'|"[^"]*"|[^\s;&|]+)`),
		repl: `${1}${2}` + placeholder,
	},

	// HTTP Authorization header values (Bearer / Basic / any scheme), inside
	// -H "Authorization: …" or a bare Authorization: … in the command.
	{
		re:   regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic|token|digest)\s+)[^\s"']+`),
		repl: `${1}` + placeholder,
	},

	// Generic secret key=value params in query strings / DSNs / argv:
	// password=, passwd=, pwd=, token=, api_key=, apikey=, secret=, access_key=.
	// Runs last so the structural shapes above win on overlap. The value class
	// excludes shell separators (;, &, |) so a trailing separator survives.
	{
		re:   regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|api[_-]?key|secret|access[_-]?key)=('[^']*'|"[^"]*"|[^\s;&|'"]+)`),
		repl: `${1}=` + placeholder,
	},
}

// Redact returns s with recognized credential shapes masked. The flag, key, or
// URL structure is preserved; only the secret value is replaced with
// "<redacted>". Strings with no recognized secret are returned unchanged.
func Redact(s string) string {
	if s == "" {
		return s
	}
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	s = redactUserAuth(s)
	if mysqlFamily.MatchString(s) {
		for _, r := range mysqlRules {
			s = r.re.ReplaceAllString(s, r.repl)
		}
	}
	return s
}

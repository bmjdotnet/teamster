package clonedata

import (
	"fmt"
	"net/url"
)

// CloneVerifyROUser is the fixed username installrunner.sh's
// provision_clone_verify_ro provisions (I7) — mirrored here so callers
// don't hand-duplicate the literal.
const CloneVerifyROUser = "clone_verify_ro"

// VerifyRODSN builds the clone_verify_ro DSN for db, given the source
// instance's own store DSN (for scheme/host/port only) and the
// clone_verify_ro password read from the install-time password file
// (<basedir>/var/clone/clone_verify_ro_password). The database segment is
// replaced with db so the same credential can target either teamster or
// claude_telemetry — mirroring internal/backup/mysql.go's dsnForDatabase,
// which does the identical URL surgery for the app DSN.
func VerifyRODSN(sourceDSN, password, db string) (string, error) {
	u, err := url.Parse(sourceDSN)
	if err != nil {
		return "", fmt.Errorf("clonedata: parse source DSN: %w", err)
	}
	u.User = url.UserPassword(CloneVerifyROUser, password)
	u.Path = "/" + db
	return u.String(), nil
}

package clone

import (
	"context"
	"fmt"
	"strings"
)

// TargetInfo captures facts interrogated from a clone target once at the
// start of a run, so every remote path clone builds afterward is absolute
// (rooted at the target's own $HOME) rather than a literal ~ that depends
// on remote shell expansion — the source of a recurring class of quoting
// bugs (see ProbeTarget, runScriptOnTarget).
type TargetInfo struct {
	Home string // target's $HOME, always an absolute path
}

// ProbeTarget interrogates target over SSH once, at clone start, to learn
// its absolute $HOME. Every remote path constructed downstream (targetDir,
// the installed teamster binary path, .claude/settings.json, ...) is built
// from this value via path.Join, never from a literal ~ — SSH does not
// reliably expand a tilde once the argument carrying it has been
// shell-quoted, which runScriptOnTarget always does.
func ProbeTarget(ctx context.Context, sshRun SSHRunner, target string) (TargetInfo, error) {
	out, err := sshRun(ctx, target, "printenv", "HOME")
	if err != nil {
		return TargetInfo{}, fmt.Errorf("clone: probing %s for $HOME: %w", target, err)
	}
	home := strings.TrimSpace(out)
	if !strings.HasPrefix(home, "/") {
		return TargetInfo{}, fmt.Errorf("clone: probing %s for $HOME: unexpected output %q (want an absolute path)", target, out)
	}
	return TargetInfo{Home: home}, nil
}

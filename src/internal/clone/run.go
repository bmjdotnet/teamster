// Package clone implements Leg 1 (ref resolution) and the ship half of Leg 2
// (source transport, provenance verification) of `teamster clone`. See the
// WP1 ref-and-sources design doc for the full specification.
package clone

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CommandRunner executes a local command and returns combined stdout+stderr.
// Injected everywhere a *git*/*ssh*/*scp* subprocess is needed so the
// resolution and shipping logic is testable without a real git repo, SSH
// target, or network.
type CommandRunner func(ctx context.Context, dir, name string, args ...string) (output string, err error)

// DefaultRunner shells out via os/exec. dir is the working directory ("" for
// the caller's own cwd).
func DefaultRunner(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

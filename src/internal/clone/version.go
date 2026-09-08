package clone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrUnstamped means a channel reported commit "none" or "" — the linker
// default for a binary lib/installrunner.sh never stamped via -ldflags.
// Never trust that value as an identity signal (WP1 §1.1).
var ErrUnstamped = errors.New("clone: binary was never built by lib/installrunner.sh (unstamped commit)")

// versionOutputRe parses `teamster --version` / `hookd --version` output:
// "<program> <version> (<commit>, <build_time>)\n". Both binaries share the
// same version.String() format, one -ldflags stamp. The commit group is
// deliberately not hex-restricted (unlike WP1 §1.1's illustrative regex) —
// the linker's own unstamped default is the literal word "none", which
// contains non-hex letters, and letting it match here means validateCommit
// produces the specific ErrUnstamped diagnosis instead of a generic parse
// failure.
var versionOutputRe = regexp.MustCompile(`^(\S+)\s+(\S+)\s+\(([^,\s]+),\s*([^)]+)\)\s*$`)

// BuildDisclosure is what a source instance reports about the build it is
// currently running.
type BuildDisclosure struct {
	Commit    string // short hash, as self-reported — never "none" or ""
	Version   string // human-readable version string (may be "dev")
	BuildTime string // empty when the channel doesn't report one (e.g. /health)
	Channel   string // e.g. "teamster --version", "GET http://host:9125/health" — for --dry-run / error text
}

func validateCommit(commit string) error {
	if commit == "" || commit == "none" {
		return ErrUnstamped
	}
	return nil
}

// ParseVersionOutput parses the stdout of `teamster --version` / `hookd
// --version`.
func ParseVersionOutput(output string) (commit, version, buildTime string, err error) {
	m := versionOutputRe.FindStringSubmatch(strings.TrimSpace(output))
	if m == nil {
		return "", "", "", fmt.Errorf("clone: unrecognized version output: %q", output)
	}
	return m[3], m[2], m[4], nil
}

// QueryLocalBuild shells out to `<binary> --version` and parses the result.
// This is the zero-network-hop channel: no daemon needs to be reachable,
// just the binary on disk. binary may be a bare name resolved via PATH or an
// absolute path.
func QueryLocalBuild(ctx context.Context, run CommandRunner, binary string) (BuildDisclosure, error) {
	out, err := run(ctx, "", binary, "--version")
	if err != nil {
		return BuildDisclosure{}, fmt.Errorf("clone: querying local build via %q: %w", binary, err)
	}
	commit, version, buildTime, err := ParseVersionOutput(out)
	if err != nil {
		return BuildDisclosure{}, err
	}
	if err := validateCommit(commit); err != nil {
		return BuildDisclosure{}, fmt.Errorf("%w (from %q --version, output: %q)", err, binary, strings.TrimSpace(out))
	}
	return BuildDisclosure{
		Commit:  commit,
		Version: version, BuildTime: buildTime,
		Channel: fmt.Sprintf("%s --version", binary),
	}, nil
}

// healthResponse is the subset of GET /health's JSON this package reads.
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// QueryRemoteHealth fetches GET <hookdBase>/health and parses the build
// fields. Used only when --source names a host other than the one `teamster
// clone` is running on (R9: clone always runs from the source host, so the
// local CLI channel is the default; this is the override path).
func QueryRemoteHealth(ctx context.Context, client *http.Client, hookdBase string) (BuildDisclosure, error) {
	url := strings.TrimRight(hookdBase, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return BuildDisclosure{}, fmt.Errorf("clone: building request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return BuildDisclosure{}, fmt.Errorf("clone: GET %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return BuildDisclosure{}, fmt.Errorf("clone: GET %s: unexpected status %d", url, resp.StatusCode)
	}
	var hr healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		return BuildDisclosure{}, fmt.Errorf("clone: GET %s: decoding response: %w", url, err)
	}
	if err := validateCommit(hr.Commit); err != nil {
		return BuildDisclosure{}, fmt.Errorf("%w (from GET %s, commit: %q)", err, url, hr.Commit)
	}
	return BuildDisclosure{Commit: hr.Commit, Version: hr.Version, Channel: fmt.Sprintf("GET %s", url)}, nil
}

// hookdBaseFor resolves a --source value to a hookd base URL: a bare
// hostname defaults to http://<host>:9125, a full URL is used as given
// (WP1 §1.1).
func hookdBaseFor(source string) string {
	if strings.Contains(source, "://") {
		return source
	}
	return fmt.Sprintf("http://%s:9125", source)
}

// isLocalSource reports whether source names the host `teamster clone` is
// actually running on — "" (unset) always does, per R9 (clone always runs
// on the source host, so the local CLI channel is the default path, not a
// heuristic "preferred when" case).
func isLocalSource(source, thisHost string) bool {
	if source == "" {
		return true
	}
	if thisHost == "" {
		thisHost, _ = os.Hostname()
	}
	host := source
	if strings.Contains(host, "://") {
		if u, err := url.Parse(host); err == nil {
			host = u.Hostname()
		}
	}
	switch host {
	case "localhost", "127.0.0.1", thisHost:
		return true
	default:
		return false
	}
}

// SourceQueryOptions configures ResolveBuild.
type SourceQueryOptions struct {
	Source      string        // --source value; "" = local instance (the common case per R9)
	LocalBinary string        // binary to shell out to for the local channel; default "teamster"
	Hostname    string        // this host's hostname, for the isLocalSource check; "" = os.Hostname()
	HTTPClient  *http.Client  // used only for the remote channel
	Run         CommandRunner // used only for the local channel
}

// ResolveBuild determines the commit/version the source instance is
// currently running, via the local CLI channel (preferred, R9) or the
// remote HTTP channel (only when --source names a different host). Does
// nothing when the operator supplied --ref — that's an explicit override
// that skips disclosure entirely (WP1 §1.1); callers check --ref first and
// only call this when it's absent.
func ResolveBuild(ctx context.Context, opts SourceQueryOptions) (BuildDisclosure, error) {
	binary := opts.LocalBinary
	if binary == "" {
		binary = "teamster"
	}
	if isLocalSource(opts.Source, opts.Hostname) {
		run := opts.Run
		if run == nil {
			run = DefaultRunner
		}
		return QueryLocalBuild(ctx, run, binary)
	}
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return QueryRemoteHealth(ctx, client, hookdBaseFor(opts.Source))
}

// LocalTeamsterBinary returns the path to the teamster binary alongside the
// currently running executable, falling back to the bare name (resolved via
// PATH) when that can't be determined. `teamster clone` passes this as
// ResolveRefOptions.LocalBinary so the local disclosure channel queries the
// actual installed binary on disk rather than reading its own in-process
// version vars — matching WP1 §1.1's "shell out to teamster --version"
// design (the installed binary is the source of truth for what's running,
// which is not guaranteed to be byte-identical to the currently-executing
// `teamster clone` invocation).
func LocalTeamsterBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "teamster"
	}
	return filepath.Join(filepath.Dir(exe), "teamster")
}

package clone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrFingerprintMismatch means the extracted tree on the target does not
// match what the source intended to ship — the real provenance gate (WP1
// §2 step 6, red-team §D3). Unlike Stage A's binary-readback check (§5),
// this catches a wrong-directory install, a corrupted-but-checksum-matching
// extraction, and a stale-reused directory: it is independent of anything
// clone itself injects into the pipeline.
var ErrFingerprintMismatch = errors.New("clone: content-manifest fingerprint mismatch — extracted tree does not match the resolved commit")

// sha256Lines canonicalizes a set of "<blob-sha> <path>" lines into one
// comparable fingerprint: sort (byte-wise, matching LC_ALL=C on the target
// side and Go's default string ordering here), join with newlines, sha256.
// This is what makes clean-mode and dirty-mode fingerprints, and the
// source/target sides of either, byte-comparable — the two ends only ever
// need to agree on this shape, never on tar/gzip encoding details.
func sha256Lines(lines []string) string {
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	joined := strings.Join(sorted, "\n")
	if joined != "" {
		joined += "\n"
	}
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:])
}

// SourceFingerprintClean computes the expected_fingerprint for a clean
// (non-dirty) ship: git's own content-addressed blob hashes for every file
// in fullHash's tree (WP1 §2 step 6). Cheap — reads objects git already
// has, no extraction or recomputation.
func SourceFingerprintClean(ctx context.Context, run CommandRunner, source, fullHash string) (string, error) {
	out, err := run(ctx, "", "git", "-C", source, "ls-tree", "-r", fullHash)
	if err != nil {
		return "", fmt.Errorf("clone: listing tree for %s: %w", fullHash, err)
	}
	lines, err := parseLsTree(out)
	if err != nil {
		return "", fmt.Errorf("clone: parsing ls-tree output for %s: %w", fullHash, err)
	}
	return sha256Lines(lines), nil
}

// parseLsTree extracts "<blob-sha> <path>" lines from `git ls-tree -r`
// output ("<mode> <type> <sha>\t<path>" per line).
func parseLsTree(out string) ([]string, error) {
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return nil, nil
	}
	rawLines := strings.Split(out, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, raw := range rawLines {
		meta, path, ok := strings.Cut(raw, "\t")
		if !ok {
			return nil, fmt.Errorf("unexpected ls-tree line (no tab): %q", raw)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected ls-tree metadata: %q", meta)
		}
		lines = append(lines, fields[2]+" "+path)
	}
	return lines, nil
}

// SourceFingerprintDirty computes the expected_fingerprint for a dirty
// (--allow-dirty) ship: git's blob-hash algorithm run over the enumerated
// dirty fileset (WP1 §2 step 6, EnumerateDirtyFiles) as it currently sits
// on disk — tracked-and-modified or untracked-but-not-ignored alike.
// git ls-tree -r <full_hash> describes only the committed tree, which is
// not a valid comparison target when the shipped content deliberately
// diverges from it.
func SourceFingerprintDirty(ctx context.Context, run CommandRunner, repoDir string, files []string) (string, error) {
	lines := make([]string, 0, len(files))
	for _, f := range files {
		out, err := run(ctx, repoDir, "git", "hash-object", f)
		if err != nil {
			// A file git ls-files --cached lists but that no longer exists on
			// disk (deleted-but-unstaged) — skip it rather than fail the whole
			// ship (WP1 §4's noted edge case).
			continue
		}
		lines = append(lines, strings.TrimSpace(out)+" "+f)
	}
	return sha256Lines(lines), nil
}

// remoteFingerprintScript is executed on the target via `ssh <target> bash
// -s -- <targetDir>` (piped to stdin). It reconstructs the same
// "<blob-sha> <path>" listing shape as the source side using
// `git hash-object` — a pure content-hashing primitive that needs no .git
// directory, which is exactly what makes it work on a `git archive`-shipped
// tree (verified live: `git hash-object` succeeds from a directory with no
// repository anywhere above it — see HANDOFF-delivery.md §2). Prints one
// line: the sha256 hex digest.
const remoteFingerprintScript = `set -euo pipefail
target_dir="$1"
cd "$target_dir"
tmp_paths=$(mktemp)
trap 'rm -f "$tmp_paths"' EXIT
find . -type f | sed 's|^\./||' | LC_ALL=C sort > "$tmp_paths"
if [ -s "$tmp_paths" ]; then
  paste -d' ' <(git hash-object --stdin-paths < "$tmp_paths") "$tmp_paths" | LC_ALL=C sort | sha256sum | awk '{print $1}'
else
  printf '%s\n' "$(printf '' | sha256sum | awk '{print $1}')"
fi
`

// TargetFingerprint reconstructs actual_fingerprint on the target by
// running remoteFingerprintScript over the freshly-extracted directory
// (WP1 §2 step 6). Must run before the installer is ever invoked.
func TargetFingerprint(ctx context.Context, runScript SSHScriptRunner, target, targetDir string) (string, error) {
	out, err := runScript(ctx, target, remoteFingerprintScript, targetDir)
	if err != nil {
		return "", fmt.Errorf("clone: computing target fingerprint on %s:%s: %w", target, targetDir, err)
	}
	sum := strings.TrimSpace(out)
	if len(sum) != 64 {
		return "", fmt.Errorf("clone: unexpected fingerprint output from %s: %q", target, out)
	}
	return sum, nil
}

// CompareFingerprints hard-fails (ErrFingerprintMismatch) on a mismatch —
// this must run before lib/installrunner.sh is ever invoked, not after.
func CompareFingerprints(expected, actual string) error {
	if expected != actual {
		return fmt.Errorf("%w: expected %s, got %s", ErrFingerprintMismatch, expected, actual)
	}
	return nil
}

// Package clonedata implements the pure, backend-independent logic behind
// Leg 3 of `teamster clone` (WP3-data-leg.md): which files in a backup
// snapshot must travel to the target, diffing post-restore row counts, and
// building the clone_verify_ro (I7) DSN. SSH/CLI orchestration (transfer,
// restore invocation, daemon quiesce) is wired in cmd/teamster/clone_data.go
// — this package has no SSH, no subprocess, no network.
package clonedata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/bmjdotnet/teamster/internal/backup"
)

// TransferableStores is the fixed set of backup stores clone ever transfers.
// prometheus and grafana are out of v1 scope (WP3-data-leg.md's "What's
// restored vs refused" table): the source hub disables prometheus backup
// entirely, and grafana's clone-side install produces its own dashboards
// rather than restoring the source hub's — nothing in either store is ever
// a transfer candidate, regardless of what a manifest happens to say.
var TransferableStores = map[string]bool{
	"mysql":    true,
	"otel":     true,
	"teamster": true,
}

// excludedFiles are files inside otherwise-transferable stores that must
// never travel — I1's actual v1 mechanism (R8). Not transferring
// config.tar.gz is what satisfies I1: RestoreTeamster silently skips a
// missing config.tar.gz, so no restore-time flag is needed. It is also the
// one file carrying the credential/relay-target payload (I2/I3), so
// omitting it at the transfer layer is a second, independent layer of the
// isolation contract, not merely a restore-time discard after arrival.
var excludedFiles = map[string]bool{
	"teamster/config.tar.gz": true,
}

// TransferCandidates reads manifest.json from a local backup snapshot
// directory and returns the manifest-relative file paths that must reach
// the target, in deterministic order with manifest.json always first. Only
// files belonging to a store whose manifest status is "ok" AND that is in
// TransferableStores are candidates — a store the source's own backup
// skipped or errored on, or one that's out of clone's v1 scope, is never a
// candidate regardless of what's physically present on disk.
func TransferCandidates(snapshotDir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(snapshotDir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("clonedata: read manifest: %w", err)
	}
	var m backup.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("clonedata: parse manifest: %w", err)
	}

	var rest []string
	for storeName, res := range m.Stores {
		if res.Status != "ok" || !TransferableStores[storeName] {
			continue
		}
		for _, f := range res.Files {
			if excludedFiles[f] {
				continue
			}
			rest = append(rest, f)
		}
	}
	sort.Strings(rest)
	return append([]string{"manifest.json"}, rest...), nil
}

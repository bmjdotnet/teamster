package statusui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// sweepState is the shape of sweep-state.json on disk.
type sweepState struct {
	LastRunTimestamp float64 `json:"last_run_timestamp"`
}

// fetchSweepAge reads sweep-state.json and returns a human-readable age string.
func fetchSweepAge(dataDir string) string {
	path := filepath.Join(dataDir, "sweep-state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "never"
	}
	var ss sweepState
	if err := json.Unmarshal(data, &ss); err != nil || ss.LastRunTimestamp == 0 {
		return "never"
	}
	t := time.Unix(int64(ss.LastRunTimestamp), 0)
	age := time.Since(t)
	switch {
	case age < 2*time.Minute:
		return fmt.Sprintf("%d sec ago", int(age.Seconds()))
	case age < 2*time.Hour:
		return fmt.Sprintf("%d min ago", int(age.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	}
}

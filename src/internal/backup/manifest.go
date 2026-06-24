package backup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Manifest struct {
	Version          string                 `json:"version"`
	Hostname         string                 `json:"hostname"`
	TeamsterVersion  string                 `json:"teamster_version"`
	Timestamp        time.Time              `json:"timestamp"`
	DurationMS       int64                  `json:"duration_ms"`
	ConfigPath       string                 `json:"config_path"`
	Stores           map[string]StoreResult `json:"stores"`
}

type StoreResult struct {
	Status     string   `json:"status"`
	Files      []string `json:"files,omitempty"`
	TotalBytes int64    `json:"total_bytes,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	Error      string   `json:"error,omitempty"`
}

func writeManifest(destDir string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	return os.WriteFile(filepath.Join(destDir, "manifest.json"), data, 0o644)
}

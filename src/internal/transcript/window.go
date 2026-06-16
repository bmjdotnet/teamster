package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TranscriptLine is one user or assistant message within a timestamp window,
// suitable for feeding to an LLM synthesis pass.
type TranscriptLine struct {
	Role      string    // "user" or "assistant"
	Content   string    // flattened text from content blocks (or bare string)
	Timestamp time.Time // RFC3339 from the JSONL line
}

// ReadWindow reads transcript lines whose timestamp falls within [start, end).
// Returns up to maxLines user/assistant messages from the session's main
// transcript file, suitable for LLM synthesis. The JSONL is scanned line by
// line — no full-file load.
func ReadWindow(sessionID, projectsDir string, start, end time.Time, maxLines int) ([]TranscriptLine, error) {
	if projectsDir == "" {
		projectsDir = filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	}

	mains, err := filepath.Glob(filepath.Join(projectsDir, "*", sessionID+".jsonl"))
	if err != nil {
		return nil, fmt.Errorf("glob session transcript: %w", err)
	}

	var result []TranscriptLine
	for _, path := range mains {
		lines, err := readWindowFile(path, start, end, maxLines-len(result))
		if err != nil {
			return nil, err
		}
		result = append(result, lines...)
		if len(result) >= maxLines {
			break
		}
	}
	return result, nil
}

func readWindowFile(path string, start, end time.Time, remaining int) ([]TranscriptLine, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)

	var result []TranscriptLine
	for scanner.Scan() {
		if len(result) >= remaining {
			break
		}
		raw := scanner.Bytes()

		var line Line
		if err := json.Unmarshal(raw, &line); err != nil {
			continue
		}
		if line.Type != "user" && line.Type != "assistant" {
			continue
		}
		if line.Timestamp.Before(start) || !line.Timestamp.Before(end) {
			continue
		}

		text := flattenContent(line.Message.Content)
		if text == "" {
			continue
		}

		result = append(result, TranscriptLine{
			Role:      line.Type,
			Content:   text,
			Timestamp: line.Timestamp,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return result, nil
}

func flattenContent(fc FlexContent) string {
	if fc.Text != "" {
		return fc.Text
	}
	var parts []string
	for _, b := range fc.Blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

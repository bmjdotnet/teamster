// Command relay tails a Teamster JSONL event file and forwards each line
// to a remote hookd's POST /event endpoint.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bmjdotnet/teamster/internal/logging"
	"github.com/bmjdotnet/teamster/internal/version"
)

func main() {
	source := flag.String("source", "", "JSONL file to tail (default: $TEAMSTER_DATA_DIR/events.jsonl)")
	target := flag.String("target", "", "destination hookd URL (e.g. http://demo:9125/event)")
	history := flag.Int("history", 0, "number of historical lines to replay on start")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("relay", version.String())
		os.Exit(0)
	}

	log := logging.Init("relay")

	if *source == "" {
		if dir := os.Getenv("TEAMSTER_DATA_DIR"); dir != "" {
			*source = dir + "/events.jsonl"
		} else if base := os.Getenv("TEAMSTER_BASEDIR"); base != "" {
			*source = base + "/var/events.jsonl"
		} else {
			log.Error("--source is required (or set TEAMSTER_DATA_DIR / TEAMSTER_BASEDIR)")
			os.Exit(1)
		}
	}
	if *target == "" {
		log.Error("--target is required")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting relay", "source", *source, "target", *target, "history", *history)

	f, err := os.Open(*source)
	if err != nil {
		log.Error("open source", "error", err)
		os.Exit(1)
	}
	defer f.Close()

	offset := seekStart(f, *history)
	client := &http.Client{Timeout: 5 * time.Second}
	reader := newChunkReader(f, *source)
	var forwarded, errCount int64
	var consecFails int

	for {
		if newOffset, rotated := reader.checkRotation(offset); rotated {
			log.Warn("source file rotated or truncated, resuming from new head", "old_offset", offset, "new_offset", newOffset)
			offset = newOffset
		}

		line, newOffset, ok := reader.readLine(offset)
		if ok {
			offset = newOffset
			if err := forward(ctx, client, *target, line); err != nil {
				errCount++
				consecFails++
				log.Warn("forward failed", "error", err, "errors_total", errCount)
				backoff := time.Duration(consecFails) * 500 * time.Millisecond
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
				select {
				case <-ctx.Done():
					log.Info("shutting down", "forwarded", forwarded, "errors", errCount)
					return
				case <-time.After(backoff):
				}
			} else {
				forwarded++
				consecFails = 0
				if forwarded%100 == 0 {
					log.Info("progress", "forwarded", forwarded, "errors", errCount)
				}
			}
			continue
		}

		select {
		case <-ctx.Done():
			log.Info("shutting down", "forwarded", forwarded, "errors", errCount)
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// seekStart positions the read offset. If history > 0, it seeks back that
// many newlines from the end; otherwise it seeks to the end of the file.
func seekStart(f *os.File, history int) int64 {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return 0
	}
	if history <= 0 {
		return info.Size()
	}

	// Scan backward counting newlines.
	pos := info.Size() - 1
	found := 0
	buf := make([]byte, 1)
	for pos > 0 {
		if _, err := f.ReadAt(buf, pos); err != nil {
			break
		}
		if buf[0] == '\n' {
			found++
			if found > history {
				return pos + 1
			}
		}
		pos--
	}
	return 0
}

const chunkSize = 8192

// chunkReader buffers file reads in 8KB chunks to avoid per-byte syscalls.
// It also tracks the source path and inode so checkRotation can detect both
// an in-place truncation (logrotate's copytruncate, which this file is
// configured for) and a replacement (a rename/create-style rotation, which
// copytruncate isn't, but the check costs nothing extra and any operator
// could switch the stanza later) — without either, a rotation stalls the
// relay permanently: ReadAt past the new, smaller EOF returns loaded==0 and
// readLine returns ok=false forever, with no error and no log line
// explaining why forwarding stopped.
type chunkReader struct {
	f      *os.File
	path   string
	ino    uint64
	buf    [chunkSize]byte
	start  int64 // file offset where buf[0] was read from
	loaded int   // valid bytes in buf
}

func newChunkReader(f *os.File, path string) *chunkReader {
	return &chunkReader{f: f, path: path, ino: inodeOf(f)}
}

// inodeOf returns f's inode, or 0 if it can't be determined (e.g. a
// platform whose Stat().Sys() isn't a *syscall.Stat_t) — checkRotation
// treats 0 as "unknown," never as "matches," so a platform where this can't
// be read simply never detects a rename-style rotation, only a truncation.
func inodeOf(f *os.File) uint64 {
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		return sys.Ino
	}
	return 0
}

// checkRotation re-stats the path (not the open fd, which no longer sees a
// replaced file's new content) and, if the on-disk file was replaced or has
// shrunk below the current offset, reopens or resets so the tail resumes
// from the new head instead of stalling. Returns the offset to continue
// from and whether anything changed.
func (cr *chunkReader) checkRotation(offset int64) (int64, bool) {
	info, err := os.Stat(cr.path)
	if err != nil {
		return offset, false
	}

	if sys, ok := info.Sys().(*syscall.Stat_t); ok && cr.ino != 0 && sys.Ino != cr.ino {
		newF, err := os.Open(cr.path)
		if err != nil {
			// Transient — e.g. mid-rename. Keep the old fd and try again
			// next pass rather than losing the source entirely.
			return offset, false
		}
		cr.f.Close()
		cr.f = newF
		cr.ino = sys.Ino
		cr.start, cr.loaded = 0, 0
		return 0, true
	}

	if info.Size() < offset {
		cr.start, cr.loaded = 0, 0
		return 0, true
	}

	return offset, false
}

// fill reads a chunk from the file starting at the given offset.
func (cr *chunkReader) fill(offset int64) {
	n, _ := cr.f.ReadAt(cr.buf[:], offset)
	cr.start = offset
	cr.loaded = n
}

// readLine reads one newline-terminated line starting at offset.
// Returns the trimmed line, the new offset, and whether a line was found.
func (cr *chunkReader) readLine(offset int64) (string, int64, bool) {
	var line []byte
	pos := offset

	for {
		bufIdx := int(pos - cr.start)
		if bufIdx < 0 || bufIdx >= cr.loaded || pos < cr.start {
			cr.fill(pos)
			bufIdx = 0
			if cr.loaded == 0 {
				return "", offset, false
			}
		}

		remaining := cr.buf[bufIdx:cr.loaded]
		nlIdx := bytes.IndexByte(remaining, '\n')
		if nlIdx >= 0 {
			line = append(line, remaining[:nlIdx]...)
			newPos := pos + int64(nlIdx) + 1
			s := strings.TrimSpace(string(line))
			if s == "" {
				line = line[:0]
				pos = newPos
				continue
			}
			return s, newPos, true
		}

		line = append(line, remaining...)
		pos += int64(len(remaining))
	}
}

// forward POSTs a JSON line to the target hookd endpoint.
func forward(ctx context.Context, client *http.Client, target, line string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewBufferString(line))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST returned %d", resp.StatusCode)
	}
	return nil
}

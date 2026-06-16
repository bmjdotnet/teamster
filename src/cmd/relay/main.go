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
	reader := &chunkReader{f: f}
	var forwarded, errCount int64
	var consecFails int

	for {
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
type chunkReader struct {
	f      *os.File
	buf    [chunkSize]byte
	start  int64 // file offset where buf[0] was read from
	loaded int   // valid bytes in buf
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

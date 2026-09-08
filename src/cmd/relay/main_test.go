package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestChunkReader_SurvivesCopytruncate exercises exactly the sequence
// WP11's own AC3 specifies for the tailer it built (cp f f.1 && : > f),
// applied here to the relay: a line written and forwarded before the
// truncate, then the file is shrunk out from under the reader in place
// (copytruncate — what the installed logrotate stanza actually does), then
// new lines are appended. Without checkRotation, readLine would return
// ok=false forever once the file shrinks below the current offset.
func TestChunkReader_SurvivesCopytruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("line-one\nline-two\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	cr := newChunkReader(f, path)

	line, offset, ok := cr.readLine(0)
	if !ok || line != "line-one" {
		t.Fatalf("first readLine = (%q, %v), want (line-one, true)", line, ok)
	}
	line, offset, ok = cr.readLine(offset)
	if !ok || line != "line-two" {
		t.Fatalf("second readLine = (%q, %v), want (line-two, true)", line, ok)
	}

	// Simulate copytruncate: copy aside, then truncate in place, then
	// append fresh content — exactly WP11 AC3's sequence.
	if err := os.WriteFile(path+".1", []byte("line-one\nline-two\n"), 0o644); err != nil {
		t.Fatalf("copy aside: %v", err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := os.WriteFile(path, []byte("line-three\n"), 0o644); err != nil {
		t.Fatalf("write post-truncate: %v", err)
	}

	newOffset, rotated := cr.checkRotation(offset)
	if !rotated {
		t.Fatal("checkRotation did not detect the truncation")
	}
	if newOffset != 0 {
		t.Errorf("newOffset = %d, want 0", newOffset)
	}

	line, _, ok = cr.readLine(newOffset)
	if !ok || line != "line-three" {
		t.Fatalf("post-truncate readLine = (%q, %v), want (line-three, true) — line written after the truncation must be delivered", line, ok)
	}
}

// TestChunkReader_SurvivesRenameRotation covers the other rotation shape:
// the path is replaced with a brand-new inode (rename-then-create, as
// opposed to copytruncate's in-place shrink). checkRotation must detect the
// inode change via the path (not the already-open, now-stale fd) and swap
// to a freshly opened file.
func TestChunkReader_SurvivesRenameRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte("old-line\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	cr := newChunkReader(f, path)

	line, offset, ok := cr.readLine(0)
	if !ok || line != "old-line" {
		t.Fatalf("first readLine = (%q, %v), want (old-line, true)", line, ok)
	}

	// Rename the old file away and create a brand-new one at the same path
	// — a different inode, not a truncation of the one currently open.
	if err := os.Rename(path, filepath.Join(dir, "events.jsonl.1")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.WriteFile(path, []byte("new-line\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	newOffset, rotated := cr.checkRotation(offset)
	if !rotated {
		t.Fatal("checkRotation did not detect the inode change")
	}
	if newOffset != 0 {
		t.Errorf("newOffset = %d, want 0", newOffset)
	}

	line, _, ok = cr.readLine(newOffset)
	if !ok || line != "new-line" {
		t.Fatalf("post-rotation readLine = (%q, %v), want (new-line, true)", line, ok)
	}
}

// TestChunkReader_NoRotationIsNoOp is the control: an unchanged, growing
// file must never trigger a reset — checkRotation should be a no-op on
// every ordinary pass, which is most passes.
func TestChunkReader_NoRotationIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("line-one\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	cr := newChunkReader(f, path)
	_, offset, ok := cr.readLine(0)
	if !ok {
		t.Fatal("expected first line to read")
	}

	// Append (grow, don't shrink or replace) — ordinary operation.
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("reopen for append: %v", err)
	}
	if _, err := fh.WriteString("line-two\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	fh.Close()

	newOffset, rotated := cr.checkRotation(offset)
	if rotated {
		t.Fatalf("checkRotation fired on an ordinary append, offset %d -> %d", offset, newOffset)
	}
	if newOffset != offset {
		t.Errorf("newOffset = %d, want unchanged %d", newOffset, offset)
	}
}

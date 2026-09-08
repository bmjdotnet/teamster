package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTailFollower_SurvivesCopytruncate exercises WP11's own AC3 sequence
// (cp f f.1 && : > f), applied to feed's follow loop: a line delivered
// before the truncate, the file shrunk in place, then new lines appended.
// Without checkRotation, readNextLine's ReadAt past the new, smaller EOF
// returns n==0 forever and both startTail and runPureTail go silent.
func TestTailFollower_SurvivesCopytruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("line-one\nline-two\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	tf := newTailFollower(f, path)
	var offset int64

	line, err := tf.next(&offset)
	if err != nil || line != "line-one" {
		t.Fatalf("first next() = (%q, %v), want (line-one, nil)", line, err)
	}
	line, err = tf.next(&offset)
	if err != nil || line != "line-two" {
		t.Fatalf("second next() = (%q, %v), want (line-two, nil)", line, err)
	}

	if err := os.WriteFile(path+".1", []byte("line-one\nline-two\n"), 0o644); err != nil {
		t.Fatalf("copy aside: %v", err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := os.WriteFile(path, []byte("line-three\n"), 0o644); err != nil {
		t.Fatalf("write post-truncate: %v", err)
	}

	line, err = tf.next(&offset)
	if err != nil || line != "line-three" {
		t.Fatalf("post-truncate next() = (%q, %v), want (line-three, nil) — a line written after the truncation must still be delivered", line, err)
	}
	if offset != int64(len("line-three\n")) {
		t.Errorf("offset after post-truncate read = %d, want %d", offset, len("line-three\n"))
	}
}

// TestTailFollower_SurvivesRenameRotation covers the replacement shape: the
// path gets a brand-new inode (rename-then-create) rather than an in-place
// shrink. checkRotation must detect it via the path, not the stale open fd.
func TestTailFollower_SurvivesRenameRotation(t *testing.T) {
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

	tf := newTailFollower(f, path)
	var offset int64
	line, err := tf.next(&offset)
	if err != nil || line != "old-line" {
		t.Fatalf("first next() = (%q, %v), want (old-line, nil)", line, err)
	}

	if err := os.Rename(path, filepath.Join(dir, "events.jsonl.1")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.WriteFile(path, []byte("new-line\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	line, err = tf.next(&offset)
	if err != nil || line != "new-line" {
		t.Fatalf("post-rotation next() = (%q, %v), want (new-line, nil)", line, err)
	}
}

// TestTailFollower_NoRotationIsNoOp is the control: an ordinary append must
// never trigger a reset.
func TestTailFollower_NoRotationIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("line-one\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	tf := newTailFollower(f, path)
	var offset int64
	if _, err := tf.next(&offset); err != nil {
		t.Fatalf("expected first line to read, got error: %v", err)
	}
	afterFirst := offset

	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("reopen for append: %v", err)
	}
	if _, err := fh.WriteString("line-two\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	fh.Close()

	newOffset, rotated := tf.checkRotation(offset)
	if rotated {
		t.Fatalf("checkRotation fired on an ordinary append, offset %d -> %d", offset, newOffset)
	}
	if newOffset != afterFirst {
		t.Errorf("checkRotation changed offset on a no-op check: %d -> %d", afterFirst, newOffset)
	}
}

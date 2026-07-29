package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// readFile returns a file's contents as a string, or "" if it doesn't exist.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func TestRotatingWriterNoRotationBelowCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tracker.log")
	w, err := newRotatingWriter(path, 100)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readFile(t, path); got != "hello" {
		t.Errorf("log = %q, want %q", got, "hello")
	}
	if got := readFile(t, path+".old"); got != "" {
		t.Errorf(".old = %q, want no file (nothing exceeded the cap)", got)
	}
}

func TestRotatingWriterRotatesWhenExceeding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tracker.log")
	w, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	// size 0 -> +5 = 5, not > 10: no rotation.
	w.Write([]byte("aaaaa"))
	// size 5 -> +7 = 12 > 10: rotate first, then write to a fresh file.
	w.Write([]byte("bbbbbbb"))

	if got := readFile(t, path+".old"); got != "aaaaa" {
		t.Errorf(".old = %q, want %q (the pre-rotation contents)", got, "aaaaa")
	}
	if got := readFile(t, path); got != "bbbbbbb" {
		t.Errorf("log = %q, want %q (the post-rotation write)", got, "bbbbbbb")
	}
}

func TestRotatingWriterSecondRotationOverwritesOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tracker.log")
	w, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	w.Write([]byte("aaaaa"))   // size 5
	w.Write([]byte("bbbbb"))   // 5+5=10, not >10: size 10
	w.Write([]byte("ccccc"))   // 10+5=15 >10: rotate -> .old="aaaaabbbbb", fresh, size 5
	w.Write([]byte("ddddddd")) // 5+7=12 >10: rotate -> .old="ccccc" (overwrites), fresh

	if got := readFile(t, path+".old"); got != "ccccc" {
		t.Errorf(".old = %q, want %q (only the most recent rollover survives)", got, "ccccc")
	}
	if got := readFile(t, path); got != "ddddddd" {
		t.Errorf("log = %q, want %q", got, "ddddddd")
	}
}

// A rename failure (e.g. a transient lock on Windows) must not permanently
// kill logging — the writer reopens the original file and keeps writing rather
// than leaving a closed handle. Forced here by making the directory read-only
// so the .old rename can't happen while the existing file stays writable.
func TestRotatingWriterSurvivesRotateFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory-permission trick doesn't force a rename failure as root or on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tracker.log")
	w, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	defer w.Close()

	w.Write([]byte("aaaaaaaa")) // size 8

	if err := os.Chmod(dir, 0o500); err != nil { // r-x: rename blocked, existing file still writable
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700)

	// 8+5=13 >10 triggers a rotate whose rename fails; the write must still land.
	if _, err := w.Write([]byte("bbbbb")); err != nil {
		t.Fatalf("Write after failed rotate returned error: %v", err)
	}
	if _, err := w.Write([]byte("ccccc")); err != nil {
		t.Fatalf("second Write after failed rotate returned error: %v", err)
	}

	os.Chmod(dir, 0o700)
	if got := readFile(t, path); !strings.Contains(got, "bbbbb") || !strings.Contains(got, "ccccc") {
		t.Errorf("log = %q, want it to still contain the writes made during the failed rotate", got)
	}
	if got := readFile(t, path+".old"); got != "" {
		t.Errorf(".old = %q, want no file (the rename never succeeded)", got)
	}
}

func TestRotatingWriterResumesSizeAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tracker.log")

	w1, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatalf("newRotatingWriter: %v", err)
	}
	w1.Write([]byte("aaaaaaaa")) // size 8
	w1.Close()

	// Reopen: size must seed from the 8-byte existing file, so the next small
	// write rotates. If size reset to 0, 0+3 would not exceed 10 and nothing
	// would rotate — this is what proves the seed.
	w2, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatalf("newRotatingWriter reopen: %v", err)
	}
	defer w2.Close()
	w2.Write([]byte("bbb")) // 8+3=11 >10: rotate

	if got := readFile(t, path+".old"); got != "aaaaaaaa" {
		t.Errorf(".old = %q, want %q (size seeded from existing file across reopen)", got, "aaaaaaaa")
	}
	if got := readFile(t, path); got != "bbb" {
		t.Errorf("log = %q, want %q", got, "bbb")
	}
}

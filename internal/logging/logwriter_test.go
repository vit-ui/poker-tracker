package logging

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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

	w.Write([]byte("aaaaa"))
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

	w.Write([]byte("aaaaa"))
	w.Write([]byte("bbbbb"))
	w.Write([]byte("ccccc"))
	w.Write([]byte("ddddddd"))

	if got := readFile(t, path+".old"); got != "ccccc" {
		t.Errorf(".old = %q, want %q (only the most recent rollover survives)", got, "ccccc")
	}
	if got := readFile(t, path); got != "ddddddd" {
		t.Errorf("log = %q, want %q", got, "ddddddd")
	}
}

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

	w.Write([]byte("aaaaaaaa"))

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700)

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
	w1.Write([]byte("aaaaaaaa"))
	w1.Close()

	w2, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatalf("newRotatingWriter reopen: %v", err)
	}
	defer w2.Close()
	w2.Write([]byte("bbb"))

	if got := readFile(t, path+".old"); got != "aaaaaaaa" {
		t.Errorf(".old = %q, want %q (size seeded from existing file across reopen)", got, "aaaaaaaa")
	}
	if got := readFile(t, path); got != "bbb" {
		t.Errorf("log = %q, want %q", got, "bbb")
	}
}

package client

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLockfileActiveAt covers the predicate the enforcement loop polls: a lock
// is active only if the file exists and its expiry is still in the future.
// Absent, expired, and malformed all read as inactive.
func TestLockfileActiveAt(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	writeLockfile := func(t *testing.T, contents string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "lockfile.json")
		if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
			t.Fatalf("writing lockfile: %v", err)
		}
		return path
	}

	t.Run("absent file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lockfile.json")
		if got := lockfileActiveAt(path, now); got != false {
			t.Errorf("lockfileActiveAt(absent) = %v, want false", got)
		}
	})

	t.Run("expires in the future", func(t *testing.T) {
		path := writeLockfile(t, `{
			"triggered_at": "2026-07-13T11:00:00Z",
			"expires_at": "2026-07-13T13:00:00Z",
			"reason": "manual",
			"duration_hours": 2
		}`)
		if got := lockfileActiveAt(path, now); got != true {
			t.Errorf("lockfileActiveAt(future expiry) = %v, want true", got)
		}
	})

	t.Run("expires in the past", func(t *testing.T) {
		path := writeLockfile(t, `{
			"triggered_at": "2026-07-13T09:00:00Z",
			"expires_at": "2026-07-13T11:00:00Z",
			"reason": "manual",
			"duration_hours": 2
		}`)
		if got := lockfileActiveAt(path, now); got != false {
			t.Errorf("lockfileActiveAt(past expiry) = %v, want false", got)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		path := writeLockfile(t, `{not valid json`)
		if got := lockfileActiveAt(path, now); got != false {
			t.Errorf("lockfileActiveAt(malformed) = %v, want false", got)
		}
	})
}

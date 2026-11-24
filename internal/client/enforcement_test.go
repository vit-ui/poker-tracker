package client

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func TestHardstopCheckIntervals(t *testing.T) {
	for _, test := range []struct {
		name   string
		active bool
		want   time.Duration
	}{
		{"active", true, time.Second},
		{"idle", false, 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := lockCheckInterval(test.active); got != test.want {
				t.Fatalf("interval=%v, want %v", got, test.want)
			}
		})
	}
}

func TestCheckLock(t *testing.T) {
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, test := range []struct {
		name, contents       string
		status               int
		stdout, stderrPrefix string
		fileRemains          bool
	}{
		{name: "missing"},
		{name: "expired", contents: `{"expires_at":"2001-01-01T12:00:00Z"}`},
		{name: "active", contents: fmt.Sprintf(`{"expires_at":%q}`, expiry.Format(time.RFC3339)), status: 1, stdout: "PokerStars is locked until " + expiry.Local().Format("2006-01-02 15:04:05") + ". Check Telegram for details.\n", fileRemains: true},
		{name: "corrupt", contents: "{", status: 1, stderrPrefix: "reading lockfile:", fileRemains: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lockfile.json")
			if test.contents != "" {
				if err := os.WriteFile(path, []byte(test.contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr strings.Builder
			if got := CheckLock(path, &stdout, &stderr); got != test.status {
				t.Errorf("exit status = %d, want %d", got, test.status)
			}
			if stdout.String() != test.stdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), test.stdout)
			}
			if test.stderrPrefix == "" && stderr.Len() > 0 {
				t.Errorf("unexpected stderr: %s", stderr.String())
			}
			if !strings.HasPrefix(stderr.String(), test.stderrPrefix) {
				t.Errorf("stderr = %q, want prefix %q", stderr.String(), test.stderrPrefix)
			}
			_, err := os.Stat(path)
			if (test.fileRemains && err != nil) || (!test.fileRemains && !os.IsNotExist(err)) {
				t.Fatalf("lockfile stat: %v", err)
			}
		})
	}
}

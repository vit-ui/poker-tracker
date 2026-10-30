// Package client is the collector half of the tracker: it watches the
// PokerStars folders, sends file changes up to the server, and enforces the
// hard-stop locks the server sends back down. Enforcement lives here, not with
// the hard-stop decision (internal/hardstop), because only the machine
// PokerStars runs on can kill it and gate its launch.
package client

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

// Enforcer shares durable lock state and a fail-closed in-memory lock between
// command handling and the process-owned enforcement loop.
type Enforcer struct {
	path    string
	mu      sync.Mutex
	active  *config.LockFile
	unsaved *config.LockFile
	changed chan struct{}
}

func NewEnforcer(path string) *Enforcer {
	return &Enforcer{path: path, changed: make(chan struct{}, 1)}
}

func (e *Enforcer) Enforce(proposed config.LockFile) (*config.LockFile, bool, error) {
	e.mu.Lock()
	now := time.Now()
	var err error
	if e.unsaved != nil {
		_, _, err = config.ApplyHardstop(e.path, *e.unsaved, now)
		if err == nil {
			e.unsaved = nil
		}
	}
	var active *config.LockFile
	var fresh bool
	if err == nil {
		active, fresh, err = config.ApplyHardstop(e.path, proposed, now)
	}
	if err != nil {
		if e.active == nil || proposed.ExpiresAt.After(e.active.ExpiresAt) {
			copy := proposed
			e.active = &copy
		}
		e.unsaved = e.active
		active = e.active
	} else {
		e.active = active
		e.unsaved = nil
	}
	e.mu.Unlock()
	select {
	case e.changed <- struct{}{}:
	default:
	}
	if active != nil && active.ExpiresAt.After(now) {
		err = errors.Join(err, killPokerStars())
	}
	if err != nil {
		slog.Error("applying hard-stop protection", "err", err)
	} else if fresh && active != nil {
		slog.Info("hard stop enforced", "reason", proposed.Reason, "expires_at", active.ExpiresAt)
	}
	return active, fresh, err
}

// Enforce retains the one-shot API for native probes. Production shares an
// Enforcer with its collector's enforcement loop.
func Enforce(path string, lock config.LockFile) { NewEnforcer(path).Enforce(lock) }

func (e *Enforcer) EnforceActive() bool {
	e.mu.Lock()
	now := time.Now()
	if e.unsaved != nil {
		if _, _, err := config.ApplyHardstop(e.path, *e.unsaved, now); err == nil {
			e.unsaved = nil
		} else {
			slog.Error("persisting retained hard stop", "err", err)
		}
	}
	saved, err := config.LoadLock(e.path)
	if saved != nil && (e.active == nil || saved.ExpiresAt.After(e.active.ExpiresAt)) {
		e.active = saved
	}
	active := e.active != nil && e.active.ExpiresAt.After(now)
	e.mu.Unlock()
	if err != nil {
		slog.Error("reading hard-stop protection; keeping enforcement active", "err", err)
	}
	if active || err != nil {
		if err := killPokerStars(); err != nil {
			slog.Error("enforcing active hard stop", "err", err)
		}
	}
	return active || err != nil
}

func lockCheckInterval(active bool) time.Duration {
	if active {
		return time.Second
	}
	return 30 * time.Second
}

// Run enforces immediately, checks active protection every second, and checks
// idle state every 30 seconds. New commands wake an idle timer immediately.
// Persistence errors retain the original expiry instead of resetting its clock.
func (e *Enforcer) Run(done <-chan struct{}) {
	timer := time.NewTimer(lockCheckInterval(e.EnforceActive()))
	defer timer.Stop()
	for {
		select {
		case <-done:
			return
		case <-e.changed:
			timer.Reset(lockCheckInterval(e.EnforceActive()))
		case <-timer.C:
			timer.Reset(lockCheckInterval(e.EnforceActive()))
		}
	}
}

func RunEnforcementLoop(path string, done <-chan struct{}) { NewEnforcer(path).Run(done) }

// lockfileActiveAt reports whether the lockfile at lockfilePath exists and
// hasn't expired as of now. Goes through config.LoadLock rather than reading
// the file directly — config.go documents that no other package touches
// config.json/lockfile.json directly. Never deletes the file even when
// expired; that stays --check-lock's responsibility.
func lockfileActiveAt(lockfilePath string, now time.Time) bool {
	lock, err := config.LoadLock(lockfilePath)
	if err != nil || lock == nil {
		return false
	}
	return now.Before(lock.ExpiresAt)
}

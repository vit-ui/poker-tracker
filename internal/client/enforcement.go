package client

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

type Enforcer struct {
	path            string
	mu              sync.Mutex
	active          *config.LockFile
	unsaved         *config.LockFile
	changed         chan struct{}
	closuresReady   chan struct{}
	unsavedClosures []config.ClosureEvent
}

func NewEnforcer(path string) *Enforcer {
	return &Enforcer{path: path, changed: make(chan struct{}, 1), closuresReady: make(chan struct{}, 1)}
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
	if active != nil && active.ExpiresAt.After(now) {
		closed, killErr := killPokerStars()
		err = errors.Join(err, e.recordClosure(active, closed, killErr, time.Now()))
	}
	e.persistClosures()
	e.mu.Unlock()
	select {
	case e.changed <- struct{}{}:
	default:
	}
	if err != nil {
		slog.Error("applying hard-stop protection", "err", err)
	} else if fresh && active != nil {
		slog.Info("hard stop enforced", "reason", proposed.Reason, "expires_at", active.ExpiresAt)
	}
	return active, fresh, err
}

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
	if err != nil {
		slog.Error("reading hard-stop protection; keeping enforcement active", "err", err)
	}
	if active || err != nil {
		closed, killErr := killPokerStars()
		if err := e.recordClosure(e.active, closed, killErr, time.Now()); err != nil {
			slog.Error("enforcing active hard stop", "err", err)
		}
	}
	e.persistClosures()
	e.mu.Unlock()
	return active || err != nil
}

func lockCheckInterval(active bool) time.Duration {
	if active {
		return time.Second
	}
	return 30 * time.Second
}

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

func lockfileActiveAt(lockfilePath string, now time.Time) bool {
	lock, err := config.LoadLock(lockfilePath)
	if err != nil || lock == nil {
		return false
	}
	return now.Before(lock.ExpiresAt)
}

func (e *Enforcer) closuresPath() string {
	return filepath.Join(filepath.Dir(e.path), "collector_closures.json")
}

func (e *Enforcer) recordClosure(lock *config.LockFile, closed bool, err error, now time.Time) error {
	if err != nil || !closed || lock == nil || !lock.ExpiresAt.After(now) {
		return err
	}
	e.unsavedClosures = append(e.unsavedClosures, config.ClosureEvent{ID: "closure:" + rand.Text(), ClosedAt: now, Lock: *lock})
	return nil
}

func (e *Enforcer) persistClosures() {
	if len(e.unsavedClosures) > 0 {
		if err := config.QueueClosures(e.closuresPath(), e.unsavedClosures); err != nil {
			slog.Error("persisting PokerStars closure events; enforcement remains active", "err", err)
		} else {
			e.unsavedClosures = nil
			select {
			case e.closuresReady <- struct{}{}:
			default:
			}
		}
	}
}

func (e *Enforcer) acknowledgeClosure(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return config.AcknowledgeClosure(e.closuresPath(), id)
}

func (e *Enforcer) checkClosureTransferReady() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.unsavedClosures) > 0 {
		return fmt.Errorf("PokerStars closure events have not been saved")
	}
	return config.CheckClosureTransferReady(filepath.Dir(e.path))
}

func CheckLock(lockfilePath string, stdout, stderr io.Writer) int {
	lock, err := config.LoadLock(lockfilePath)
	if err != nil {
		fmt.Fprintf(stderr, "reading lockfile: %v\n", err)
		return 1
	}
	if lock == nil {
		return 0
	}
	if config.IsLockExpired(lock) {
		if err := config.DeleteLock(lockfilePath); err != nil {
			fmt.Fprintf(stderr, "deleting lockfile: %v\n", err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "PokerStars is locked until %s. Check Telegram for details.\n",
		lock.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
	return 1
}

// Package client is the collector half of the tracker: it watches the
// PokerStars folders, sends file changes up to the server, and enforces the
// hard-stop locks the server sends back down. Enforcement lives here, not with
// the hard-stop decision (internal/hardstop), because only the machine
// PokerStars runs on can kill it and gate its launch.
package client

import (
	"log/slog"
	"os/exec"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

// Enforce carries out a hard-stop lock the server decided: it kills PokerStars
// and writes the lockfile. The taskkill target is PokerStars.exe (not
// PokerStarsUpdate.exe). A failed lockfile write is logged and otherwise
// ignored — the enforcement loop re-kills on its own schedule regardless.
func Enforce(lockfilePath string, lock config.LockFile) {
	_ = exec.Command("taskkill.exe", "/IM", "PokerStars.exe", "/F").Run()
	if err := config.SaveLock(lockfilePath, &lock); err != nil {
		slog.Error("saving lockfile", "error", err)
	}
	slog.Info("hard stop enforced", "reason", lock.Reason, "expires_at", lock.ExpiresAt.Format(time.RFC3339))
}

// RunEnforcementLoop re-kills PokerStars every 30 seconds for as long as the
// lockfile at lockfilePath is active, so a player who relaunches PokerStars
// mid-lockout gets killed again rather than slipping through until the next
// file event triggers a check. Runs for the life of the collector; stops when
// done is closed. Because it checks lockfile state on every tick from the
// moment it starts, restarting the process (e.g. Task Scheduler after a reboot)
// automatically resumes enforcement if a lockfile written before the restart is
// still active.
func RunEnforcementLoop(lockfilePath string, done <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !lockfileActiveAt(lockfilePath, time.Now()) {
				continue
			}
			lock, err := config.LoadLock(lockfilePath)
			if err != nil || lock == nil {
				continue
			}
			slog.Info("hard stop lock active, killing PokerStars", "expires_at", lock.ExpiresAt)
			_ = exec.Command("taskkill.exe", "/IM", "PokerStars.exe", "/F").Run()
		}
	}
}

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

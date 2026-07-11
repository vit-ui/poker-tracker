// Package hardstop implements the hard stop trigger: detecting loss conditions,
// killing PokerStars, and writing/reading the lockfile.
package hardstop

import (
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
)

// Evaluate checks hard stop conditions after a cash session and triggers a
// hard stop if any condition is met.
func Evaluate(session parsers.CashSession, sheetsClient *sheets.Client, configPath, lockfilePath string, alertChan chan<- string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	h := cfg.Hardstop
	if h.SessionLossBuyins == nil && h.ConsecutiveLosses == nil && h.SpingoDailyLossUSD == nil && h.DurationHours == nil {
		return nil
	}

	if h.SessionLossBuyins != nil && session.NetUSD < 0 &&
		math.Abs(session.NetUSD) > (*h.SessionLossBuyins*session.BuyInUSD) {
		if h.DurationHours == nil {
			return fmt.Errorf("session_loss_buyins configured without duration_hours")
		}
		trigger("session_loss_buyins", *h.DurationHours, lockfilePath, alertChan)
		return nil
	}

	if h.ConsecutiveLosses != nil {
		rows, err := sheetsClient.GetRecentLedgerRows(*h.ConsecutiveLosses)
		if err != nil {
			return err
		}
		allLosses := len(rows) == *h.ConsecutiveLosses
		for _, row := range rows {
			if row.AmountUSD >= 0 || (row.Type != "cash_session" && row.Type != "tournament") {
				allLosses = false
				break
			}
		}
		if allLosses {
			if h.DurationHours == nil {
				return fmt.Errorf("consecutive_losses configured without duration_hours")
			}
			trigger("consecutive_losses", *h.DurationHours, lockfilePath, alertChan)
			return nil
		}
	}

	return nil
}

// EvaluateTournament checks hard stop conditions after a tournament. Filters
// by Type == "spingo" internally when checking the daily loss threshold.
func EvaluateTournament(t parsers.Tournament, sheetsClient *sheets.Client, configPath, lockfilePath string, alertChan chan<- string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	h := cfg.Hardstop
	if h.SpingoDailyLossUSD == nil {
		return nil
	}
	if t.Type != "spingo" {
		return nil
	}

	now := time.Now().UTC()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	tournaments, err := sheetsClient.GetTournamentsSince(startOfToday)
	if err != nil {
		return err
	}

	var netToday float64
	for _, tourn := range tournaments {
		if tourn.Type == "spingo" {
			netToday += tourn.NetUSD
		}
	}

	if netToday < -*h.SpingoDailyLossUSD {
		if h.DurationHours == nil {
			return fmt.Errorf("spingo_daily_loss_usd configured without duration_hours")
		}
		trigger("spingo_daily_loss_usd", *h.DurationHours, lockfilePath, alertChan)
	}
	return nil
}

// ManualTrigger activates a hard stop on demand, for /hardstop trigger.
func ManualTrigger(configPath, lockfilePath string, alertChan chan<- string) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if cfg.Hardstop.DurationHours == nil {
		return fmt.Errorf("duration_hours is not configured, run /hardstop first")
	}
	trigger("manual", *cfg.Hardstop.DurationHours, lockfilePath, alertChan)
	return nil
}

// trigger kills PokerStars, writes the lockfile, and sends the hard stop
// alert.
func trigger(reason string, durationHours int, lockfilePath string, alertChan chan<- string) {
	if err := exec.Command("taskkill.exe", "/IM", "PokerStars.exe", "/F").Run(); err != nil {
		slog.Info("taskkill PokerStars.exe", "error", err)
	} else {
		slog.Info("taskkill PokerStars.exe succeeded")
	}

	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(durationHours) * time.Hour)
	lock := &config.LockFile{
		TriggeredAt:   now,
		ExpiresAt:     expiresAt,
		Reason:        reason,
		DurationHours: durationHours,
	}
	if err := config.SaveLock(lockfilePath, lock); err != nil {
		slog.Error("saving lockfile", "error", err)
	}

	alertChan <- fmt.Sprintf("🛑 Hard Stop triggered\nReason: %s\nExpires: %s", reason, expiresAt.Local().Format("2006-01-02 15:04"))
}

package hardstop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

// AutomaticTriggerID fingerprints only the financial inputs to the rule that
// fired. It excludes bankroll totals, transfers, replay time and display stats.
// Stored dates use minute digits so backend migration cannot change identity.
func AutomaticTriggerID(reason string, duration int, cfg *config.Config, now time.Time, cash *parsers.CashSession, tournament *parsers.Tournament, ledger []storage.LedgerEntry, dayTournaments []parsers.Tournament) (string, error) {
	type result struct {
		ID, Date    string
		AmountCents money.Cents
	}
	payload := struct {
		Reason     string
		Duration   int
		Threshold  string
		Source     string
		BuyInCents money.Cents
		NetCents   money.Cents
		Hands      int
		Day        string
		Results    []result
	}{Reason: reason, Duration: duration}
	var relevant []storage.LedgerEntry
	switch reason {
	case "session_loss_buyins":
		payload.Threshold = strconv.FormatFloat(*cfg.Hardstop.SessionLossBuyins, 'g', -1, 64)
		payload.Source = cash.FirstHandID
		payload.BuyInCents = cash.BuyInCents
		payload.NetCents = cash.NetCents
		payload.Hands = cash.Hands
	case "consecutive_losses":
		payload.Threshold = strconv.Itoa(*cfg.Hardstop.ConsecutiveLosses)
		relevant = recentSessionRows(ledger, *cfg.Hardstop.ConsecutiveLosses)
	case "downswing_buyins":
		payload.Threshold = strconv.FormatFloat(*cfg.Hardstop.DownswingBuyins, 'g', -1, 64)
		if cash != nil {
			payload.BuyInCents = cash.BuyInCents
		} else {
			payload.BuyInCents = tournament.BuyInCents
		}
		cutoff := rollingWindowStart(now, 30*24*time.Hour)
		for _, entry := range sessionOnly(ledger) {
			if inRollingWindow(entry.Date, cutoff) {
				relevant = append(relevant, entry)
			}
		}
	case "daily_loss_cents":
		payload.Threshold = strconv.FormatInt(int64(*cfg.Hardstop.DailyLossCents), 10)
		payload.Day = PokerDayStart(cfg, now).Format("2006-01-02 15:04")
		relevant = PokerDaySessionEntries(ledger, cfg, now)
	case "spingo_daily_loss_cents":
		payload.Threshold = strconv.FormatInt(int64(*cfg.Hardstop.SpingoDailyLossCents), 10)
		payload.Day = PokerDayStart(cfg, now).Format("2006-01-02 15:04")
		for _, entry := range SpingoOnly(dayTournaments) {
			payload.Results = append(payload.Results, result{ID: entry.TournamentID, Date: entry.Date.Format("2006-01-02 15:04"), AmountCents: entry.NetCents})
		}
	default:
		return "", fmt.Errorf("unknown automatic hard-stop reason %q", reason)
	}
	for _, entry := range relevant {
		payload.Results = append(payload.Results, result{ID: entry.Type + ":" + entry.SessionID, Date: entry.Date.Format("2006-01-02 15:04"), AmountCents: entry.AmountCents})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding hard-stop trigger identity: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "hardstop:" + hex.EncodeToString(sum[:]), nil
}

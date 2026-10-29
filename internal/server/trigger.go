package server

import (
	"crypto/rand"
	"log/slog"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/hardstop"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func automaticEnforcement(cfg *config.Config, now time.Time, cash *parsers.CashSession, tournament *parsers.Tournament, ledger []storage.LedgerEntry, day []parsers.Tournament, deliver func(string, int, string)) func(string, int) {
	return func(reason string, duration int) {
		id, err := hardstop.AutomaticTriggerID(reason, duration, cfg, now, cash, tournament, ledger, day)
		if err != nil {
			// An identity error must never suppress a genuine safety decision.
			slog.Error("identifying hard-stop decision", "err", err)
			id = "hardstop:unidentified:" + rand.Text()
		}
		deliver(reason, duration, id)
	}
}

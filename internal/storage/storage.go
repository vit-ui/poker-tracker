// Package storage defines the persistence contract shared by every backend
// (Google Sheets, SQLite) and the types that cross that boundary.
package storage

import (
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

// LedgerEntry represents a row in the Bankroll Ledger.
type LedgerEntry struct {
	Date         time.Time
	Type         string // "deposit" | "withdrawal" | "cash_session" | "tournament"
	AmountUSD    float64
	RunningTotal float64
}

// Store is the persistence contract both backends implement. Its method set
// is exactly the operations the worker, bot, and hardstop code call.
type Store interface {
	AppendCashSession(session parsers.CashSession) error
	AppendTournament(t parsers.Tournament) error
	UpdateTournamentVPIPPFR(tournamentID string, vpip, pfr float64) error
	AppendLedgerEntry(entryType string, amountUSD float64) error
	GetCurrentBankroll() (float64, error)
	GetSheetURL() string
	GetRecentLedgerRows(n int) ([]LedgerEntry, error)
	GetLedgerEntriesSince(t time.Time) ([]LedgerEntry, error)
	GetCashSessionsSince(t time.Time) ([]parsers.CashSession, error)
	GetTournamentsSince(since time.Time) ([]parsers.Tournament, error)
}

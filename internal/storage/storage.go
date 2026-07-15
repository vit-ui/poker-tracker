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
	SessionID    string // blank for deposit/withdrawal rows
}

// CashSessionRecord is a full Cash Sessions row, including the Session ID
// upsert key — parsers.CashSession itself carries no notion of storage
// identity, since the parser produces session data without knowing how (or
// whether) it's persisted. Used only by /export, which needs every column.
type CashSessionRecord struct {
	parsers.CashSession
	SessionID string
}

// TournamentRecord is a full Tournaments row, including the two
// storage-computed columns (Bounty Won, Prize Won) that parsers.Tournament
// never carries — they're derived by the storage layer once a tournament's
// hand-file bounty data is folded into its summary-file baseline net, not by
// the parser itself. Used only by /export.
type TournamentRecord struct {
	parsers.Tournament
	BountyWonUSD float64
	PrizeWonUSD  float64
}

// TimeAnalysisRecord is a row in the Time Analysis tab/table.
type TimeAnalysisRecord struct {
	Date      time.Time
	Hour      int
	DayOfWeek string
	NetUSD    float64
	SessionID string
}

// Store is the persistence contract both backends implement. Its method set
// is exactly the operations the worker, bot, and hardstop code call.
type Store interface {
	AppendCashSession(session parsers.CashSession) error
	AppendTournament(t parsers.Tournament) (float64, error)
	UpdateTournamentHandStats(tournamentID string, vpip, pfr float64, hands int, bountyUSD float64, isKnockout bool) error
	AppendLedgerEntry(entryType string, amountUSD float64) error
	GetCurrentBankroll() (float64, error)
	GetSheetURL() string
	GetLedgerEntriesSince(t time.Time) ([]LedgerEntry, error)
	GetCashSessionsSince(t time.Time) ([]parsers.CashSession, error)
	GetTournamentsSince(since time.Time) ([]parsers.Tournament, error)
	GetAllCashSessions() ([]CashSessionRecord, error)
	GetAllTournaments() ([]TournamentRecord, error)
	GetAllTimeAnalysis() ([]TimeAnalysisRecord, error)
}

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

// Snapshot is every row of all four data sets, as full records, used only by
// the backend-migration path (bot's /storage). Unlike GetAllTournaments,
// its Tournaments slice includes partial rows (a hand file arrived before the
// summary — Date zero, no ledger impact yet), so a tournament still in progress
// across a backend switch keeps its VPIP/PFR/hands/bounty. A Snapshot is a raw
// copy: every value is carried verbatim (session IDs, dates, and money columns
// — including each ledger row's stored running total and each tournament's
// Prize Won), never re-derived, so ImportSnapshot reproduces the source rows
// byte-for-byte rather than replaying them through the Append* methods (which
// would re-fold bounties and recompute session IDs off a FirstHandID the read
// side never carries).
type Snapshot struct {
	CashSessions []CashSessionRecord
	Tournaments  []TournamentRecord
	Ledger       []LedgerEntry
	TimeAnalysis []TimeAnalysisRecord
}

// IsEmpty reports whether the snapshot holds no rows in any data set — the
// emptiness test a migration target must pass before it's imported into.
func (s Snapshot) IsEmpty() bool {
	return len(s.CashSessions) == 0 && len(s.Tournaments) == 0 &&
		len(s.Ledger) == 0 && len(s.TimeAnalysis) == 0
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

	// ExportSnapshot and ImportSnapshot move an install's whole history
	// between backends (bot's /storage). Both backends need both
	// directions — either can be the source or the empty target — which is
	// why they're on the interface rather than a concrete type.
	ExportSnapshot() (Snapshot, error)
	ImportSnapshot(Snapshot) error
}

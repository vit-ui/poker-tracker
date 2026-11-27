package storage

import (
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
)

type LedgerEntry struct {
	Date              time.Time
	Type              string
	AmountCents       money.Cents
	RunningTotalCents money.Cents
	SessionID         string
}

func (entry LedgerEntry) IsPokerResult() bool {
	return entry.Type == "cash_session" || entry.Type == "tournament"
}

type CashSessionRecord struct {
	parsers.CashSession
	SessionID   string
	HandDetails []parsers.CashHand `json:",omitempty"`
}

type TournamentRecord struct {
	parsers.Tournament
	BountyWonCents money.Cents
	PrizeWonCents  money.Cents
}

type Snapshot struct {
	CashSessions []CashSessionRecord
	Tournaments  []TournamentRecord
	Ledger       []LedgerEntry
}

func (s Snapshot) IsEmpty() bool {
	return len(s.CashSessions) == 0 && len(s.Tournaments) == 0 &&
		len(s.Ledger) == 0
}

type Store interface {
	AppendCashSession(session parsers.CashSession) error
	RecordCashHands(session parsers.CashSession, hands []parsers.CashHand, liveSource string) (CashWrite, error)
	AppendTournament(t parsers.Tournament) (money.Cents, error)
	UpdateTournamentHandStats(tournamentID string, vpip, pfr float64, hands int, bountyCents money.Cents, isKnockout bool) error
	ScanTournamentHandStats(parsers.TournamentHandStats) error
	AppendLedgerEntry(entryType string, amountCents money.Cents, now time.Time, operationID string) error
	GetCurrentBankroll() (money.Cents, error)
	GetSheetURL() string
	GetLedgerEntriesSince(t time.Time) ([]LedgerEntry, error)
	GetCashSessionsSince(t time.Time) ([]parsers.CashSession, error)
	GetTournamentsSince(since time.Time) ([]parsers.Tournament, error)

	GetTournamentByID(tournamentID string) (t parsers.Tournament, found bool, err error)
	GetAllCashSessions() ([]CashSessionRecord, error)
	GetAllTournaments() ([]TournamentRecord, error)

	ExportSnapshot() (Snapshot, error)
	ImportSnapshot(Snapshot) error
}

package storage

import (
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
)

type TournamentAnswer struct {
	TournamentID   string
	HandID         string
	Reason         string
	Ticket         bool
	TotalCostCents money.Cents
	PayoutCents    *money.Cents
}

type Operation struct {
	TournamentAnswer           *TournamentAnswer
	ScanPath                   string
	ScanAttachment             string
	Updates                    bool
	UpdateRequest              *config.UpdateRequest
	CollectorSettings          bool
	CollectorChange            *config.CollectorChange
	PrepareMigration           bool
	RequireConfirmedProtection bool
	PrepareRestart             bool
	FinalizeAllCash            bool
	FinalizeCash               bool
	RetryHardstops             bool
	Release                    <-chan struct{}
	Resumed                    chan error
	Apply                      func(Store) OperationResult
	Reply                      chan OperationResult
}

type OperationResult struct {
	Scan              ScanTally
	Updates           config.CollectorUpdates
	CollectorSettings config.CollectorSettingsState
	CollectorOnline   bool
	Recovered         int
	Snapshot          *Snapshot
	NetCents          money.Cents
	BankrollCents     money.Cents
	Found             bool
	Err               error
	BankrollErr       error
}

package storage

// Operation is a bot-originated storage or collector-settings request executed
// by the server worker.
// Reply is buffered once so completion never waits for a Telegram send.
// PrepareMigration first pauses/drains collection and finalizes pending cash.
// On success the worker stays held until restart, or Release is closed after
// rollback. Resumed must then be buffered once to receive the resume outcome.
// PrepareRestart drains collection without requiring PokerStars to close and
// preserves pending cash, unless FinalizeAllCash is set for a sheet change.
// RequireConfirmedProtection prevents deployment cutover from losing the sole
// replay owner of an unconfirmed hard-stop intent, including unsaved intents.

import (
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
)

type Operation struct {
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

// OperationResult is consumed on the bot event loop, where replies are sent.
type OperationResult struct {
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

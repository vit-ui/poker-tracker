package storage

// Operation is a bot-originated storage operation executed by the server worker.
// Reply is buffered once so completion never waits for a Telegram send.
// PrepareMigration first pauses/drains collection and finalizes pending cash.
// On success the worker stays held until restart, or Release is closed after
// rollback. Resumed must then be buffered once to receive the resume outcome.
type Operation struct {
	PrepareMigration bool
	Release          <-chan struct{}
	Resumed          chan error
	Apply            func(Store) OperationResult
	Reply            chan OperationResult
}

// OperationResult is consumed on the bot event loop, where replies are sent.
type OperationResult struct {
	Recovered   int
	Snapshot    *Snapshot
	NetUSD      float64
	Bankroll    float64
	Found       bool
	Err         error
	BankrollErr error
}

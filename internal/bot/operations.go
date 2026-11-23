package bot

import "github.com/vit-ui/self-tracker/internal/storage"

type pendingOperation struct {
	request  storage.Operation
	complete func(storage.OperationResult)
}

func (b *Bot) queueTournamentAnswer(answer storage.TournamentAnswer, complete func(storage.OperationResult)) {
	b.queueOperation(nil, complete)
	b.pendingOperations[len(b.pendingOperations)-1].request.TournamentAnswer = &answer
}

func (b *Bot) queueOperation(apply func(storage.Store) storage.OperationResult, complete func(storage.OperationResult)) {
	b.pendingOperations = append(b.pendingOperations, pendingOperation{
		request:  storage.Operation{Apply: apply, Reply: make(chan storage.OperationResult, 1)},
		complete: complete,
	})
}

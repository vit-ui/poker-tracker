package bot

import (
	"path/filepath"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

type pendingOperation struct {
	request  storage.Operation
	complete func(storage.OperationResult)
}

// recordPendingTournament runs on the server worker so the answer uses the
// latest queued summary and cannot overlap a file-driven correction.
func recordPendingTournament(store storage.Store, configPath, tournamentID string, resolve func(parsers.Tournament) parsers.Tournament) storage.OperationResult {
	path := filepath.Join(filepath.Dir(configPath), "pending_tournaments.json")
	pending, err := config.LoadPendingTournaments(path)
	if err != nil {
		return storage.OperationResult{Err: err}
	}
	for _, entry := range pending {
		if entry.Tournament.TournamentID != tournamentID {
			continue
		}
		netCents, err := store.AppendTournament(resolve(entry.Tournament))
		if err != nil {
			return storage.OperationResult{Found: true, Err: err}
		}
		err = config.UpdatePendingTournaments(path, func(current []config.PendingTournament) ([]config.PendingTournament, bool) {
			for i, candidate := range current {
				if candidate.Tournament.TournamentID == tournamentID {
					return append(current[:i], current[i+1:]...), true
				}
			}
			return current, false
		})
		return storage.OperationResult{Found: true, NetCents: netCents, Err: err}
	}
	return storage.OperationResult{}
}

func (b *Bot) queueOperation(apply func(storage.Store) storage.OperationResult, complete func(storage.OperationResult)) {
	b.pendingOperations = append(b.pendingOperations, pendingOperation{
		request:  storage.Operation{Apply: apply, Reply: make(chan storage.OperationResult, 1)},
		complete: complete,
	})
}

package server

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/sheets"
	"github.com/vit-ui/self-tracker/internal/storage"
)

type tournamentResults struct {
	server  *Server
	path    string
	timers  map[string]*quietTimer
	signals chan quietSignal
	done    <-chan struct{}
	fired   map[string]bool
	enforce func(string, int, string)
}

func (r *tournamentResults) restoreTimers() error {
	for id := range r.timers {
		stopQuietTimer(r.timers, id)
	}
	pending, err := config.LoadPendingTournaments(r.path)
	if err != nil {
		return err
	}
	for _, entry := range pending {
		r.arm(entry)
	}
	return nil
}

func (r *tournamentResults) arm(entry config.PendingTournament) {
	if entry.QuietUntil != nil {
		armQuietTimer(r.timers, entry.Tournament.TournamentID, time.Until(*entry.QuietUntil), r.signals, r.done)
	}
}

func (r *tournamentResults) update(id string, change func(*config.PendingTournament) bool) error {
	return config.UpdatePendingTournaments(r.path, func(pending []config.PendingTournament) ([]config.PendingTournament, bool) {
		i := slices.IndexFunc(pending, func(entry config.PendingTournament) bool { return entry.Tournament.TournamentID == id })
		if i >= 0 {
			before := pending[i]
			if !change(&pending[i]) {
				return slices.Delete(pending, i, i+1), true
			}
			return pending, pending[i] != before
		}
		var entry config.PendingTournament
		if change(&entry) {
			return append(pending, entry), true
		}
		return pending, false
	})
}

func (r *tournamentResults) removeRecovery(id string) error {
	stopQuietTimer(r.timers, id)
	if client, ok := r.server.store.(*sheets.Client); ok {
		if err := client.DiscardTournamentRecovery(id); err != nil {
			return err
		}
	}
	return r.update(id, func(entry *config.PendingTournament) bool {
		return entry.Tournament.TournamentID != "" && entry.Recovery == nil
	})
}

func (r *tournamentResults) invalidateFinish(stats parsers.TournamentHandStats) error {
	pending, err := config.LoadPendingTournaments(r.path)
	if err != nil {
		return err
	}
	for _, entry := range pending {
		if entry.Tournament.TournamentID == stats.TournamentID && entry.Recovery != nil &&
			compareHandIDs(stats.LastPlayedHandID, entry.Recovery.HandID) > 0 {
			return r.removeRecovery(stats.TournamentID)
		}
	}
	return nil
}

func (r *tournamentResults) history(stats parsers.TournamentHandStats, activity time.Time, historical bool) (posted, review bool, err error) {
	id := stats.TournamentID
	if stats.Date.IsZero() {
		stats.Date = stats.Finish.At
	}
	if historical {
		recorded, found, err := r.server.store.GetTournamentByID(id)
		if err != nil {
			return false, false, err
		}
		if found {
			if !recorded.Recovered() {
				return false, false, r.removeRecovery(id)
			}
			return false, false, nil
		}
	}

	finish := *stats.Finish
	if stats.IsKnockout && finish.PayoutCents != nil && *finish.PayoutCents != 0 {
		// The finish line does not say whether a positive receipt includes bounties.
		finish.PayoutCents = nil
	}
	t := parsers.Tournament{
		Date: stats.Date, TournamentID: id, Type: "mtt", FinishPos: finish.Position,
		ResultSource: parsers.TournamentSourceHistory, ResultHandID: finish.HandID,
	}
	if stats.SpinAndGo {
		t.Type = "spingo"
	}
	now := time.Now()
	entry := config.PendingTournament{
		Tournament: t, Reason: config.PendingMissingResult, Recovery: &finish, FirstQueuedAt: now, Historical: historical,
	}
	until := activity.Add(r.server.QuietPeriod)
	entry.QuietUntil = &until
	accepted := false
	err = r.update(id, func(previous *config.PendingTournament) bool {
		if previous.Tournament.TournamentID != "" && previous.Recovery == nil {
			return true
		}
		if previous.Recovery != nil && compareHandIDs(finish.HandID, previous.Recovery.HandID) < 0 {
			return true
		}
		if previous.Recovery != nil && previous.Recovery.HandID == finish.HandID {
			if previous.Tournament.ResultSource == parsers.TournamentSourceConfirmed {
				return true
			}
			entry.FirstQueuedAt = previous.FirstQueuedAt
			entry.LastRemindedAt = previous.LastRemindedAt
			// A folder scan must not prematurely finalize a tournament
			// that is still within its live quiet period.
			if historical && previous.QuietUntil != nil && previous.QuietUntil.After(now) {
				return true
			}
			if previous.Historical {
				entry.Historical = true
			}
			if !historical && previous.QuietUntil != nil && entry.QuietUntil.Before(*previous.QuietUntil) {
				entry.QuietUntil = previous.QuietUntil
			}
		}
		accepted = true
		*previous = entry
		return true
	})
	if err != nil || !accepted {
		return false, false, err
	}
	if historical && !entry.QuietUntil.After(now) {
		stopQuietTimer(r.timers, id)
		return r.finish(entry)
	}
	r.arm(entry)
	return false, historical && (finish.TotalCostCents == nil || finish.PayoutCents == nil), nil
}

func compareHandIDs(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}

func (r *tournamentResults) restoreExpired() error {
	pending, err := config.LoadPendingTournaments(r.path)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, entry := range pending {
		if entry.QuietUntil != nil && !entry.QuietUntil.After(now) && r.timers[entry.Tournament.TournamentID] == nil {
			r.arm(entry)
		}
	}
	return nil
}

func (r *tournamentResults) expire(id string) error {
	pending, err := config.LoadPendingTournaments(r.path)
	if err != nil {
		return err
	}
	for _, entry := range pending {
		if entry.Tournament.TournamentID == id && entry.QuietUntil != nil {
			if entry.QuietUntil.After(time.Now()) {
				r.arm(entry)
				return nil
			}
			_, _, err := r.finish(entry)
			return err
		}
	}
	return nil
}

func (r *tournamentResults) finish(entry config.PendingTournament) (posted, review bool, err error) {
	t := entry.Tournament
	recorded, found, err := r.server.store.GetTournamentByID(t.TournamentID)
	if err != nil {
		return false, false, err
	}
	if found && (!recorded.Recovered() || compareHandIDs(t.ResultHandID, recorded.ResultHandID) <= 0) {
		return false, false, r.removeRecovery(t.TournamentID)
	}
	finish := entry.Recovery
	if finish == nil {
		return false, false, nil
	}
	if finish.TotalCostCents == nil || finish.PayoutCents == nil {
		entry.QuietUntil = nil
		err := r.update(t.TournamentID, func(current *config.PendingTournament) bool {
			entry.LastRemindedAt = current.LastRemindedAt
			*current = entry
			return true
		})
		return false, err == nil, err
	}
	t.BuyInCents = *finish.TotalCostCents
	t.NetCents, err = money.Subtract(*finish.PayoutCents, t.BuyInCents)
	if err != nil {
		return false, false, err
	}
	err = r.write(t, recorded, found, entry.Historical).Err
	if err != nil {
		return false, false, err
	}
	return true, false, r.removeRecovery(t.TournamentID)
}

func validateOfficialSummary(t parsers.Tournament) error {
	if t.TournamentID == "" || t.Date.IsZero() || t.FinishPos <= 0 {
		return fmt.Errorf("tournament summary %q has no dated player finish", t.TournamentID)
	}
	return nil
}

func (r *tournamentResults) official(t parsers.Tournament, historical bool) (posted, skipped bool, err error) {
	if err := validateOfficialSummary(t); err != nil {
		return false, false, err
	}
	id := t.TournamentID
	if err := r.removeRecovery(id); err != nil {
		return false, false, err
	}
	recorded, found, duplicate, err := r.inspectOfficial(t, historical)
	var reason string
	switch {
	case t.LikelySatellite:
		reason = config.PendingSatellite
	case t.BuyInCents == 0:
		reason = config.PendingFreerollBuyin
	}
	if err != nil {
		return false, false, err
	}
	if historical && reason != "" && !found {
		return false, true, nil
	}
	if duplicate {
		// Preserve the scan report's count of successfully imported summaries.
		return historical && reason == "", historical && reason != "", nil
	}
	if reason != "" {
		if historical {
			return false, true, nil
		}
		err = r.update(id, func(entry *config.PendingTournament) bool {
			*entry = config.PendingTournament{Tournament: t, Reason: reason, FirstQueuedAt: time.Now()}
			return true
		})
		return false, false, err
	}
	err = r.write(t, recorded, found, historical).Err
	return err == nil, false, err
}

func (r *tournamentResults) inspectOfficial(t parsers.Tournament, historical bool) (recorded parsers.Tournament, found, duplicate bool, err error) {
	recorded, found, err = r.server.store.GetTournamentByID(t.TournamentID)
	if err != nil {
		return recorded, found, false, err
	}
	pending, err := config.LoadPendingTournaments(r.path)
	if err != nil {
		return recorded, found, false, err
	}
	duplicate, refresh := isDuplicateTournament(t, pending, recorded, found)
	if refresh >= 0 && !historical {
		err = r.update(t.TournamentID, func(entry *config.PendingTournament) bool {
			entry.Tournament = t
			return true
		})
	}
	return recorded, found, duplicate, err
}

func (r *tournamentResults) write(t, recorded parsers.Tournament, found, historical bool) storage.OperationResult {
	var cfg *config.Config
	if !historical {
		var err error
		cfg, err = config.LoadConfig(r.server.configPath)
		if err != nil {
			return storage.OperationResult{Found: true, Err: err}
		}
	}
	before := sheetsPending(r.server.store)
	netCents, err := r.server.store.AppendTournament(t)
	after := sheetsPending(r.server.store)
	if err != nil {
		if !historical {
			if msg := writeFailureMessage(before, after, err); msg != "" {
				bot.PostAlert(r.server.alertChan, msg)
			}
		}
		return storage.OperationResult{Found: true, Err: fmt.Errorf("recording tournament %s: %w", t.TournamentID, err)}
	}
	result := storage.OperationResult{Found: true, NetCents: netCents}
	if !historical {
		if msg := syncedMessage(before, after); msg != "" {
			bot.PostAlert(r.server.alertChan, msg)
		}
		notify := !found || !recorded.Recovered() || !equivalentRecovery(t, recorded, netCents)
		result.BankrollCents = r.server.evaluateResult(cfg, nil, &t, netCents, notify, r.fired, r.enforce)
	}
	return result
}

func equivalentRecovery(incoming, recorded parsers.Tournament, netCents money.Cents) bool {
	return incoming.BuyInCents == recorded.BuyInCents && netCents == recorded.NetCents &&
		incoming.FinishPos == recorded.FinishPos &&
		(incoming.TotalPlayers == recorded.TotalPlayers || incoming.TotalPlayers == 0 || recorded.TotalPlayers == 0)
}

func (r *tournamentResults) confirm(answer storage.TournamentAnswer) storage.OperationResult {
	pending, err := config.LoadPendingTournaments(r.path)
	if err != nil {
		return storage.OperationResult{Err: err}
	}
	for _, entry := range pending {
		t := entry.Tournament
		reason := entry.Reason
		if reason == "" {
			reason = config.PendingSatellite
		}
		if t.TournamentID != answer.TournamentID || reason != answer.Reason || entry.QuietUntil != nil {
			continue
		}
		if entry.Recovery == nil {
			return r.confirmOfficial(t, answer)
		}
		if t.ResultHandID != answer.HandID {
			continue
		}
		payoutCents := entry.Recovery.PayoutCents
		if payoutCents == nil {
			payoutCents = answer.PayoutCents
		}
		costCents := answer.TotalCostCents
		if entry.Recovery.TotalCostCents != nil {
			costCents = *entry.Recovery.TotalCostCents
		}
		if payoutCents == nil || *payoutCents < 0 || costCents < 0 {
			return storage.OperationResult{Err: fmt.Errorf("confirm the cash payout and total cash entry cost")}
		}
		t.BuyInCents = costCents
		t.NetCents, err = money.Subtract(*payoutCents, t.BuyInCents)
		if err != nil {
			return storage.OperationResult{Err: err}
		}
		t.ResultSource = parsers.TournamentSourceConfirmed
		recorded, found, err := r.server.store.GetTournamentByID(t.TournamentID)
		if err != nil {
			return storage.OperationResult{Err: err}
		}
		if found && !recorded.Recovered() {
			return storage.OperationResult{Err: r.removeRecovery(t.TournamentID)}
		}
		if found && recorded.ResultHandID == t.ResultHandID && recorded.ResultSource == parsers.TournamentSourceConfirmed {
			return storage.OperationResult{Found: true, NetCents: recorded.NetCents, Err: r.removeRecovery(t.TournamentID)}
		}
		entry.Tournament = t
		entry.Recovery.TotalCostCents = &t.BuyInCents
		entry.Recovery.PayoutCents = payoutCents
		now := time.Now()
		entry.QuietUntil = &now
		if err := r.update(t.TournamentID, func(current *config.PendingTournament) bool {
			entry.LastRemindedAt = current.LastRemindedAt
			*current = entry
			return true
		}); err != nil {
			return storage.OperationResult{Found: true, Err: err}
		}
		result := r.write(t, recorded, found, entry.Historical)
		if result.Err == nil {
			result.Err = r.removeRecovery(t.TournamentID)
		}
		return result
	}
	return storage.OperationResult{}
}

func (r *tournamentResults) confirmOfficial(t parsers.Tournament, answer storage.TournamentAnswer) storage.OperationResult {
	switch answer.Reason {
	case config.PendingSatellite:
		if answer.Ticket {
			t.BuyInCents, t.NetCents = bot.RecoverTicketPayout(t.BuyInCents, t.NetCents)
		}
	case config.PendingFreerollBuyin:
		t.BuyInCents = answer.TotalCostCents
		var err error
		t.NetCents, err = money.Subtract(t.NetCents, t.BuyInCents)
		if err != nil {
			return storage.OperationResult{Found: true, Err: err}
		}
	}
	recorded, found, err := r.server.store.GetTournamentByID(t.TournamentID)
	if err != nil {
		return storage.OperationResult{Found: true, Err: err}
	}
	result := r.write(t, recorded, found, false)
	if result.Err == nil {
		result.Err = r.update(t.TournamentID, func(*config.PendingTournament) bool { return false })
	}
	return result
}

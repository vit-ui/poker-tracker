package server

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/vit-ui/self-tracker/internal/bot"
	"github.com/vit-ui/self-tracker/internal/config"
)

// closureQueue is owned exclusively by the server worker. Receipts remain after
// delivery; forgetting IDs would turn collector replay into new notifications.
type closureQueue struct {
	path             string
	entries          map[string]config.ClosureNotification
	attempted        map[string]bool
	inflight         map[string]bool
	acknowledgements map[string]bool
	dirty            bool
	replay           bool
}

func loadClosureQueue(path string) (*closureQueue, error) {
	entries, err := config.LoadClosureNotifications(path)
	if err != nil {
		return nil, err
	}
	return &closureQueue{path: path, entries: entries, attempted: make(map[string]bool), inflight: make(map[string]bool), acknowledgements: make(map[string]bool)}, nil
}

func (q *closureQueue) receive(event config.ClosureEvent) error {
	if event.ID == "" || event.ClosedAt.IsZero() || !event.Lock.ExpiresAt.After(event.ClosedAt) {
		return fmt.Errorf("invalid PokerStars closure event")
	}
	if _, seen := q.entries[event.ID]; !seen {
		q.entries[event.ID] = config.ClosureNotification{Event: event}
		if err := config.SaveClosureNotifications(q.path, q.entries); err != nil {
			delete(q.entries, event.ID)
			return err
		}
		q.dirty = false
	}
	q.acknowledgements[event.ID] = true
	return nil
}

func (q *closureQueue) next() *config.ClosureEvent {
	var ids []string
	for id, entry := range q.entries {
		if !entry.Delivered && !q.attempted[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := q.entries[ids[i]].Event, q.entries[ids[j]].Event
		if a.ClosedAt.Equal(b.ClosedAt) {
			return a.ID < b.ID
		}
		return a.ClosedAt.Before(b.ClosedAt)
	})
	event := q.entries[ids[0]].Event
	return &event
}

func (q *closureQueue) result(result bot.ClosureResult) {
	if !q.inflight[result.ID] {
		return
	}
	delete(q.inflight, result.ID)
	if result.Err != nil {
		return
	} // maintenance/reconnect provides the next opportunity
	entry := q.entries[result.ID]
	entry.Delivered = true
	q.entries[result.ID] = entry
	q.dirty = true
	q.persist()
}

func (q *closureQueue) persist() {
	if !q.dirty {
		return
	}
	if err := config.SaveClosureNotifications(q.path, q.entries); err != nil {
		slog.Error("saving closure delivery result", "err", err)
	} else {
		q.dirty = false
	}
}

func (q *closureQueue) retry() {
	q.persist()
	for id := range q.attempted {
		if !q.inflight[id] {
			delete(q.attempted, id)
		}
	}
	q.replay = true
}

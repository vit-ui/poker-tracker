package client

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/watcher"
)

// collection belongs to the collector loop. Replacements start paused, and the
// previous watcher drains before changing the screen name used for uploads.
type collection struct {
	events chan watcher.FileEvent
	active *collectionWatcher
	err    error // collection is inactive; commands and enforcement remain available
}

type collectionWatcher struct {
	control *watcher.Control
	done    chan struct{}
	err     error // read only after done closes
}

func (w *collectionWatcher) stop() ([]watcher.FileEvent, error) {
	events, err := w.control.Stop()
	<-w.done
	return events, errors.Join(err, w.err)
}

func (c *collection) prepare(settings config.CollectorSettings) (*collectionWatcher, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if !settings.Complete() {
		return nil, nil
	}
	w := &collectionWatcher{control: watcher.NewPausedControl(), done: make(chan struct{})}
	go func() {
		w.err = watcher.WatchControlled(nil, filepath.Join(*settings.HandHistory, *settings.ScreenName), filepath.Join(*settings.TournamentSummary, *settings.ScreenName), c.events, w.control)
		close(w.done)
	}()
	if _, err := w.control.SetPaused(true); err != nil {
		<-w.done
		return nil, fmt.Errorf("preparing file watcher: %w", errors.Join(err, w.err))
	}
	return w, nil
}

func (c *collection) setPaused(paused bool) ([]watcher.FileEvent, error) {
	if c.active == nil {
		return nil, nil
	}
	return c.active.control.SetPaused(paused)
}

func (c *collection) stopped() <-chan struct{} {
	if c.active == nil {
		return nil
	}
	return c.active.done
}

// Serve owns watcher startup, replacement and cleanup in both deployment modes.
// A required watcher failure returns to the service manager's restart path.
func (c *Collector) Serve(configPath string, dead <-chan struct{}) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	c.collection = &collection{events: make(chan watcher.FileEvent, 10)}
	c.control = c.collection.setPaused
	c.collection.active, err = c.collection.prepare(config.CollectionSettings(cfg))
	if err != nil {
		c.collection.err = err
		slog.Error("collection inactive; waiting for folder correction", "err", err)
	}
	defer func() {
		if c.collection.active != nil {
			c.collection.active.stop()
		}
	}()
	if _, err := c.control(c.StartPaused); err != nil {
		return err
	}
	return c.Run(c.collection.events, configPath, dead)
}

func (c *Collector) changeSettings(configPath string, patch config.CollectorSettings, upload func(watcher.FileEvent, bool) error) (config.CollectorSettings, error) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return config.CollectorSettings{}, err
	}
	previous := config.CollectionSettings(cfg)
	settings, err := config.ResolveCollection(cfg, patch)
	if err != nil {
		return previous, err
	}
	if c.collection == nil {
		return previous, fmt.Errorf("collector-owned watcher is unavailable")
	}
	if reflect.DeepEqual(previous, settings) && (c.collection.active != nil || !settings.Complete()) {
		return previous, nil
	}
	next, err := c.collection.prepare(settings)
	if err != nil {
		return previous, err
	}
	stopNext := func() {
		if next != nil {
			next.stop()
		}
	}
	var pending []watcher.FileEvent
	if c.collection.active != nil {
		pending, err = c.collection.active.stop()
		c.collection.active = nil
	}
	// The old watcher has stopped sending, so this includes every accepted,
	// buffered event; all uploads still read the old screen name from config.
drain:
	for {
		select {
		case event := <-c.collection.events:
			pending = append(pending, event)
		default:
			break drain
		}
	}
	for _, event := range pending {
		err = errors.Join(err, upload(event, false))
	}
	saved := false
	if err == nil {
		_, err = config.UpdateConfig(configPath, func(cfg *config.Config) error {
			settings.Apply(cfg)
			return nil
		})
		saved = err == nil
	}
	if err == nil && next != nil {
		_, err = next.control.SetPaused(false)
	}
	if err == nil {
		c.collection.active = next
		c.collection.err = nil
		return settings, nil
	}
	stopNext()
	var rollbackErr error
	if saved {
		_, rollbackErr = config.UpdateConfig(configPath, func(cfg *config.Config) error {
			cfg.Setup.PokerStarsScreenName = previous.ScreenName
			cfg.Paths = config.Paths{HandHistory: previous.HandHistory, TournamentSummary: previous.TournamentSummary}
			return nil
		})
	}
	actual, readErr := config.LoadConfig(configPath)
	if readErr != nil || !reflect.DeepEqual(previous, config.CollectionSettings(actual)) {
		// Never resume old folders under an unconfirmed on-disk screen name.
		c.collection.err = errors.Join(fmt.Errorf("previous collection settings could not be restored"), rollbackErr, readErr)
		if readErr == nil {
			previous = config.CollectionSettings(actual)
		}
		return previous, errors.Join(err, c.collection.err)
	}
	old, restoreErr := c.collection.prepare(previous)
	c.collection.active = old
	if old != nil {
		_, restoreErr = old.control.SetPaused(false)
	}
	c.collection.err = restoreErr
	return previous, errors.Join(err, rollbackErr, restoreErr, readErr)
}

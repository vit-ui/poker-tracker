package watcher

import "fmt"

// Control belongs to one watcher run. A pause returns all accepted file events
// that have not been delivered, including files still in their debounce period.
// The collector sends those files before acknowledging its own pause upstream.
type Control struct {
	requests        chan pauseRequest
	done            chan struct{}
	initiallyPaused bool
}

type pauseRequest struct {
	paused bool
	stop   bool
	reply  chan pauseResult
}

type pauseResult struct {
	events []FileEvent
}

func NewControl() *Control {
	return &Control{requests: make(chan pauseRequest), done: make(chan struct{})}
}

// NewPausedControl prepares a replacement without delivering events before its
// settings are committed and the previous watcher has drained.
func NewPausedControl() *Control {
	c := NewControl()
	c.initiallyPaused = true
	return c
}

// Stop drains accepted events and waits for watcher-owned resources to close.
func (c *Control) Stop() ([]FileEvent, error) {
	events, err := c.change(pauseRequest{paused: true, stop: true, reply: make(chan pauseResult, 1)})
	<-c.done
	return events, err
}

// SetPaused waits for the watcher to apply the transition. Resume retains any
// changes observed while paused and debounces them normally. A failed/stopped
// watcher cannot acknowledge a successful pause.
func (c *Control) SetPaused(paused bool) ([]FileEvent, error) {
	request := pauseRequest{paused: paused, reply: make(chan pauseResult, 1)}
	return c.change(request)
}

func (c *Control) change(request pauseRequest) ([]FileEvent, error) {
	select {
	case c.requests <- request:
	case <-c.done:
		return nil, fmt.Errorf("file watcher is not running")
	}
	select {
	case result := <-request.reply:
		return result.events, nil
	case <-c.done:
		select {
		case result := <-request.reply:
			return result.events, nil
		default:
		}
		return nil, fmt.Errorf("file watcher stopped during collection change")
	}
}

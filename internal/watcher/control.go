package watcher

import "fmt"

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

func NewPausedControl() *Control {
	c := NewControl()
	c.initiallyPaused = true
	return c
}

func (c *Control) Stop() ([]FileEvent, error) {
	events, err := c.change(pauseRequest{paused: true, stop: true, reply: make(chan pauseResult, 1)})
	<-c.done
	return events, err
}

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

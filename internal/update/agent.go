package update

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/vit-ui/self-tracker/internal/config"
)

type queuedRequest struct {
	request config.UpdateRequest
	reply   chan requestResult
}
type requestResult struct {
	report config.UpdateReport
	err    error
}

// Agent owns request acceptance in the service. The detached helper is the
// sole journal writer from launch until completion; the service only observes.
type Agent struct {
	Role, Version          string
	target, installationID string
	requests               chan queuedRequest
	Changes                chan config.UpdateReport
	done                   chan struct{}
}

func NewAgent(role, version, installationID string) (*Agent, error) {
	target, err := os.Executable()
	if err != nil {
		return nil, err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return nil, err
	}
	return &Agent{Role: role, Version: version, target: target, installationID: installationID,
		requests: make(chan queuedRequest), Changes: make(chan config.UpdateReport, 1), done: make(chan struct{})}, nil
}

func (a *Agent) path() string { return filepath.Join(filepath.Dir(a.target), "requested_update.json") }

func (a *Agent) Done() <-chan struct{} { return a.done }

func (a *Agent) Report() (config.UpdateReport, error) {
	job, err := config.LoadUpdateJob(a.path())
	return config.UpdateReport{Version: a.Version, Job: job}, err
}

func (a *Agent) Request(request config.UpdateRequest) (config.UpdateReport, error) {
	reply := make(chan requestResult, 1)
	select {
	case a.requests <- queuedRequest{request, reply}:
	case <-a.done:
		return config.UpdateReport{}, fmt.Errorf("update request owner stopped")
	}
	select {
	case result := <-reply:
		return result.report, result.err
	case <-a.done:
		return config.UpdateReport{}, fmt.Errorf("update request owner stopped")
	}
}

// Run observes atomic journal replacement through fsnotify. It never polls
// PokerStars folders, stops collection, or waits for a downloaded executable.
func (a *Agent) Run(stop, ready <-chan struct{}) {
	defer close(a.done)
	if ready != nil {
		select {
		case <-ready:
		case <-stop:
			return
		}
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("watching update requests", "err", err)
		return
	}
	defer watcher.Close()
	if err := watcher.Add(filepath.Dir(a.target)); err != nil {
		slog.Error("watching update requests", "err", err)
		return
	}
	var previous config.UpdateReport
	launched := ""
	observe := func() {
		report, err := a.Report()
		if err != nil {
			slog.Error("reading update request", "err", err)
			return
		}
		if report.Job != nil && report.Job.Pending() && launched != report.Job.Request.ID {
			launched = report.Job.Request.ID
			if err := launchRequested(a.target, a.Role, a.installationID, report.Job.Request.ID); err != nil {
				job := *report.Job
				job.State, job.Error = config.UpdateFailed, err.Error()
				if saveErr := config.SaveUpdateJob(a.path(), job); saveErr != nil {
					slog.Error("saving update launch failure", "err", saveErr)
				} else {
					report.Job = &job
				}
			}
		}
		if reflect.DeepEqual(previous, report) {
			return
		}
		if report.Job != nil && report.Job.State == config.UpdateFailed {
			slog.Error("requested update failed", "version", report.Job.Request.Version, "err", report.Job.Error)
		}
		previous = report
		select {
		case <-a.Changes:
		default:
		}
		a.Changes <- report
	}
	observe()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// Resume an interrupted pending attempt only after the previous
			// helper released its process-held lock. Recorded failures are terminal.
			if report, err := a.Report(); err == nil && report.Job != nil && report.Job.Pending() && report.Job.Request.ID == launched {
				if release, err := lockRequested(filepath.Join(filepath.Dir(a.target), ".tracker-request.lock")); err == nil {
					release()
					launched = ""
				}
			}
			observe()
		case call := <-a.requests:
			report, err := a.Report()
			if err == nil {
				err = acceptRequest(a.path(), a.Version, report.Job, call.request)
			}
			if err == nil {
				observe()
				report, err = a.Report()
			}
			call.reply <- requestResult{report, err}
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Name == a.path() {
				observe()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Error("watching update request", "err", err)
		}
	}
}

func acceptRequest(path, current string, job *config.UpdateJob, request config.UpdateRequest) error {
	if request.ID == "" {
		return fmt.Errorf("update request has no identity")
	}
	if err := ValidateRequestVersion(request.Version); err != nil {
		return err
	}
	if _, err := newer(request.Version, current); err != nil {
		return err
	}
	if job != nil && (job.Pending() || job.Request.ID == request.ID) {
		if job.Pending() && job.Request.Version != request.Version {
			return fmt.Errorf("update %s is already pending; wait for it to finish before requesting %s", job.Request.Version, request.Version)
		}
		return nil
	}
	return config.SaveUpdateJob(path, config.UpdateJob{Request: request, State: config.UpdateQueued, Version: current})
}

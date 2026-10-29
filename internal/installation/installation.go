// Package installation keeps installer process and task operations scoped to a deployment.
package installation

import "runtime"

// TaskName returns the existing production task name, or a separate name for
// a disposable build. The same ID must be compiled into the installer and binary.
func TaskName(id string, server bool) string {
	name := "Poker Tracker"
	if server {
		name = "PokerTrackerServer"
	}
	if id != "" {
		name += "-" + id
	}
	return name
}

// ServerServiceName is shared by server setup and bot-triggered restarts.
// Production retains its existing service/task name on each supported OS.
func ServerServiceName(id string) string {
	if runtime.GOOS != "linux" {
		return TaskName(id, true)
	}
	name := "poker-tracker"
	if id != "" {
		name += "-" + id
	}
	return name + ".service"
}

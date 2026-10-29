// Package installation keeps installer process and task operations scoped to a deployment.
package installation

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

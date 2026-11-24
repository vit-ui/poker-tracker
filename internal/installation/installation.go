package installation

import "runtime"

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

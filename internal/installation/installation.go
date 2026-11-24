package installation

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

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

func ExecutableDir() string {
	exe, err := os.Executable()
	if err != nil {
		panic("getting executable path: " + err.Error())
	}
	return filepath.Dir(exe)
}

func ReportRole(role, outputPath string) error {
	if outputPath != "" {
		return os.WriteFile(outputPath, []byte(role), 0600)
	}
	fmt.Println(role)
	return nil
}

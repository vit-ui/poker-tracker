package installation

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf16"
)

func WindowsTaskXML(executable, directory string, now time.Time) string {
	return WindowsCommandTaskXML(executable, directory, "--serve", now)
}

func WindowsCommandTaskXML(executable, directory, arguments string, now time.Time) string {
	escape := func(value string) string {
		var out bytes.Buffer
		xml.EscapeText(&out, []byte(value))
		return out.String()
	}
	return fmt.Sprintf(taskSchedulerXML, now.Format("2006-01-02T15:04:05"), escape(executable), escape(arguments), escape(directory))
}

func RegisterWindowsTask(name, executable string) error {
	file, err := os.CreateTemp("", "poker-tracker-task-*.xml")
	if err != nil {
		return fmt.Errorf("creating task definition: %w", err)
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(utf16File(WindowsTaskXML(executable, filepath.Dir(executable), time.Now())))
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("writing task definition: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing task definition: %w", closeErr)
	}
	output, err := exec.Command("schtasks.exe", "/Create", "/TN", name, "/XML", file.Name(), "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("registering installation task %s: %w: %s", name, err, output)
	}
	return nil
}

func utf16File(s string) []byte {
	points := utf16.Encode([]rune(s))
	buf := make([]byte, 2+2*len(points))
	buf[0], buf[1] = 0xFF, 0xFE
	for i, p := range points {
		buf[2+2*i] = byte(p)
		buf[2+2*i+1] = byte(p >> 8)
	}
	return buf
}

const taskSchedulerXML = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <BootTrigger>
      <Enabled>true</Enabled>
      <Delay>PT30S</Delay>
    </BootTrigger>
    <TimeTrigger>
      <Repetition><Interval>PT1M</Interval><StopAtDurationEnd>false</StopAtDurationEnd></Repetition>
      <StartBoundary>%s</StartBoundary>
      <Enabled>true</Enabled>
    </TimeTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-18</UserId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>true</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure><Interval>PT1M</Interval><Count>255</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>%s</Arguments>
      <WorkingDirectory>%s</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`

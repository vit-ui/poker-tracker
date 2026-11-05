package installation

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestShutdownSignalReachesOwnerAndRunsDefers(t *testing.T) {
	if root := os.Getenv("TRACKER_SHUTDOWN_CHILD"); root != "" {
		done, release, err := ListenShutdown(root)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		defer os.WriteFile(filepath.Join(root, "cleaned"), []byte("owner cleanup"), 0600)
		os.Stdout.WriteString("ready\n")
		<-done
		return
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			root := t.TempDir()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(exe, "-test.run=^TestShutdownSignalReachesOwnerAndRunsDefers$")
			child.Env = append(os.Environ(), "TRACKER_SHUTDOWN_CHILD="+root)
			output, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { child.Process.Kill(); child.Wait() }()
			if !bufio.NewScanner(output).Scan() {
				t.Fatal("owner did not register")
			}
			if err := child.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, "cleaned"))
			if err != nil || string(data) != "owner cleanup" {
				t.Fatalf("cleanup=%q, %v", data, err)
			}
		})
	}
}

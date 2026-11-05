package installation

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStop(t *testing.T) {
	if os.Getenv("TRACKER_INSTALLATION_TEST_CHILD") == "1" {
		// Exercise self-exclusion in a real tracker.exe process before allowing
		// the parent to exercise process-path matching.
		exe, err := os.Executable()
		if err != nil {
			os.Exit(3)
		}
		if err := Stop(filepath.Dir(exe)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		done, cleanup, err := ListenShutdown(filepath.Dir(exe))
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		defer os.WriteFile(filepath.Join(filepath.Dir(exe), "cleaned"), []byte("done"), 0600)
		fmt.Println("ready")
		<-done
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tracker.exe", "tracker-server.exe", "tracker-client.exe"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "install")
			other := target + "-other"
			var children []*exec.Cmd
			for _, dir := range []string{target, other} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, data, 0700); err != nil {
					t.Fatal(err)
				}
				child := exec.Command(path, "-test.run=^TestStop$")
				child.Env = append(os.Environ(), "TRACKER_INSTALLATION_TEST_CHILD=1")
				output, err := child.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				child.Stderr = os.Stderr
				if err := child.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { child.Process.Kill(); child.Wait() })
				if !bufio.NewScanner(output).Scan() {
					t.Fatalf("child did not survive self-exclusion: %v", child.Wait())
				}
				children = append(children, child)
			}
			if err := Stop(target); err != nil {
				t.Fatal(err)
			}
			if err := children[0].Wait(); err != nil {
				t.Fatalf("target termination: %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(target, "cleaned")); err != nil || string(data) != "done" {
				t.Fatalf("owner cleanup: %q, %v", data, err)
			}
			handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(children[1].Process.Pid))
			if err != nil {
				t.Fatalf("other installation did not survive: %v", err)
			}
			var exitCode uint32
			err = windows.GetExitCodeProcess(handle, &exitCode)
			windows.CloseHandle(handle)
			if err != nil || exitCode != 259 { // STILL_ACTIVE
				t.Fatalf("other installation exit code = %d, error = %v", exitCode, err)
			}
			if err := Stop(other); err != nil {
				t.Fatal(err)
			}
			if err := children[1].Wait(); err != nil {
				t.Fatalf("other installation termination: %v", err)
			}
		})
	}
}

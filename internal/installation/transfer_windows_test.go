package installation

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestTransferMailbox(t *testing.T) {
	if root := os.Getenv("TRACKER_TRANSFER_MAILBOX_CHILD"); root != "" {
		mailbox, err := NewTransferMailbox(root)
		if err != nil {
			t.Fatal(err)
		}
		defer mailbox.Close()
		defer os.WriteFile(filepath.Join(root, "cleaned"), []byte("owner exited"), 0600)
		fmt.Println(os.Getpid())
		for range mailbox.Events {
			data, err := os.ReadFile(filepath.Join(root, "request"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "result"), append([]byte("ack:"), data...), 0600); err != nil {
				t.Fatal(err)
			}
			if err := mailbox.Acknowledge(); err != nil {
				t.Fatal(err)
			}
			if string(data) == "stop" {
				return
			}
		}
		return
	}
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^TestTransferMailbox$")
	child.Env = append(os.Environ(), "TRACKER_TRANSFER_MAILBOX_CHILD="+root)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("source did not register events")
	}
	pid, err := strconv.Atoi(scanner.Text())
	if err != nil {
		t.Fatal(err)
	}
	if wrong, err := OpenTransferPeer(root+"-other", pid); err == nil {
		wrong.Close()
		t.Fatal("other installation reached source mailbox")
	}
	peer, err := OpenTransferPeer(root, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	for _, request := range []string{"prepare", "abort", "stop"} {
		if err := peer.Reset(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "request"), []byte(request), 0600); err != nil {
			t.Fatal(err)
		}
		if err := peer.Notify(); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { result <- peer.Wait() }()
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("mailbox did not acknowledge")
		}
		data, err := os.ReadFile(filepath.Join(root, "result"))
		if err != nil || string(data) != "ack:"+request {
			t.Fatalf("result=%s %v", data, err)
		}
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "cleaned")); err != nil {
		t.Fatal("owner deferred cleanup did not run", err)
	}
}

func TestTransferMailboxCloseInterruptsUndeliveredEvent(t *testing.T) {
	root := t.TempDir()
	mailbox, err := NewTransferMailbox(root)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := OpenTransferPeer(root, os.Getpid())
	if err != nil {
		mailbox.Close()
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.Notify(); err != nil {
		mailbox.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { mailbox.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closing mailbox left its event goroutine blocked")
	}
}

func TestTransferHelperMutexIsInstallationScoped(t *testing.T) {
	root := t.TempDir()
	release, err := AcquireTransferLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := AcquireTransferLock(root); err == nil {
		second()
		release()
		t.Fatal("concurrent helper acquired same installation")
	}
	other, err := AcquireTransferLock(root + "-other")
	if err != nil {
		release()
		t.Fatal(err)
	}
	other()
	release()
	next, err := AcquireTransferLock(root)
	if err != nil {
		t.Fatal("released helper lock could not be reacquired", err)
	}
	next()
}

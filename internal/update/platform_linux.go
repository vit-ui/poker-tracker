package update

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/vit-ui/self-tracker/internal/installation"
	"golang.org/x/sys/unix"
)

func attachConsole() {}
func runHelper(path string, args []string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

type serviceState struct {
	target, name     string
	managed, running bool
	pids             []int
	lock             *os.File
}

func inspectService(target, role, id string, parent int) (*serviceState, error) {
	s := &serviceState{target: target, name: installation.ServerServiceName(id)}
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(target), ".tracker-update.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another updater owns this installation: %w", err)
	}
	s.lock = lock
	ok := false
	defer func() {
		if !ok {
			s.close()
		}
	}()
	result, err := exec.Command("systemctl", "--user", "show", s.name, "--property=ExecStart", "--property=MainPID", "--property=ActiveState", "--property=LoadState").CombinedOutput()
	if err == nil {
		properties := string(result)
		s.managed = strings.Contains(properties, "path="+target+" ;")
		for _, line := range strings.Split(properties, "\n") {
			if strings.HasPrefix(line, "MainPID=") {
				pid, _ := strconv.Atoi(strings.TrimPrefix(line, "MainPID="))
				s.running = s.managed && pid > 0
			}
		}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == parent || pid == os.Getpid() {
			continue
		}
		path, err := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
		if err != nil {
			continue
		}
		if path == target {
			cgroup, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "cgroup"))
			if readErr != nil {
				return nil, readErr
			}
			if !s.managed && strings.Contains(string(cgroup), "/"+s.name) {
				return nil, fmt.Errorf("cannot verify the service owning this installation")
			}
			s.pids = append(s.pids, pid)
		}
	}
	if s.managed && !s.running && len(s.pids) > 0 {
		return nil, fmt.Errorf("installation has a foreground owner alongside its stopped service; stop it before updating")
	}
	ok = true
	return s, nil
}
func (s *serviceState) close() {
	if s.lock != nil {
		s.lock.Close()
	}
}
func (s *serviceState) stop() error {
	if s.managed && s.running {
		output, err := exec.Command("systemctl", "--user", "stop", s.name).CombinedOutput()
		if err != nil {
			return fmt.Errorf("stopping %s: %w: %s", s.name, err, output)
		}
		return nil
	}
	for _, pid := range s.pids {
		fd, err := unix.PidfdOpen(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return err
		}
		err = unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0)
		if err == nil {
			_, err = unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, -1)
		}
		unix.Close(fd)
		if err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
	}
	return nil
}
func (s *serviceState) restore() error {
	if s.managed {
		if !s.running {
			return nil
		}
		output, err := exec.Command("systemctl", "--user", "start", s.name).CombinedOutput()
		if err != nil {
			return fmt.Errorf("starting %s: %w: %s", s.name, err, output)
		}
		output, err = exec.Command("systemctl", "--user", "is-active", s.name).CombinedOutput()
		if err != nil {
			return fmt.Errorf("service did not become active: %w: %s", err, output)
		}
		return nil
	}
	if len(s.pids) == 0 {
		return nil
	}
	for _, pid := range s.pids {
		path, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
		if err == nil && path == s.target {
			return nil
		}
	}
	cmd := exec.Command(s.target, "--serve")
	cmd.Dir = filepath.Dir(s.target)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.pids = []int{cmd.Process.Pid}
	return cmd.Process.Release()
}

func (s *serviceState) wasRunning() bool { return s.running || len(s.pids) > 0 }
func (s *serviceState) alive() (bool, error) {
	if s.managed {
		output, err := exec.Command("systemctl", "--user", "show", s.name, "--property=MainPID", "--value").CombinedOutput()
		if err != nil {
			return false, fmt.Errorf("checking startup owner: %w: %s", err, output)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
		if err != nil {
			return false, err
		}
		return pid > 0, nil
	}
	for _, pid := range s.pids {
		path, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
		if err == nil && path == s.target {
			return true, nil
		}
	}
	return false, nil
}

func recoverPendingUpdate(string, string, string, int) (string, error) { return "", nil }
func (*serviceState) prepareUpdate(string, string, string) error       { return nil }
func (s *serviceState) previousExecutable() (string, error)            { return reservePrevious(s.target) }
func (*serviceState) confirmUpdate() error                             { return nil }
func (*serviceState) finishUpdate() error                              { return nil }

func moveExecutable(from, to string) error { return os.Rename(from, to) }

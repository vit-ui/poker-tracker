package bot

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// selfRestart queues a restart of the user service that actually owns this
// process. A manually launched server must not restart another installation's
// service just because it uses the same production unit name.
func selfRestart(_ string, serviceName string) error {
	output, err := exec.Command("systemctl", "--user", "show", "--property=MainPID", "--value", serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("checking restart service %s: %w: %s", serviceName, err, strings.TrimSpace(string(output)))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil || pid != os.Getpid() {
		return fmt.Errorf("service %s does not own this server process; restart this installation manually", serviceName)
	}
	// --no-block acknowledges the manager's queued job without waiting for the
	// process making this request to stop. The caller has already drained work.
	output, err = exec.Command("systemctl", "--user", "--no-block", "restart", serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("requesting restart of %s: %w: %s", serviceName, err, strings.TrimSpace(string(output)))
	}
	return nil
}

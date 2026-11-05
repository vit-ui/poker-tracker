package bot

import (
	"path/filepath"

	"github.com/vit-ui/self-tracker/internal/config"
)

func (b *Bot) deploymentPaused() bool {
	state, err := config.LoadDeploymentTransfer(filepath.Join(filepath.Dir(b.configPath), "deployment_transfer.json"))
	if err != nil || config.DeploymentInProgress(state) {
		return true
	}
	source, err := config.LoadSourceTransfer(filepath.Join(filepath.Dir(b.configPath), "source_transfer.json"))
	return err != nil || source != nil && (source.Phase == "reserving" || source.Phase == "preparing" || source.Phase == "prepared" || source.Phase == "stopping")
}

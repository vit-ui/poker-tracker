package storage

import (
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

type DeploymentPayload struct {
	Snapshot    Snapshot                   `json:"snapshot"`
	Settings    config.OperationalSettings `json:"settings"`
	Pending     []config.PendingTournament `json:"pending"`
	ClientClock time.Time                  `json:"client_clock"`
}

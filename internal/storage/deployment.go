package storage

import (
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
)

// DeploymentPayload is a frozen raw history plus player-owned operational state.
// Credentials, backend identity, collector folders and TLS identity are excluded.
type DeploymentPayload struct {
	Snapshot    Snapshot                   `json:"snapshot"`
	Settings    config.OperationalSettings `json:"settings"`
	Pending     []config.PendingTournament `json:"pending"`
	ClientClock time.Time                  `json:"client_clock"`
}

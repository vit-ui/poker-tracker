package client

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
	"github.com/vit-ui/self-tracker/internal/update"
)

// RunNetworkCollector keeps protection alive independently of server availability.
// Only unexpected collection failures return to the service manager. Reconnect
// attempts use the idle check cadence; no separate connectivity probe is needed.
func RunNetworkCollector(configPath string, enforcer *Enforcer, done <-chan struct{}, updater *update.Agent) error {
	return runNetworkCollector(configPath, enforcer, done, lockCheckInterval(false), updater)
}

func runNetworkCollector(configPath string, enforcer *Enforcer, done <-chan struct{}, reconnectDelay time.Duration, updater *update.Agent) error {
	enforcementDone, enforcementStopped := make(chan struct{}), make(chan struct{})
	go func() { defer close(enforcementStopped); enforcer.Run(enforcementDone) }()
	defer func() { close(enforcementDone); <-enforcementStopped }()
	for {
		select {
		case <-done:
			return nil
		default:
		}
		cfg, err := config.LoadConfig(configPath)
		var connection *transport.CollectorConnection
		if err == nil {
			if cfg.Setup.ConnectionString == nil {
				err = fmt.Errorf("collector connection string is not configured")
			} else {
				var info transport.ConnInfo
				info, err = transport.ParseConnectionString(*cfg.Setup.ConnectionString)
				if err == nil {
					attemptStop, connected, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
					go func() {
						defer close(joined)
						select {
						case <-done:
							close(attemptStop)
						case <-connected:
						}
					}()
					connection, err = transport.ConnectCollector(info, attemptStop)
					close(connected)
					<-joined
				}
			}
		}
		if err != nil {
			slog.Error("collector connection unavailable; enforcement remains active", "err", err)
		} else {
			slog.Info("collector transport connected")
			collector := NewCollector(connection.Link, enforcer, nil)
			collector.Updater = updater
			collector.Stop = done
			err = collector.Serve(configPath, connection.Done)
			if err != nil {
				connection.Close()
				return fmt.Errorf("collecting files: %w", err)
			}
			connection.Drain()
			slog.Info("collector transport disconnected; enforcement remains active")
		}
		timer := time.NewTimer(reconnectDelay)
		select {
		case <-done:
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

package client

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func TestUnavailableUpdaterReportsFailureOverBothLinks(t *testing.T) {
	for _, network := range []bool{false, true} {
		t.Run(map[bool]string{false: "channel", true: "TLS"}[network], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.json")
			if err := config.SaveConfig(path, &config.Config{}); err != nil {
				t.Fatal(err)
			}
			link, peer, dead, disconnect := collectionTestLink(t, network)
			collector := NewCollector(peer, NewEnforcer(filepath.Join(root, "lockfile.json")), nil)
			stopped := make(chan error, 1)
			go func() { stopped <- collector.Serve(path, dead) }()
			defer func() {
				disconnect()
				if err := <-stopped; err != nil {
					t.Error(err)
				}
			}()
			if up := collectionUpload(t, link); up.Status == nil {
				t.Fatal("collector did not report startup")
			}
			request := config.UpdateRequest{ID: "no-owner", Version: "v4.9"}
			link.Commands <- transport.Command{Kind: transport.CommandUpdate, RequestID: request.ID, Update: &request}
			up := collectionUpload(t, link)
			if up.UpdateReply == nil || up.UpdateReply.RequestID != request.ID || !strings.Contains(up.UpdateReply.Error, "no update request owner") || up.UpdateReply.Report.Job != nil {
				t.Fatalf("unavailable updater claimed success: %+v", up.UpdateReply)
			}
		})
	}
}

package sheets

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestIdentifiedMoneyRecoveryReconcilesLostAcknowledgement(t *testing.T) {
	fake := &fakeSheets{failWrites: true}
	client := newTestClient(t, fake)
	err := client.AppendLedgerEntry("deposit", 100, time.Unix(1000, 0), "money:lost-response")
	var writeErr *storage.WriteError
	if !errors.As(err, &writeErr) || !writeErr.Queued {
		t.Fatalf("failed write not durably queued: %v", err)
	}
	data, err := os.ReadFile(client.fallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"operation_id": "money:lost-response"`) {
		t.Fatalf("missing durable identity: %s", data)
	}
	// Script the independently observed ledger column after a remote commit with
	// a failed acknowledgement. This is a GET fixture, not a spreadsheet model.
	fake.mu.Lock()
	fake.failWrites = false
	fake.getBody = func(rng string) valueRange {
		if rng == "Bankroll Ledger!E:E" {
			return valueRange{Values: [][]interface{}{{"Session ID"}, {"money:lost-response"}}}
		}
		return valueRange{}
	}
	fake.mu.Unlock()
	requestsBefore := len(fake.calls())
	restarted := newTestClient(t, fake)
	restarted.fallbackPath = client.fallbackPath
	if err := restarted.loadFallback(); err != nil {
		t.Fatal(err)
	}
	restarted.DrainPendingWrites()
	if restarted.PendingWrites() != 0 {
		t.Fatal("verified movement remained queued")
	}
	for _, request := range fake.calls()[requestsBefore:] {
		if request.method != "GET" {
			t.Fatalf("recovery duplicated an accepted write: %+v", request)
		}
	}
	if err := restarted.AppendLedgerEntry("deposit", 100, time.Unix(1000, 0), "money:lost-response"); err != nil {
		t.Fatal(err)
	}
}

func TestMoneyFallbackReportsLocalSaveFailure(t *testing.T) {
	fake := &fakeSheets{failWrites: true}
	client := newTestClient(t, fake)
	client.fallbackPath = filepath.Join(t.TempDir(), "missing", "queue.json")
	err := client.AppendLedgerEntry("deposit", 100, time.Now(), "money:unsaved")
	var writeErr *storage.WriteError
	if !errors.As(err, &writeErr) || writeErr.Queued {
		t.Fatalf("false durable-queue claim: %v", err)
	}
}

func TestIntentionalEqualMoneyMovementsCarryDistinctIDs(t *testing.T) {
	fake := &fakeSheets{}
	client := newTestClient(t, fake)
	for _, id := range []string{"money:first", "money:second"} {
		if err := client.AppendLedgerEntry("deposit", 100, time.Now(), id); err != nil {
			t.Fatal(err)
		}
	}
	ids := make(map[string]bool)
	for _, request := range fake.calls() {
		if request.method == "POST" && strings.Contains(request.path, ":append") {
			var body valueRange
			if err := json.Unmarshal([]byte(request.body), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Values) != 1 || len(body.Values[0]) != 5 {
				t.Fatalf("ledger columns changed: %s", request.body)
			}
			ids[body.Values[0][4].(string)] = true
		}
	}
	if len(ids) != 2 || !ids["money:first"] || !ids["money:second"] {
		t.Fatalf("distinct identities missing: %v", ids)
	}
}

package sheets

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestCashCorrectionRetainsBothHistoricalIdentityFormats(t *testing.T) {
	date := time.Date(2026, 1, 2, 12, 0, 15, 0, time.UTC)
	for _, id := range storage.CashSessionIdentities(date, "owned-hand") {
		t.Run(id, func(t *testing.T) {
			fake := &fakeSheets{getBody: func(rng string) valueRange {
				switch rng {
				case "Cash Sessions!J:J", "Bankroll Ledger!E:E":
					return valueRange{Values: [][]interface{}{{"ID"}, {id}}}
				case "Bankroll Ledger!C:D":
					return valueRange{Values: [][]interface{}{{"Amount", "Running Total"}, {-1, -1}}}
				}
				return valueRange{}
			}}
			client := newTestClient(t, fake)
			if err := client.AppendCashSession(parsers.CashSession{Date: date, FirstHandID: "owned-hand", NetCents: -50, BuyInCents: 200, Hands: 2}); err != nil {
				t.Fatal(err)
			}
			cashUpdated := false
			for _, request := range fake.calls() {
				if request.method == "POST" {
					t.Fatalf("existing session appended instead of corrected: %+v", request)
				}
				if request.method == "PUT" && strings.Contains(request.path, "Cash Sessions!") {
					var body valueRange
					if err := json.Unmarshal([]byte(request.body), &body); err != nil {
						t.Fatal(err)
					}
					if body.Values[0][9] != id {
						t.Fatalf("stored identity was rewritten: %s", request.body)
					}
					cashUpdated = true
				}
			}
			if !cashUpdated {
				t.Fatal("no cash correction")
			}
		})
	}
}

package sheets

import (
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
				case "Cash Sessions!J:J":
					return valueRange{Values: [][]interface{}{{"ID"}, {id}}}
				case "Bankroll Ledger!A:E":
					return valueRange{Values: [][]interface{}{{"Date", "Type", "Amount", "Running Total", "ID"}, {"2026-01-02 12:00", "cash_session", -1, -1, id}}}
				}
				return valueRange{}
			}}
			client := newTestClient(t, fake)
			if err := client.AppendCashSession(parsers.CashSession{Date: date, FirstHandID: "owned-hand", NetCents: -50, BuyInCents: 200, Hands: 2}); err != nil {
				t.Fatal(err)
			}
			writes := historyGridWrites(t, fake.calls())
			if len(writes) != 2 || writes[0].Start.Row != 1 || writes[0].Rows[0].Values[9].Value.Text != id ||
				writes[1].Start.Row != 1 || writes[1].Rows[0].Values[0].Value.Number != -0.5 {
				t.Fatalf("incorrect legacy cash correction: %+v", writes)
			}
		})
	}
}

package sheets

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/money"
	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestSheetsTournamentResultAndLedgerCorrectionsAreAtomic(t *testing.T) {
	const id = "9900000200"
	at := time.Date(2025, 2, 10, 22, 0, 0, 0, time.UTC)
	recovered := parsers.Tournament{
		Date: at, Type: "mtt", TournamentID: id, FinishPos: 5, BuyInCents: 110, NetCents: 451,
		ResultSource: parsers.TournamentSourceConfirmed, ResultHandID: "990000020174",
	}
	official := recovered
	official.Date, official.NetCents, official.FinishPos, official.TotalPlayers = at.Add(-time.Hour), 541, 4, 18
	official.ResultSource, official.ResultHandID = parsers.TournamentSourceSummary, ""
	baseStats := []interface{}{"", "ko", id, "", "", "", "", "", "", 20.0, 9.0, 0.0, "", 174.0}
	recoveredRow := buildTournamentRows([]storage.TournamentRecord{{Tournament: recovered, PrizeWonCents: 561}})[0]
	ledgerHeader := []interface{}{"Date", "Type", "Amount", "Running Total", "Session ID"}
	cash := []interface{}{"2025-02-10 21:00", "cash_session", 1.0, 1.0, id}
	priorTournament := []interface{}{"2025-02-10 22:00", "tournament", 4.51, 5.51, id}
	deposit := []interface{}{"2025-02-10 22:30", "deposit", 2.0, 7.51, "money:deposit"}
	for _, test := range []struct {
		name             string
		result           parsers.Tournament
		tournamentRow    []interface{}
		ledger           [][]interface{}
		bountyCorrection bool
		wantNetCents     money.Cents
		wantLedgerDate   string
		wantTotal        float64
	}{
		{"complete partial Rounder", recovered, baseStats, [][]interface{}{ledgerHeader, cash}, false, 451, "2025-02-10 22:00", 5.51},
		{"reimport same result", recovered, recoveredRow, [][]interface{}{ledgerHeader, cash, priorTournament, deposit}, false, 451, "2025-02-10 22:00", 7.51},
		{"official summary corrects date and amount", official, recoveredRow, [][]interface{}{ledgerHeader, cash, priorTournament, deposit}, false, 541, "2025-02-10 21:00", 8.41},
		{"hand stats correct bounty and ledger", recovered, recoveredRow, [][]interface{}{ledgerHeader, cash, priorTournament, deposit}, true, 486, "2025-02-10 22:00", 7.86},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeSheets{getBody: func(rng string) valueRange {
				switch rng {
				case "Tournaments!C:C":
					return valueRange{Values: [][]interface{}{{"Tournament ID"}, {id}}}
				case "Tournaments!A2:N2":
					return valueRange{Values: dollarCells([][]interface{}{test.tournamentRow})}
				case "Bankroll Ledger!A:E":
					return valueRange{Values: test.ledger}
				default:
					t.Errorf("unexpected read %s", rng)
					return valueRange{}
				}
			}}
			client := newTestClient(t, fake)
			if test.bountyCorrection {
				if err := client.UpdateTournamentHandStats(id, 20, 9, 174, 35, true); err != nil {
					t.Fatal(err)
				}
			} else if netCents, err := client.AppendTournament(test.result); err != nil || netCents != test.wantNetCents {
				t.Fatalf("net = %s, %v", netCents, err)
			}
			writes := historyGridWrites(t, fake.calls())
			ledgerStart := len(writes) - 2
			if len(test.ledger) == 2 {
				ledgerStart++
				appended := writes[ledgerStart]
				cells := appended.Rows[0].Values
				if appended.Start.SheetID != 3 || appended.Start.Row != 2 || appended.Start.Column != 0 ||
					len(appended.Rows) != 1 || cells[1].Value.Text != "tournament" || cells[4].Value.Text != id ||
					cells[2].Value.Number != 4.51 || cells[3].Value.Number != 5.51 {
					t.Fatalf("invalid appended ledger row %+v", appended)
				}
			} else {
				date, tail := writes[ledgerStart], writes[ledgerStart+1]
				if date.Start.SheetID != 3 || date.Start.Row != 2 || date.Start.Column != 0 || date.Rows[0].Values[0].Value.Text != test.wantLedgerDate ||
					tail.Start.Row != 2 || tail.Start.Column != 2 || len(tail.Rows) != 2 ||
					tail.Rows[0].Values[0].Value.Number != float64(test.wantNetCents)/100 ||
					tail.Rows[1].Values[0].Value.Number != 2 || tail.Rows[1].Values[1].Value.Number != test.wantTotal {
					t.Fatalf("invalid ledger correction %+v", writes[ledgerStart:])
				}
			}
			if !test.bountyCorrection {
				result, source := writes[0], writes[2]
				if result.Start.SheetID != 2 || result.Start.Row != 1 || len(result.Rows[0].Values) != 9 ||
					source.Start.Column != 14 || source.Rows[0].Values[0].Value.Text != test.result.ResultSource ||
					source.Rows[0].Values[1].Value.Text != test.result.ResultHandID {
					t.Fatalf("result or provenance wrong %+v", writes[:ledgerStart])
				}
			}
		})
	}
}

func TestHandHistoryBeforeSummaryAppendsOnlyKnownTournamentCells(t *testing.T) {
	fake := &fakeSheets{}
	client := newTestClient(t, fake)
	if err := client.UpdateTournamentHandStats("partial-hand-history", 24, 12, 50, 125, true); err != nil {
		t.Fatal(err)
	}
	for _, call := range fake.calls() {
		if !strings.HasSuffix(call.path, "Tournaments!A1:append") {
			continue
		}
		var body valueRange
		if err := json.Unmarshal([]byte(call.body), &body); err != nil {
			t.Fatal(err)
		}
		want := [][]interface{}{{"", "ko", "partial-hand-history", "", "", "", "", "", "", 24.0, 12.0, 1.25, "", 50.0}}
		if !reflect.DeepEqual(body.Values, want) {
			t.Fatalf("partial row = %v, want %v", body.Values, want)
		}
		return
	}
	t.Fatal("missing tournament append")
}

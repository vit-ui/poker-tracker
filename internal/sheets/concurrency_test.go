package sheets

import (
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestMoneyMovementsSerializeReadAndAppend(t *testing.T) {
	firstRead := make(chan struct{})
	secondRead := make(chan struct{})
	releaseFirst := make(chan struct{})
	var reads atomic.Int32
	fake := &fakeSheets{getBody: func(rng string) valueRange {
		if rng != "Bankroll Ledger!D:D" {
			return valueRange{}
		}
		switch reads.Add(1) {
		case 1:
			close(firstRead)
			<-releaseFirst
			return valueRange{Values: [][]interface{}{{"Running Total"}, {100.0}}}
		case 2:
			close(secondRead)
			return valueRange{Values: [][]interface{}{{"Running Total"}, {101.0}}}
		default:
			return valueRange{}
		}
	}}
	c := newTestClient(t, fake)
	errors := make(chan error, 2)
	go func() { errors <- c.AppendLedgerEntry("deposit", 1, time.Unix(100, 0)) }()
	<-firstRead
	go func() { errors <- c.AppendLedgerEntry("deposit", 1, time.Unix(200, 0)) }()
	select {
	case <-secondRead:
		close(releaseFirst)
		t.Error("second mutation read the ledger before the first mutation finished")
	case <-time.After(50 * time.Millisecond):
		close(releaseFirst)
	}
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	var methods []string
	var totals []float64
	for _, request := range fake.calls() {
		methods = append(methods, request.method)
		if request.method == http.MethodPost {
			var body valueRange
			if err := json.Unmarshal([]byte(request.body), &body); err != nil {
				t.Fatal(err)
			}
			totals = append(totals, body.Values[0][3].(float64))
		}
	}
	if !reflect.DeepEqual(methods, []string{"GET", "POST", "GET", "POST"}) {
		t.Fatalf("overlapping mutation requests: %v", methods)
	}
	if !reflect.DeepEqual(totals, []float64{101, 102}) {
		t.Fatalf("submitted totals = %v, want [101 102]", totals)
	}
}

func TestNonFiniteWritesDoNotChangeSheetOrFallback(t *testing.T) {
	for _, invalid := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for _, tc := range []struct {
			name  string
			write func(*Client) error
		}{
			{"movement", func(c *Client) error { return c.AppendLedgerEntry("deposit", invalid, time.Now()) }},
			{"cash", func(c *Client) error { return c.AppendCashSession(parsers.CashSession{NetUSD: invalid}) }},
			{"tournament", func(c *Client) error {
				_, err := c.AppendTournament(parsers.Tournament{TournamentID: "T-FINITE", BuyInUSD: invalid})
				return err
			}},
			{"stats", func(c *Client) error { return c.UpdateTournamentHandStats("T-FINITE", 20, 10, 10, invalid, true) }},
			{"snapshot", func(c *Client) error {
				return c.ImportSnapshot(storage.Snapshot{Ledger: []storage.LedgerEntry{{AmountUSD: invalid}}})
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fake := &fakeSheets{}
				c := newTestClient(t, fake)
				if err := tc.write(c); err == nil {
					t.Fatal("accepted non-finite input")
				}
				if c.PendingWrites() != 0 || len(fake.calls()) != 0 {
					t.Fatal("rejected write reached Sheets or fallback")
				}
			})
		}
	}
}

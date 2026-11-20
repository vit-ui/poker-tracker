package sheets

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
	"github.com/vit-ui/self-tracker/internal/storage"
)

func TestCashHandsAndLedgerUseAtomicBatch(t *testing.T) {
	for _, test := range []struct {
		name string
		fail bool
	}{
		{"success", false},
		{"network failure queues complete input", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			batches := make(chan map[string][]map[string]json.RawMessage, 1)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "values/'Cash Sessions'"):
					io.WriteString(w, `{"values":[["Date"]]}`)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "Bankroll Ledger!A:E"):
					io.WriteString(w, `{"values":[["Date"],["2026-07-03 16:00","deposit",1,1,"money:fixture"]]}`)
				case r.Method == http.MethodGet && r.URL.Path == "/fixture":
					io.WriteString(w, `{"sheets":[{"properties":{"sheetId":0,"title":"Cash Sessions","gridProperties":{"rowCount":1,"columnCount":10}}},{"properties":{"sheetId":3,"title":"Bankroll Ledger","gridProperties":{"rowCount":2,"columnCount":5}}}]}`)
				case r.Method == http.MethodPost && r.URL.Path == "/fixture:batchUpdate":
					var body map[string][]map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					batches <- body
					if test.fail {
						http.Error(w, "offline", http.StatusServiceUnavailable)
						return
					}
					io.WriteString(w, "{}")
				default:
					t.Errorf("unexpected Sheets request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer endpoint.Close()
			client := Client{
				svc: &restService{hc: endpoint.Client(), baseURL: endpoint.URL + "/"}, spreadsheetID: "fixture",
				fallback: make(map[string]fallbackEntry), fallbackPath: t.TempDir() + "/fallback.json",
			}
			at := time.Date(2026, 7, 3, 16, 30, 21, 0, time.UTC)
			hands := []parsers.CashHand{{ID: "9000000310", At: at, NetCents: -7, RakeCents: 1, BigBlindCents: 2}}
			session, err := parsers.SessionFromCashHands("NL2", hands)
			if err != nil {
				t.Fatal(err)
			}
			written, err := client.RecordCashHands(session, hands, "fixture-live")
			if (err != nil) != test.fail {
				t.Fatalf("RecordCashHands error = %v", err)
			}
			var body map[string][]map[string]json.RawMessage
			select {
			case body = <-batches:
			default:
				t.Fatal("cash and ledger never reached a joint batch")
			}
			requests := body["requests"]
			if len(requests) != 5 || !strings.Contains(string(requests[2]["updateCells"]), "hands:1:") ||
				!strings.Contains(string(requests[4]["updateCells"]), `"numberValue":0.93`) {
				t.Fatalf("unexpected accounting batch: %v", requests)
			}
			if test.fail {
				reopened := Client{fallbackPath: client.fallbackPath}
				if err := reopened.loadFallback(); err != nil {
					t.Fatal(err)
				}
				if len(reopened.fallback) != 1 {
					t.Fatalf("fallback = %+v", reopened.fallback)
				}
				for _, queued := range reopened.fallback {
					if queued.CashHands == nil || len(queued.CashHands.Hands) != 1 || queued.CashHands.Hands[0] != hands[0] || queued.CashHands.LiveSource != "fixture-live" {
						t.Fatalf("queued cash payload = %+v", queued.CashHands)
					}
				}
			} else if written != (storage.CashWrite{New: 1}) {
				t.Fatalf("stored hands = %+v", written)
			}
		})
	}
}

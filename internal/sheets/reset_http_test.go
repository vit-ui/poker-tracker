package sheets

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClearHistoryUsesSingleAtomicBatch(t *testing.T) {
	for _, test := range []struct {
		name  string
		tabs  string
		batch bool
		fail  bool
	}{
		{"three tracker tabs", `{"sheets":[{"properties":{"sheetId":0,"title":"Cash Sessions"}},{"properties":{"sheetId":2,"title":"Tournaments"}},{"properties":{"sheetId":3,"title":"Bankroll Ledger"}},{"properties":{"sheetId":4,"title":"Other"}}]}`, true, false},
		{"missing tab", `{"sheets":[{"properties":{"sheetId":2,"title":"Tournaments"}}]}`, false, true},
		{"failed response", `{"sheets":[{"properties":{"sheetId":0,"title":"Cash Sessions"}},{"properties":{"sheetId":2,"title":"Tournaments"}},{"properties":{"sheetId":3,"title":"Bankroll Ledger"}}]}`, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			type request struct {
				UpdateCells struct {
					Range  map[string]int
					Fields string
				}
			}
			calls := make(chan []request, 2)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/values/"):
					io.WriteString(w, `{"values":[["Date"]]}`)
				case r.Method == http.MethodGet && r.URL.Path == "/active-id":
					fmt.Fprint(w, test.tabs)
				case r.Method == http.MethodPost && r.URL.Path == "/active-id:batchUpdate":
					var body struct{ Requests []request }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					calls <- body.Requests
					if test.fail {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
					} else {
						fmt.Fprint(w, "{}")
					}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := Client{svc: &restService{hc: srv.Client(), baseURL: srv.URL + "/"}, spreadsheetID: "active-id"}
			if err := client.ClearHistory(); (err != nil) != test.fail {
				t.Fatalf("ClearHistory = %v, error wanted %t", err, test.fail)
			}
			want := 0
			if test.batch {
				want = 1
			}
			if len(calls) != want {
				t.Fatalf("got %d batches, want %d", len(calls), want)
			}
			if !test.batch {
				return
			}
			requests := <-calls
			if len(requests) != 3 {
				t.Fatalf("got %d requests, want 3", len(requests))
			}
			widths := map[int]int{0: 11, 2: 16, 3: 5}
			for _, req := range requests {
				grid := req.UpdateCells.Range
				id, hasID := grid["sheetId"]
				if !hasID || req.UpdateCells.Fields != "userEnteredValue" || grid["startRowIndex"] != 1 || grid["endColumnIndex"] != widths[id] {
					t.Fatalf("unexpected history clear request: %+v", req)
				}
				delete(widths, id)
			}
		})
	}
}

package sheets

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

type fakeRequest struct {
	method string
	path   string
	body   string
}

type fakeSheets struct {
	mu         sync.Mutex
	requests   []fakeRequest
	getBody    func(rng string) valueRange
	failWrites bool
}

func (f *fakeSheets) setFailWrites(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWrites = v
}

func (f *fakeSheets) calls() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func (f *fakeSheets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{method: r.Method, path: r.URL.Path, body: string(bodyBytes)})
	fail := f.failWrites
	getBody := f.getBody
	f.mu.Unlock()

	if r.Method == http.MethodGet {
		if r.URL.Path == "/SHEET" {
			io.WriteString(w, `{"sheets":[{"properties":{"sheetId":1,"title":"Cash Sessions","gridProperties":{"rowCount":100,"columnCount":20}}},{"properties":{"sheetId":2,"title":"Tournaments","gridProperties":{"rowCount":100,"columnCount":20}}},{"properties":{"sheetId":3,"title":"Bankroll Ledger","gridProperties":{"rowCount":100,"columnCount":20}}}]}`)
			return
		}
		var out valueRange
		if getBody != nil {
			if idx := strings.Index(r.URL.Path, "/values/"); idx >= 0 {
				out = getBody(r.URL.Path[idx+len("/values/"):])
			}
		}
		if len(out.Values) == 0 && strings.HasSuffix(r.URL.Path, "Bankroll Ledger!A:E") {
			out.Values = [][]interface{}{{"Date", "Type", "Amount", "Running Total", "Session ID"}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}

	if fail {
		http.Error(w, `{"error":{"code":500,"message":"backend error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

func newTestClient(t *testing.T, fake *fakeSheets) *Client {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return &Client{
		svc:           &restService{hc: srv.Client(), baseURL: srv.URL + "/", driveURL: srv.URL + "/drive"},
		spreadsheetID: "SHEET",
		fallbackPath:  filepath.Join(t.TempDir(), "sheets_fallback.json"),
		fallback:      make(map[string]fallbackEntry),
	}
}

func TestFallbackCacheAndReplay(t *testing.T) {
	fake := &fakeSheets{}
	c := newTestClient(t, fake)

	session := parsers.CashSession{Stake: "NL10", Hands: 40, NetCents: -300, FirstHandID: "h1"}

	fake.setFailWrites(true)
	if err := c.AppendCashSession(session); err == nil {
		t.Fatal("AppendCashSession = nil error while the API is failing, want an error")
	}
	if got := c.PendingWrites(); got != 1 {
		t.Fatalf("PendingWrites after a failed write = %d, want 1 (cached for replay)", got)
	}
	if _, err := readFallbackFile(t, c.fallbackPath); err != nil {
		t.Fatalf("fallback file not written to disk: %v", err)
	}

	fake.setFailWrites(false)
	if err := c.AppendCashSession(parsers.CashSession{Stake: "NL10", Hands: 41, NetCents: -400, FirstHandID: "h2"}); err != nil {
		t.Fatalf("AppendCashSession after recovery = %v, want success", err)
	}
	if got := c.PendingWrites(); got != 0 {
		t.Errorf("PendingWrites after recovery = %d, want 0 (queued write replayed)", got)
	}
}

func TestLedgerFallbackCacheAndReplay(t *testing.T) {
	fake := &fakeSheets{}
	c := newTestClient(t, fake)

	fake.setFailWrites(true)
	if err := c.AppendLedgerEntry("deposit", 657, time.Unix(1000, 0), ""); err == nil {
		t.Fatal("AppendLedgerEntry = nil error while the API is failing, want an error")
	}
	if err := c.AppendLedgerEntry("withdrawal", -2000, time.Unix(2000, 0), ""); err == nil {
		t.Fatal("second AppendLedgerEntry = nil error while the API is failing, want an error")
	}
	if got := c.PendingWrites(); got != 2 {
		t.Fatalf("PendingWrites after two failed deposits = %d, want 2 (both queued distinctly)", got)
	}
	queued, err := readFallbackFile(t, c.fallbackPath)
	if err != nil {
		t.Fatalf("fallback file not written to disk: %v", err)
	}
	for key, entry := range queued {
		if entry.Kind != fallbackKindLedger || entry.Ledger == nil {
			t.Fatalf("queued entry %q = kind %q ledger=%v, want a ledger entry", key, entry.Kind, entry.Ledger)
		}
	}

	fake.setFailWrites(false)
	if err := c.AppendLedgerEntry("deposit", 1000, time.Unix(3000, 0), ""); err != nil {
		t.Fatalf("AppendLedgerEntry after recovery = %v, want success", err)
	}
	if got := c.PendingWrites(); got != 0 {
		t.Errorf("PendingWrites after recovery = %d, want 0 (both queued deposits replayed)", got)
	}
}

func TestHealth(t *testing.T) {
	fake := &fakeSheets{}
	c := newTestClient(t, fake)
	session := parsers.CashSession{Stake: "NL10", Hands: 40, NetCents: -300, FirstHandID: "h1"}

	if err := c.AppendCashSession(session); err != nil {
		t.Fatalf("AppendCashSession = %v, want success", err)
	}
	if down, since, herr := c.Health(); down || !since.IsZero() || herr != nil {
		t.Errorf("Health after a successful call = (down=%v, since=%v, err=%v), want (false, zero, nil)", down, since, herr)
	}

	fake.setFailWrites(true)
	before := time.Now()
	if err := c.AppendCashSession(session); err == nil {
		t.Fatal("AppendCashSession = nil error while the API is failing, want an error")
	}
	if down, since, herr := c.Health(); !down || herr == nil || since.Before(before) {
		t.Errorf("Health after a failed call = (down=%v, since=%v, err=%v), want (true, >= %v, non-nil)", down, since, herr, before)
	}

	fake.setFailWrites(false)
	if err := c.AppendCashSession(session); err != nil {
		t.Fatalf("AppendCashSession after recovery = %v, want success", err)
	}
	if down, since, herr := c.Health(); down || !since.IsZero() || herr != nil {
		t.Errorf("Health after recovery = (down=%v, since=%v, err=%v), want (false, zero, nil)", down, since, herr)
	}
}

func TestFallbackSupersededByNewerHandCount(t *testing.T) {
	fake := &fakeSheets{}
	c := newTestClient(t, fake)
	session := parsers.CashSession{Stake: "NL10", Hands: 40, NetCents: -300, FirstHandID: "h1"}

	fake.setFailWrites(true)
	if err := c.AppendCashSession(session); err == nil {
		t.Fatal("AppendCashSession = nil error while failing, want an error")
	}
	if got := c.PendingWrites(); got != 1 {
		t.Fatalf("PendingWrites = %d, want 1", got)
	}

	id := CashSessionID(session.Date, session.FirstHandID)
	fake.mu.Lock()
	fake.getBody = func(rng string) valueRange {
		switch {
		case strings.HasPrefix(rng, "Cash Sessions!J"):
			return valueRange{Values: [][]interface{}{{"Session ID"}, {id}}}
		case strings.HasPrefix(rng, "Cash Sessions!C"):
			return valueRange{Values: [][]interface{}{{50.0}}}
		}
		return valueRange{}
	}
	fake.failWrites = false
	fake.mu.Unlock()

	before := len(fake.calls())
	c.flush()
	if got := c.PendingWrites(); got != 0 {
		t.Errorf("PendingWrites after flush = %d, want 0 (stale snapshot discarded, not replayed)", got)
	}
	for _, r := range fake.calls()[before:] {
		if r.method != http.MethodGet && strings.Contains(r.path, "Cash Sessions") {
			t.Errorf("superseded entry was written to the sheet during flush: %s %s", r.method, r.path)
		}
	}
}

func TestAppendTournamentFindOrAppend(t *testing.T) {
	tourn := parsers.Tournament{
		Type: "spingo", TournamentID: "T1", BuyInCents: 100, FinishPos: 1, TotalPlayers: 3, NetCents: 200,
	}
	for _, test := range []struct {
		name     string
		ids      [][]interface{}
		rowIndex int
	}{
		{"existing row", [][]interface{}{{"Tournament ID"}, {"T0"}, {"T1"}, {"T2"}}, 2},
		{"new row", [][]interface{}{{"Tournament ID"}, {"T0"}}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeSheets{getBody: func(rng string) valueRange {
				if strings.HasPrefix(rng, "Tournaments!C") {
					return valueRange{Values: test.ids}
				}
				return valueRange{}
			}}
			c := newTestClient(t, fake)
			if _, err := c.AppendTournament(tourn); err != nil {
				t.Fatal(err)
			}
			writes := historyGridWrites(t, fake.calls())
			if len(writes) != 4 || writes[0].Start.SheetID != 2 || writes[0].Start.Row != test.rowIndex ||
				writes[3].Start.SheetID != 3 || writes[3].Rows[0].Values[2].Value.Number != 2 {
				t.Fatalf("unexpected writes %+v", writes)
			}
		})
	}
}

type testGridWrite struct {
	Start struct {
		SheetID int `json:"sheetId"`
		Row     int `json:"rowIndex"`
		Column  int `json:"columnIndex"`
	} `json:"start"`
	Rows []struct {
		Values []struct {
			Value struct {
				Number float64 `json:"numberValue"`
				Text   string  `json:"stringValue"`
			} `json:"userEnteredValue"`
		} `json:"values"`
	} `json:"rows"`
}

func historyGridWrites(t *testing.T, calls []fakeRequest) []testGridWrite {
	t.Helper()
	var writes []testGridWrite
	batches := 0
	for _, request := range calls {
		if request.method == "GET" {
			continue
		}
		if request.method != "POST" || request.path != "/SHEET:batchUpdate" {
			t.Fatalf("unexpected separate history write: %+v", request)
		}
		batches++
		var body struct {
			Requests []struct {
				Update *testGridWrite `json:"updateCells"`
			} `json:"requests"`
		}
		if err := json.Unmarshal([]byte(request.body), &body); err != nil {
			t.Fatal(err)
		}
		for _, entry := range body.Requests {
			if entry.Update != nil {
				writes = append(writes, *entry.Update)
			}
		}
	}
	if batches != 1 {
		t.Fatalf("wrote %d batches, want one", batches)
	}
	return writes
}

func TestValuesGetEscapesTabName(t *testing.T) {
	fake := &fakeSheets{}
	c := newTestClient(t, fake)
	if _, err := c.getLastRunningTotal(); err != nil {
		t.Fatalf("getLastRunningTotal: %v", err)
	}
	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("got %d requests, want 1", len(calls))
	}
	if !strings.Contains(calls[0].path, "Bankroll Ledger!D:D") {
		t.Errorf("request path = %q, want it to carry the decoded range Bankroll Ledger!D:D", calls[0].path)
	}
}

func readFallbackFile(t *testing.T, path string) (map[string]fallbackEntry, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]fallbackEntry
	return m, json.Unmarshal(data, &m)
}

package sheets

// These tests exercise the sheets-only code paths that have no mirror in the
// sqlite backend and can't be reached without HTTP: the write-ahead fallback
// (cache-on-failure / replay-on-recovery) and the raw upserts' find-or-append
// branch selection. They run the Client's real request-building, JSON
// decoding, and control flow against an httptest server standing in for the
// Google Sheets API — no real credentials, no reimplementation of spreadsheet
// semantics. The fake returns programmable GET bodies and can be flipped to
// fail every mutating call, which is all these two behaviors need; row
// encode/decode alignment and the money math are covered elsewhere (the
// migration round-trip, the pure cascade/parse tests, and the sqlite backend).

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

// fakeRequest is one recorded call to the fake Sheets API.
type fakeRequest struct {
	method string
	path   string // decoded URL path, e.g. "/SHEET/values/Cash Sessions!J:J"
	body   string
}

// fakeSheets is a scripted stand-in for the Google Sheets REST API. GETs
// return getBody(range) (empty valueRange when getBody is nil or returns the
// zero value), so findRowByColumnValue/getLastRunningTotal see "no such row"
// by default; mutating calls succeed with 200 unless failWrites is set, in
// which case they return 500 to drive the write-ahead fallback. Every request
// is recorded for assertions.
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
		var out valueRange
		if getBody != nil {
			// The range is the path segment after "/values/", e.g.
			// "Cash Sessions!J:J" — already decoded in r.URL.Path.
			if idx := strings.Index(r.URL.Path, "/values/"); idx >= 0 {
				out = getBody(r.URL.Path[idx+len("/values/"):])
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}

	// Any mutating call (POST :append/:clear/batchUpdate, PUT update).
	if fail {
		http.Error(w, `{"error":{"code":500,"message":"backend error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// newTestClient stands up the fake server and a Client wired to it with a real
// on-disk fallback file. Constructing Client directly (white-box) skips
// ensureTabs, so the fake needn't serve tab metadata just to build the client.
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

// TestFallbackCacheAndReplay covers the write-ahead fallback end to end: a
// write that fails against the API is cached to disk (PendingWrites 0→1), and
// the next successful write flushes it back (PendingWrites 1→0). This is the
// path that keeps a transient Sheets 500 from silently losing a session.
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
	// cacheFailed persists immediately so a crash never loses the write.
	if _, err := readFallbackFile(t, c.fallbackPath); err != nil {
		t.Fatalf("fallback file not written to disk: %v", err)
	}

	// The API recovers; the next successful write flushes the queued one.
	fake.setFailWrites(false)
	if err := c.AppendCashSession(parsers.CashSession{Stake: "NL10", Hands: 41, NetCents: -400, FirstHandID: "h2"}); err != nil {
		t.Fatalf("AppendCashSession after recovery = %v, want success", err)
	}
	if got := c.PendingWrites(); got != 0 {
		t.Errorf("PendingWrites after recovery = %d, want 0 (queued write replayed)", got)
	}
}

// TestLedgerFallbackCacheAndReplay covers the write-ahead fallback for a
// deposit/withdrawal: a failed AppendLedgerEntry is cached to disk (as a ledger
// kind), and the next successful write replays it — the path that stops a
// transient Sheets outage from silently losing a money movement (the /deposit
// that vanished after the scan blew the quota). Two failed deposits must queue
// as two distinct entries (ledger rows have no upsert key, so the timestamp
// keys them).
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

	// The API recovers; the next successful write flushes both queued deposits.
	fake.setFailWrites(false)
	if err := c.AppendLedgerEntry("deposit", 1000, time.Unix(3000, 0), ""); err != nil {
		t.Fatalf("AppendLedgerEntry after recovery = %v, want success", err)
	}
	if got := c.PendingWrites(); got != 0 {
		t.Errorf("PendingWrites after recovery = %d, want 0 (both queued deposits replayed)", got)
	}
}

// TestHealth covers restService.doJSON's health tracking (surfaced through
// Client.Health, the heartbeat's sheets-only data source): a successful call
// reports healthy, a failing call reports down with the error and a since
// timestamp no earlier than the call, and recovery reports healthy again with
// since reset to zero.
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

// TestFallbackSupersededByNewerHandCount covers flushDecision's supersede
// guard: a queued cash write is discarded rather than replayed when the sheet
// already holds a strictly higher hand count for the same session (a later
// direct write got there first). Replaying it would regress the row backward.
func TestFallbackSupersededByNewerHandCount(t *testing.T) {
	fake := &fakeSheets{}
	c := newTestClient(t, fake)
	session := parsers.CashSession{Stake: "NL10", Hands: 40, NetCents: -300, FirstHandID: "h1"}

	// Queue a 40-hand snapshot by failing its write.
	fake.setFailWrites(true)
	if err := c.AppendCashSession(session); err == nil {
		t.Fatal("AppendCashSession = nil error while failing, want an error")
	}
	if got := c.PendingWrites(); got != 1 {
		t.Fatalf("PendingWrites = %d, want 1", got)
	}

	// The sheet now reports a 50-hand row for this session (a later direct
	// write superseded the queued snapshot). Column J holds the session ID at
	// row 2; column C holds its hand count.
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

	before := len(fake.calls()) // ignore the failed initial write's recorded attempts
	c.flush()
	if got := c.PendingWrites(); got != 0 {
		t.Errorf("PendingWrites after flush = %d, want 0 (stale snapshot discarded, not replayed)", got)
	}
	// Discarded, not replayed: during the flush, the guard should read the
	// sheet's hand count and then write nothing to the Cash Sessions row.
	for _, r := range fake.calls()[before:] {
		if r.method != http.MethodGet && strings.Contains(r.path, "Cash Sessions") {
			t.Errorf("superseded entry was written to the sheet during flush: %s %s", r.method, r.path)
		}
	}
}

// TestAppendTournamentFindOrAppend covers the sheets-only branch selection in
// the tournament upsert: an existing row (found in the Tournament ID column)
// is corrected via batchUpdate, while a new tournament is appended. Each
// backend implements this separately, so the sqlite tests don't exercise it.
func TestAppendTournamentFindOrAppend(t *testing.T) {
	tourn := parsers.Tournament{
		Type: "spingo", TournamentID: "T1", BuyInCents: 100, FinishPos: 1, TotalPlayers: 3, NetCents: 200,
	}

	t.Run("existing row is updated, not appended", func(t *testing.T) {
		fake := &fakeSheets{
			getBody: func(rng string) valueRange {
				switch {
				case strings.HasPrefix(rng, "Tournaments!C"):
					// ID column: header, a filler row, then T1 at sheet row 3.
					return valueRange{Values: [][]interface{}{{"Tournament ID"}, {"T0"}, {"T1"}}}
				case strings.HasPrefix(rng, "Tournaments!A3"):
					// The existing row readTournamentRow reads back.
					return valueRange{Values: [][]interface{}{{"2026-07-20 21:00", "spingo", "T1", 1.0, 3.0, 3.0, 1.0, 3.0, 2.0}}}
				}
				return valueRange{}
			},
		}
		c := newTestClient(t, fake)
		if _, err := c.AppendTournament(tourn); err != nil {
			t.Fatalf("AppendTournament: %v", err)
		}
		var sawBatchUpdate bool
		for _, r := range fake.calls() {
			if strings.HasSuffix(r.path, "/values:batchUpdate") {
				sawBatchUpdate = true
			}
			if strings.Contains(r.path, "Tournaments") && strings.HasSuffix(r.path, ":append") {
				t.Errorf("existing tournament was appended, not updated: %s", r.path)
			}
		}
		if !sawBatchUpdate {
			t.Error("no batchUpdate issued for an existing tournament row")
		}
	})

	t.Run("absent row is appended", func(t *testing.T) {
		fake := &fakeSheets{} // all GETs empty → row not found
		c := newTestClient(t, fake)
		if _, err := c.AppendTournament(tourn); err != nil {
			t.Fatalf("AppendTournament: %v", err)
		}
		var sawTournamentAppend bool
		for _, r := range fake.calls() {
			if strings.Contains(r.path, "Tournaments") && strings.HasSuffix(r.path, ":append") {
				sawTournamentAppend = true
			}
		}
		if !sawTournamentAppend {
			t.Error("no append issued for a brand-new tournament")
		}
	})
}

// TestValuesGetEscapesTabName confirms a tab name with a space is
// percent-escaped in the request path — the URL construction that would
// otherwise send a malformed range for "Cash Sessions".
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
	// The server decodes the path, so a correctly-escaped "Cash%20Sessions"
	// arrives as "Bankroll Ledger!D:D" with the space intact and no raw
	// unescaped characters mangled.
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

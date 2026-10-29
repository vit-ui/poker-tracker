package sheets

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vit-ui/self-tracker/internal/parsers"
)

func TestSuccessfulReentryRetiresOlderQueuedSummary(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-process", true: "restart"}[restart], func(t *testing.T) {
			fake := &fakeSheets{failWrites: true}
			client := newTestClient(t, fake)
			summary := parsers.Tournament{Date: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC), Type: "spingo", TournamentID: "T-REENTRY", BuyInUSD: 1.1, NetUSD: -1.1, FinishPos: 3, TotalPlayers: 3}
			if _, err := client.AppendTournament(summary); err == nil {
				t.Fatal("expected outage")
			}
			if restart {
				next := newTestClient(t, fake)
				next.fallbackPath = client.fallbackPath
				if err := next.loadFallback(); err != nil {
					t.Fatal(err)
				}
				client = next
			}
			fake.setFailWrites(false)
			start := len(fake.calls())
			summary.BuyInUSD = 2.2
			summary.NetUSD = -2.2
			if _, err := client.AppendTournament(summary); err != nil {
				t.Fatal(err)
			}
			client.DrainPendingWrites()
			if client.PendingWrites() != 0 {
				t.Fatal("superseded summary still queued")
			}
			rows := 0
			for _, request := range fake.calls()[start:] {
				if request.method == "POST" && strings.Contains(request.path, "Tournaments!") {
					var body valueRange
					if err := json.Unmarshal([]byte(request.body), &body); err != nil {
						t.Fatal(err)
					}
					if body.Values[0][3] != 2.2 {
						t.Fatalf("stale buy-in replayed: %s", request.body)
					}
					rows++
				}
			}
			if rows != 1 {
				t.Fatalf("wrote %d summaries; want only newest", rows)
			}
		})
	}
}

func TestSameHandCountCorrectionRetiresQueuedStatistics(t *testing.T) {
	fake := &fakeSheets{failWrites: true}
	client := newTestClient(t, fake)
	if err := client.UpdateTournamentHandStats("T-STATS", 20, 10, 10, 1, true); err == nil {
		t.Fatal("expected outage")
	}
	fake.setFailWrites(false)
	start := len(fake.calls())
	if err := client.UpdateTournamentHandStats("T-STATS", 30, 20, 10, 2, true); err != nil {
		t.Fatal(err)
	}
	if client.PendingWrites() != 0 {
		t.Fatal("same-hand-count correction did not supersede queued data")
	}
	for _, request := range fake.calls()[start:] {
		if request.method == "POST" && strings.Contains(request.path, "Tournaments!") {
			var body valueRange
			if err := json.Unmarshal([]byte(request.body), &body); err != nil {
				t.Fatal(err)
			}
			if body.Values[0][11] != float64(2) {
				t.Fatalf("old bounty replayed: %s", request.body)
			}
		}
	}
}

func TestSameHandCountCashCorrectionRetiresQueuedSnapshot(t *testing.T) {
	fake := &fakeSheets{failWrites: true}
	client := newTestClient(t, fake)
	session := parsers.CashSession{Date: time.Now(), FirstHandID: "cash-correction", Hands: 10, NetUSD: 1, BuyInUSD: 2}
	if err := client.AppendCashSession(session); err == nil {
		t.Fatal("expected outage")
	}
	fake.setFailWrites(false)
	start := len(fake.calls())
	session.NetUSD = 2
	if err := client.AppendCashSession(session); err != nil {
		t.Fatal(err)
	}
	if client.PendingWrites() != 0 {
		t.Fatal("corrected cash still queued")
	}
	rows := 0
	for _, request := range fake.calls()[start:] {
		if request.method == "POST" && strings.Contains(request.path, "Cash Sessions!") {
			var body valueRange
			if err := json.Unmarshal([]byte(request.body), &body); err != nil {
				t.Fatal(err)
			}
			if body.Values[0][4] != float64(2) {
				t.Fatalf("stale cash replayed: %s", request.body)
			}
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("wrote %d cash snapshots, want newest once", rows)
	}
}

func TestSupersessionPersistsNewestBeforeRemoteCorrection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions failure injection")
	}
	fake := &fakeSheets{failWrites: true}
	client := newTestClient(t, fake)
	summary := parsers.Tournament{Date: time.Now(), Type: "mtt", TournamentID: "T-DURABLE", BuyInUSD: 1.1, NetUSD: -1.1}
	if _, err := client.AppendTournament(summary); err == nil {
		t.Fatal("expected outage")
	}
	directory := filepath.Dir(client.fallbackPath)
	defer os.Chmod(directory, 0700)
	fake.setFailWrites(false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			if err := os.Chmod(directory, 0500); err != nil {
				t.Error(err)
			}
		}
		fake.ServeHTTP(w, r)
	}))
	defer server.Close()
	client.svc.baseURL = server.URL + "/"
	client.svc.hc = server.Client()
	summary.BuyInUSD = 2.2
	summary.NetUSD = -2.2
	if _, err := client.AppendTournament(summary); err == nil {
		t.Fatal("retirement persistence failure was hidden")
	}
	if client.PendingWrites() != 1 {
		t.Fatal("failed retirement discarded durable pending entry")
	}
	data, err := os.ReadFile(client.fallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	var queue map[string]fallbackEntry
	if err := json.Unmarshal(data, &queue); err != nil {
		t.Fatal(err)
	}
	for _, entry := range queue {
		if entry.Summary == nil || entry.Summary.BuyInUSD != 2.2 {
			t.Fatalf("restart could replay old summary: %s", data)
		}
	}
}

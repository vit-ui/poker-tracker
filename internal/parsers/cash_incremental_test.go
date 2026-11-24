package parsers

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseCashFileFromWaitsForHandSummary(t *testing.T) {
	const hero = "AuditHero"
	const actions = "PokerStars Hand #300000000001: Hold'em No Limit ($0.01/$0.02 USD) - 2026/07/13 15:00:00 ET\n" +
		"*** HOLE CARDS ***\nDealt to AuditHero [Ad Kd]\nAuditHero: bets $0.25\n"
	const payout = "AuditHero collected $0.53 from pot\n*** SUMMARY ***\nTotal pot $0.53 | Rake $0\n"
	path := filepath.Join(t.TempDir(), "live.txt")
	var prior *ParseState
	for _, tc := range []struct {
		name     string
		content  string
		wantHand bool
	}{
		{"during betting", actions, false},
		{"with payout and summary", actions + payout, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			session, _, state, err := ParseCashFileFrom(path, hero, prior)
			if err != nil || state == nil {
				t.Fatalf("parse state = %v, error = %v", state, err)
			}
			prior = state
			if !tc.wantHand && session != nil {
				t.Fatalf("incomplete hand counted as %+v", session)
			}
			if tc.wantHand && (session == nil || session.Hands != 1 || session.NetCents != 28) {
				t.Fatalf("completed session = %+v, want one hand and 28 cents", session)
			}
		})
	}
}

func handHeaderOffsets(data []byte) []int64 {
	var offs []int64
	var off int64
	rest := data
	for len(rest) > 0 {
		var raw []byte
		if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
			raw = rest[:idx+1]
			rest = rest[idx+1:]
		} else {
			raw = rest
			rest = nil
		}
		line := trimLineEnding(string(raw))
		if len(line) >= 3 && line[0] == '\xef' && line[1] == '\xbb' && line[2] == '\xbf' {
			line = line[3:]
		}
		if isHandHeader(line) {
			offs = append(offs, off)
		}
		off += int64(len(raw))
	}
	return offs
}

func assertSameResult(t *testing.T, label string, gotS *CashSession, gotT *TournamentHandStats, gotErr error, wantS *CashSession, wantT *TournamentHandStats, wantErr error) {
	t.Helper()
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%s: error mismatch: incremental=%v whole=%v", label, gotErr, wantErr)
	}
	if !reflect.DeepEqual(gotS, wantS) {
		t.Fatalf("%s: cash session mismatch:\n incremental=%+v\n whole      =%+v", label, gotS, wantS)
	}
	if !reflect.DeepEqual(gotT, wantT) {
		t.Fatalf("%s: tournament stats mismatch:\n incremental=%+v\n whole      =%+v", label, gotT, wantT)
	}
}

func TestParseCashFileFrom_Equivalence(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "hand_history", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures in testdata/hand_history/")
	}

	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			offs := handHeaderOffsets(data)
			if len(offs) == 0 {
				t.Fatalf("no hand headers found in %s", filepath.Base(f))
			}

			var ends []int64
			ends = append(ends, offs[1:]...)
			ends = append(ends, int64(len(data)))

			dir := t.TempDir()
			growing := filepath.Join(dir, "growing.txt")
			sawResume := false

			var prior *ParseState
			for i, end := range ends {
				if err := os.WriteFile(growing, data[:end], 0o644); err != nil {
					t.Fatal(err)
				}
				if prior != nil && prior.consumedOffset > 0 {
					sawResume = true
				}

				gotS, gotT, st, gotErr := ParseCashFileFrom(growing, testScreenName, prior)

				whole := filepath.Join(dir, "whole.txt")
				if err := os.WriteFile(whole, data[:end], 0o644); err != nil {
					t.Fatal(err)
				}
				wantS, wantT, wantErr := ParseCashFile(whole, testScreenName)

				assertSameResult(t, fmt.Sprintf("prefix #%d (end=%d)", i, end), gotS, gotT, gotErr, wantS, wantT, wantErr)
				prior = st
			}

			if len(offs) > 1 && !sawResume {
				t.Fatalf("%s: multi-hand fixture never exercised the resume path", filepath.Base(f))
			}
		})
	}
}

func TestParseCashFileFrom_FallbackShorterFile(t *testing.T) {
	full := readFixtureWithMultipleHands(t)
	offs := handHeaderOffsets(full)
	if len(offs) < 2 {
		t.Fatal("need a multi-hand fixture")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "hh.txt")

	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, prior, err := ParseCashFileFrom(path, testScreenName, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prior.consumedOffset == 0 {
		t.Fatal("expected a non-zero consumedOffset for a multi-hand file")
	}

	shorter := full[:offs[1]]
	if err := os.WriteFile(path, shorter, 0o644); err != nil {
		t.Fatal(err)
	}
	gotS, gotT, _, gotErr := ParseCashFileFrom(path, testScreenName, prior)

	whole := filepath.Join(dir, "whole.txt")
	if err := os.WriteFile(whole, shorter, 0o644); err != nil {
		t.Fatal(err)
	}
	wantS, wantT, wantErr := ParseCashFile(whole, testScreenName)
	assertSameResult(t, "shorter-file fallback", gotS, gotT, gotErr, wantS, wantT, wantErr)
}

func TestParseCashFileFrom_FallbackRewrittenContent(t *testing.T) {
	full := readFixtureWithMultipleHands(t)
	offs := handHeaderOffsets(full)
	if len(offs) < 2 {
		t.Fatal("need a multi-hand fixture")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "hh.txt")

	if err := os.WriteFile(path, full[:offs[1]], 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, prior, err := ParseCashFileFrom(path, testScreenName, nil)
	if err != nil {
		t.Fatal(err)
	}

	rewritten := append([]byte("PokerStars Hand #999: extra padding line to shift offsets\n"), full...)
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatal(err)
	}
	gotS, gotT, _, gotErr := ParseCashFileFrom(path, testScreenName, prior)

	whole := filepath.Join(dir, "whole.txt")
	if err := os.WriteFile(whole, rewritten, 0o644); err != nil {
		t.Fatal(err)
	}
	wantS, wantT, wantErr := ParseCashFile(whole, testScreenName)
	assertSameResult(t, "rewritten-content fallback", gotS, gotT, gotErr, wantS, wantT, wantErr)
}

func TestParseCashFileFrom_EquivalenceDataFiles(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "data", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no files in data/ — gitignored raw real fixtures, only present on the dev machine")
	}

	checked := 0
	for _, f := range files {
		base := filepath.Base(f)
		line := firstLine(t, f)
		if !strings.HasPrefix(line, "PokerStars Hand #") && !strings.HasPrefix(line, "PokerStars Zoom Hand #") {
			continue
		}
		hero := detectHero(t, f)
		if hero == "" {
			t.Errorf("%s: could not detect hero", base)
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		offs := handHeaderOffsets(data)
		if len(offs) == 0 {
			continue
		}
		ends := sampledEnds(offs, int64(len(data)), 40)

		dir := t.TempDir()
		growing := filepath.Join(dir, "growing.txt")
		var prior *ParseState
		for _, end := range ends {
			if err := os.WriteFile(growing, data[:end], 0o644); err != nil {
				t.Fatal(err)
			}
			gotS, gotT, st, gotErr := ParseCashFileFrom(growing, hero, prior)

			whole := filepath.Join(dir, "whole.txt")
			if err := os.WriteFile(whole, data[:end], 0o644); err != nil {
				t.Fatal(err)
			}
			wantS, wantT, wantErr := ParseCashFile(whole, hero)
			assertSameResult(t, fmt.Sprintf("%s @%d", base, end), gotS, gotT, gotErr, wantS, wantT, wantErr)
			prior = st
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no hand-history files in data/ (only summaries)")
	}
	t.Logf("incremental equivalence verified on %d real hand-history file(s)", checked)
}

func sampledEnds(offs []int64, total int64, max int) []int64 {
	var boundaries []int64
	boundaries = append(boundaries, offs[1:]...)
	boundaries = append(boundaries, total)
	if len(boundaries) <= max {
		return boundaries
	}
	step := len(boundaries) / (max - 1)
	if step < 1 {
		step = 1
	}
	var out []int64
	for i := 0; i < len(boundaries)-1; i += step {
		out = append(out, boundaries[i])
	}
	return append(out, total)
}

func readFixtureWithMultipleHands(t *testing.T) []byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "hand_history", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(handHeaderOffsets(data)) >= 2 && !strings.Contains(filepath.Base(f), "does-not-exist") {
			return data
		}
	}
	t.Fatal("no multi-hand fixture found in testdata/hand_history/")
	return nil
}

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

// handHeaderOffsets returns the byte offset of every hand-header line in data,
// so a test can truncate the file at hand boundaries (right before a header)
// and get a prefix that holds only complete hands.
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

// assertSameResult fails if the incremental and whole-file parses disagree on
// the session, the tournament stats, or whether an error occurred.
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

// TestParseCashFileFrom_Equivalence is the load-bearing test for the
// incremental fast path: for every committed hand-history fixture, growing the
// file one hand-boundary at a time and carrying ParseState forward must yield,
// at every step, exactly what a from-scratch whole-file parse of that same
// prefix produces. Both the intermediate steps and the final full file are
// checked, so a mid-stream divergence can't hide behind a correct end state.
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

			// Prefix ends: right before each header after the first (i.e. 1
			// hand, 2 hands, …), then the whole file. This exercises the
			// resume path only when there is more than one hand.
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

				// Whole-file parse of the identical prefix, from a fresh file.
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

// TestParseCashFileFrom_FallbackShorterFile confirms that when the file shrinks
// below a carried consumedOffset (rewritten to fewer hands), the parser
// discards the stale state and re-parses from zero, matching a whole-file parse
// of the new, shorter content.
func TestParseCashFileFrom_FallbackShorterFile(t *testing.T) {
	full := readFixtureWithMultipleHands(t)
	offs := handHeaderOffsets(full)
	if len(offs) < 2 {
		t.Fatal("need a multi-hand fixture")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "hh.txt")

	// Parse the full file to get a state whose consumedOffset points deep in.
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

	// Now shrink the file to a single hand and re-parse carrying the stale
	// (too-large) state. It must fall back and match a fresh whole-file parse.
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

// TestParseCashFileFrom_FallbackRewrittenContent confirms that when the byte at
// a carried consumedOffset is no longer a hand header (the file was rewritten in
// place, not appended to), the parser falls back to a full parse.
func TestParseCashFileFrom_FallbackRewrittenContent(t *testing.T) {
	full := readFixtureWithMultipleHands(t)
	offs := handHeaderOffsets(full)
	if len(offs) < 2 {
		t.Fatal("need a multi-hand fixture")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "hh.txt")

	// A state pointing at the second hand's header.
	if err := os.WriteFile(path, full[:offs[1]], 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, prior, err := ParseCashFileFrom(path, testScreenName, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the file with different, still-valid content whose bytes at the
	// old consumedOffset are not a header. Carrying the stale state must fall
	// back and match a fresh parse of the new content.
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

// TestParseCashFileFrom_EquivalenceDataFiles is the dev-machine-only counterpart
// of TestParseCashFileFrom_Equivalence: it runs the same growing-prefix
// equivalence over the real, larger hand histories in the gitignored data/ dir
// (HORSE, PLO, high-volume cash — the formats and sizes the small committed
// fixtures don't reach). Skips when data/ is absent (CI, fresh checkout). Each
// file is parsed for its own auto-detected hero, so no real nick enters source.
// Boundaries are sampled (the whole-file comparison is O(hands) per step) while
// always including the full file, so resume is exercised repeatedly and the
// final state is checked exactly.
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
			continue // tournament summaries aren't parsed incrementally
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

// sampledEnds returns up to max prefix ends (byte offsets just before a hand
// header, plus the full length), evenly sampled across the file's hand
// boundaries. It always includes the final full-file length so the end state is
// checked exactly.
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
	return append(out, total) // always end on the full file
}

// readFixtureWithMultipleHands returns the bytes of a committed cash fixture
// known to contain more than one hand, for the fallback tests.
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

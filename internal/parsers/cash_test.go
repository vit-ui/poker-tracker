package parsers

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testScreenName matches the anonymized player in the testdata/ files.
const testScreenName = "AnonHero01"

func isCashGameFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return false
	}
	line := scanner.Text()
	if len(line) >= 3 && line[0] == '\xef' && line[1] == '\xbb' && line[2] == '\xbf' {
		line = line[3:]
	}
	return strings.HasPrefix(line, "PokerStars Hand #") && !strings.Contains(line, "Tournament #")
}

// TestParseCashFile parses every committed hand-history fixture in
// testdata/hand_history/ (anonymized, derived from real files) and asserts each
// yields exactly one of a cash session or tournament-hand stats — and that both
// kinds are present, since the folder holds cash hands and tournament hands
// alike (which is why "cash" was the wrong old name for it).
func TestParseCashFile(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "hand_history", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures in testdata/hand_history/ — committed anonymized hand histories are expected here")
	}
	cashParsed := 0
	tourneyParsed := 0
	for _, f := range files {
		session, tourneyStats, err := ParseCashFile(f, testScreenName)
		if err != nil {
			t.Errorf("ParseCashFile(%s): %v", filepath.Base(f), err)
			continue
		}
		if session != nil {
			t.Logf("cash %s: %+v", filepath.Base(f), *session)
			cashParsed++
		} else if tourneyStats != nil {
			t.Logf("tourney %s: %+v", filepath.Base(f), *tourneyStats)
			tourneyParsed++
		} else {
			t.Errorf("ParseCashFile(%s): both session and tourneyStats are nil", filepath.Base(f))
		}
	}
	if cashParsed == 0 {
		t.Fatal("no cash game hand histories found in testdata/hand_history/")
	}
	if tourneyParsed == 0 {
		t.Fatal("no tournament hand histories found in testdata/hand_history/")
	}
}

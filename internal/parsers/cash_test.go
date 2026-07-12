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

func TestParseCashFile(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "cash", "*.txt"))
	if err != nil {
		t.Fatal(err)
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
		t.Fatal("no cash game files found in testdata/cash/")
	}
	if tourneyParsed == 0 {
		t.Fatal("no tournament hand history files found in testdata/cash/")
	}
}

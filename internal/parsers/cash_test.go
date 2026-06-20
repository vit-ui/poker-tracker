package parsers

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testScreenName matches the anonymized player in the testdata/ files.
const testScreenName = "Mayor2003"

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

func TestParseCashSession(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "cash", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	parsed := 0
	for _, f := range files {
		if !isCashGameFile(f) {
			continue
		}
		session, err := ParseCashSession(f, testScreenName)
		if err != nil {
			t.Errorf("ParseCashSession(%s): %v", filepath.Base(f), err)
			continue
		}
		t.Logf("%s: %+v", filepath.Base(f), session)
		parsed++
	}
	if parsed == 0 {
		t.Fatal("no cash game files found in testdata/cash/")
	}
}

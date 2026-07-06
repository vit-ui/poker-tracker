package parsers

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestParseTournamentFile(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "tournaments", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files found in testdata/tournaments/")
	}
	for _, f := range files {
		tournament, err := ParseTournamentFile(f, testScreenName)
		if errors.Is(err, ErrFreeroll) {
			continue
		}
		if err != nil {
			t.Errorf("ParseTournamentFile(%s): %v", filepath.Base(f), err)
			continue
		}
		t.Logf("%s: %+v", filepath.Base(f), tournament)
	}
}

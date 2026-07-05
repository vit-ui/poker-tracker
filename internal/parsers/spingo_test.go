package parsers

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestParseSpinGo(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "spingo", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files found in testdata/spingo/")
	}
	for _, f := range files {
		game, err := ParseSpinGo(f, testScreenName)
		if errors.Is(err, ErrFreeroll) || errors.Is(err, ErrNotSpinGo) {
			continue
		}
		if err != nil {
			t.Errorf("ParseSpinGo(%s): %v", filepath.Base(f), err)
			continue
		}
		t.Logf("%s: %+v", filepath.Base(f), game)
	}
}

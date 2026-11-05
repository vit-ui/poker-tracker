package sheets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTransferReadinessInspectsFallbackWithoutReplay(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
		reject     bool
	}{{"empty", `{}`, 0, false}, {"null", `null`, 0, false}, {"queued", `{"money:owned":{"kind":"ledger"}}`, 1, true}, {"corrupt", `{`, 0, true}, {"wrong shape", `[]`, 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fallback.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			c := &Client{fallbackPath: path, fallback: make(map[string]fallbackEntry)}
			err := c.CheckTransferReady()
			if (err != nil) != tc.reject {
				t.Fatalf("error=%v reject=%v", err, tc.reject)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != tc.body {
				t.Fatalf("readiness changed pending writes: %s %v", after, err)
			}
		})
	}
	if n, err := PendingFallbackCount(filepath.Join(t.TempDir(), "missing")); err != nil || n != 0 {
		t.Fatalf("missing queue count=%d %v", n, err)
	}
}

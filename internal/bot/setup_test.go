package bot

// Tests for setup.go's offline-testable pieces. Moved verbatim from the
// former bot_calc_test.go so test files mirror the package's
// file-per-concern split; the setup conversation itself is a live Telegram
// interaction and stays manually verified (see CLAUDE.md's testing rules).

import "testing"

// TestResolveSheetInput_URLBranch exercises only the URL branch of
// resolveSheetInput, which returns before ever touching serviceAccountPath
// or the network — the name-lookup branch calls sheets.ResolveSheetID (a
// live Drive API call) and is not testable without real credentials.
func TestResolveSheetInput_URLBranch(t *testing.T) {
	name, id, err := resolveSheetInput("/nonexistent/credentials.json", "https://docs.google.com/spreadsheets/d/1AbC-23_xyz/edit#gid=0")
	if err != nil {
		t.Fatalf("resolveSheetInput: %v", err)
	}
	if name != nil {
		t.Errorf("name = %v, want nil for a URL input (nothing to save as a resolved name)", *name)
	}
	if id != "1AbC-23_xyz" {
		t.Errorf("id = %q, want 1AbC-23_xyz", id)
	}
}

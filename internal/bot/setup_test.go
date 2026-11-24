package bot

import "testing"

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

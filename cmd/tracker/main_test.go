package main

import "testing"

// TestUTF16File confirms the schtasks-required encoding: a little-endian
// BOM followed by each rune as a little-endian 16-bit code unit. schtasks
// /Create /XML silently rejects a plain UTF-8 file, so this encoding is
// load-bearing for --setup's Task Scheduler registration even though it's
// never exercised on this (non-Windows) test machine.
func TestUTF16File(t *testing.T) {
	got := utf16File("Ab")
	want := []byte{0xFF, 0xFE, 'A', 0x00, 'b', 0x00}
	if len(got) != len(want) {
		t.Fatalf("utf16File(%q) length = %d, want %d (%v)", "Ab", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("utf16File(%q)[%d] = %#x, want %#x", "Ab", i, got[i], want[i])
		}
	}

	t.Run("empty string is just the BOM", func(t *testing.T) {
		if got := utf16File(""); len(got) != 2 || got[0] != 0xFF || got[1] != 0xFE {
			t.Errorf("utf16File(\"\") = %v, want the 2-byte BOM only", got)
		}
	})

	t.Run("code point above the ASCII range keeps little-endian byte order", func(t *testing.T) {
		// U+00E9 (é) — high byte non-zero, so byte order is observable.
		got := utf16File("é")
		want := []byte{0xFF, 0xFE, 0xE9, 0x00}
		if len(got) != len(want) {
			t.Fatalf("utf16File(\"é\") = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("utf16File(\"é\")[%d] = %#x, want %#x", i, got[i], want[i])
			}
		}
	})
}

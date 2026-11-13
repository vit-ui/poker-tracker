package update

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxPackageSelectsOnlyRegularInstaller(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entry   bool
		kind    byte
		invalid bool
	}{
		{"regular", true, tar.TypeReg, false}, {"missing", false, tar.TypeReg, true},
		{"symlink", true, tar.TypeSymlink, true}, {"directory", true, tar.TypeDir, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "package.tar.gz")
			file, err := os.Create(archive)
			if err != nil {
				t.Fatal(err)
			}
			gz := gzip.NewWriter(file)
			writer := tar.NewWriter(gz)
			// A traversal entry must never be materialized while looking for the installer.
			if err := writer.WriteHeader(&tar.Header{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0600, Size: 6}); err != nil {
				t.Fatal(err)
			}
			writer.Write([]byte("unsafe"))
			if tc.entry {
				h := &tar.Header{Name: "PokerTracker-Server-linux", Typeflag: tc.kind, Mode: 0700, Linkname: "../outside"}
				if tc.kind == tar.TypeReg {
					h.Size = 9
				}
				if err := writer.WriteHeader(h); err != nil {
					t.Fatal(err)
				}
				if tc.kind == tar.TypeReg {
					writer.Write([]byte("installer"))
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "selected")
			if err := unpackLinuxInstaller(archive, output); (err != nil) != tc.invalid {
				t.Fatalf("invalid=%v err=%v", tc.invalid, err)
			}
			if !tc.invalid {
				if data, err := os.ReadFile(output); err != nil || string(data) != "installer" {
					t.Fatalf("selected=%s err=%v", data, err)
				}
			}
		})
	}
}

package update

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerAssetNames(t *testing.T) {
	for _, tc := range []struct{ version, goos, want string }{
		{"v4.10", "windows", "PokerTracker-Setup-4.10.exe"},
		{"v4.10.1", "windows", "PokerTracker-Setup-4.10.1.exe"},
		{"v4.10", "linux", "PokerTracker-Server-linux.tar.gz"},
		{"dev", "windows", ""}, {"v4.10-rc1", "linux", ""}, {"v4.10", "darwin", ""},
	} {
		t.Run(tc.version+tc.goos, func(t *testing.T) {
			got, err := packageName(tc.version, tc.goos)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("package=%q err=%v", got, err)
			}
		})
	}
}

func TestInstallerDigestIsRequiredBeforeDownloading(t *testing.T) {
	for _, digest := range []string{"", "md5:" + strings.Repeat("0", 32), "sha256:short", "sha256:" + strings.Repeat("x", 64)} {
		t.Run(digest, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.Write([]byte("unsafe")) }))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "package")
			err := downloadPackage(server.Client(), releaseAsset{Name: "installer", URL: server.URL, Size: 6, Digest: digest}, path)
			if err == nil || requests != 0 {
				t.Fatalf("invalid digest downloaded or accepted: %v requests=%d", err, requests)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid package left on disk: %v", err)
			}
		})
	}
}

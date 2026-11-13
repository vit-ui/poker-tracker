package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseVersionComparison(t *testing.T) {
	for _, tc := range []struct {
		latest, current string
		newer, invalid  bool
	}{
		{"v4.8", "v4.7", true, false}, {"v4.10", "v4.9", true, false}, {"v4.7.1", "v4.7", true, false},
		{"v4.7", "v4.7.0", false, false}, {"v4.6", "v4.7", false, false}, {"v4.8", "dev", false, true},
		{"v4.8-rc1", "v4.7", false, true}, {"v4", "v4.7", false, true}, {"v4.+8", "v4.7", false, true},
	} {
		t.Run(tc.latest+"/"+tc.current, func(t *testing.T) {
			got, err := newer(tc.latest, tc.current)
			if got != tc.newer || (err != nil) != tc.invalid {
				t.Fatalf("newer=%v err=%v", got, err)
			}
		})
	}
}
func TestRolePayloadNames(t *testing.T) {
	for _, tc := range []struct{ role, goos, arch, want string }{
		{"combined", "windows", "amd64", "tracker-amd64.exe"}, {"combined", "windows", "arm64", "tracker-arm64.exe"},
		{"client", "windows", "amd64", "tracker-client-amd64.exe"}, {"client", "windows", "arm64", "tracker-client-arm64.exe"},
		{"server", "windows", "amd64", "tracker-server-windows-amd64.exe"}, {"server", "windows", "arm64", "tracker-server-windows-arm64.exe"},
		{"server", "linux", "amd64", "tracker-server-amd64"}, {"server", "linux", "arm64", "tracker-server-arm64"},
		{"client", "linux", "amd64", ""}, {"server", "linux", "386", ""},
	} {
		t.Run(tc.role+tc.goos+tc.arch, func(t *testing.T) {
			got, err := payloadName(tc.role, tc.goos, tc.arch)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("asset=%s err=%v", got, err)
			}
		})
	}
}
func TestDownloadStagesOnlyVerifiedBytes(t *testing.T) {
	for _, tc := range []struct {
		name                                               string
		badHash, short, missing, downloadError, prerelease bool
	}{
		{name: "verified"}, {name: "checksum mismatch", badHash: true}, {name: "truncated", short: true},
		{name: "missing executable", missing: true}, {name: "download failure", downloadError: true}, {name: "prerelease", prerelease: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte("release executable bytes")
			sum := sha256.Sum256(payload)
			hash := hex.EncodeToString(sum[:])
			if tc.badHash {
				hash = strings.Repeat("0", 64)
			}
			var value release
			mux := http.NewServeMux()
			mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(value) })
			mux.HandleFunc("/binary", func(w http.ResponseWriter, r *http.Request) {
				if tc.downloadError {
					w.WriteHeader(503)
					return
				}
				body := payload
				if tc.short {
					body = body[:3]
				}
				w.Write(body)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			value = release{Tag: "v4.8", Prerelease: tc.prerelease}
			if !tc.missing {
				value.Assets = append(value.Assets, releaseAsset{Name: "PokerTracker-Server-linux.tar.gz", URL: server.URL + "/binary", Size: int64(len(payload)), Digest: "sha256:" + hash})
			}
			got, err := latest(server.Client(), server.URL+"/latest")
			if tc.prerelease {
				if err == nil {
					t.Fatal("accepted prerelease")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			dataPath := filepath.Join(dir, "config.json")
			os.WriteFile(dataPath, []byte("preserve"), 0600)
			staged := filepath.Join(dir, "package")
			asset, err := findAsset(got, "PokerTracker-Server-linux.tar.gz")
			if err == nil {
				err = downloadPackage(server.Client(), asset, staged)
			}
			wantErr := tc.badHash || tc.short || tc.missing || tc.downloadError
			if (err != nil) != wantErr {
				t.Fatalf("stage=%s err=%v", staged, err)
			}
			files, _ := os.ReadDir(dir)
			if wantErr && len(files) != 1 {
				t.Fatal("failed download left staged bytes")
			}
			if !wantErr {
				bytes, err := os.ReadFile(staged)
				if err != nil || string(bytes) != string(payload) {
					t.Fatal("staged bytes differ")
				}
			}
			bytes, _ := os.ReadFile(dataPath)
			if string(bytes) != "preserve" {
				t.Fatal("download changed settings")
			}
		})
	}
}
func TestUpdateRejectsOtherActions(t *testing.T) {
	for _, args := range [][]string{{"--update", "--serve"}, {"--version", "--update"}, {"--update", "extra"}} {
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		options := Register(flags)
		flags.Bool("serve", false, "")
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		handled, err := options.Handle(flags, "server", "v4.7", "")
		if !handled || err == nil {
			t.Fatal("conflicting action accepted")
		}
	}
}

// Package update replaces only the local role from a stable GitHub release.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/logging"
)

const latestURL = "https://api.github.com/repos/vit-ui/poker-tracker/releases/latest"

type Options struct {
	update, version               bool
	target, output, recoverTarget string
	requestID                     string
	parent                        int
}

func Register(flags *flag.FlagSet) *Options {
	options := new(Options)
	flags.BoolVar(&options.update, "update", false, "update this installation to the latest stable GitHub release")
	flags.BoolVar(&options.version, "version", false, "print this executable's release version")
	flags.StringVar(&options.recoverTarget, "recover-update", "", "internal interrupted-update recovery target")
	flags.StringVar(&options.target, "update-target", "", "internal update helper target")
	flags.StringVar(&options.output, "update-output", "", "internal update helper output")
	flags.StringVar(&options.requestID, "update-request", "", "internal durable update request identity")
	flags.IntVar(&options.parent, "update-parent", 0, "internal update caller PID")
	return options
}

// Handle runs before setup/serve so updating never loads another installation's config.
func (options *Options) Handle(flags *flag.FlagSet, role, version, installationID string) (bool, error) {
	if options.recoverTarget != "" {
		if runtime.GOOS != "windows" {
			return true, fmt.Errorf("interrupted-update recovery is Windows-only")
		}
		count := 0
		flags.Visit(func(*flag.Flag) { count++ })
		if count != 1 || flags.NArg() != 0 || !filepath.IsAbs(options.recoverTarget) {
			return true, fmt.Errorf("--recover-update requires one absolute target and no other actions")
		}
		closeLog := logging.Configure(filepath.Join(filepath.Dir(options.recoverTarget), "tracker.log"))
		defer closeLog()
		_, err := recoverPendingUpdate(options.recoverTarget, role, installationID, 0)
		if err != nil {
			slog.Error("recovering interrupted update", "err", err)
		}
		return true, err
	}
	if options.target == "" && (options.parent != 0 || options.output != "" || options.requestID != "") {
		return true, fmt.Errorf("internal update arguments require a helper target")
	}
	if !options.update && !options.version && options.target == "" {
		return false, nil
	}
	attachConsole()
	var conflict string
	flags.Visit(func(f *flag.Flag) {
		if f.Name != "update" && f.Name != "update-target" && f.Name != "update-parent" && f.Name != "update-output" && f.Name != "update-request" && f.Name != "version" {
			conflict = f.Name
		}
	})
	if conflict != "" || flags.NArg() != 0 || (options.version && (options.update || options.target != "")) || (options.update && options.target != "") {
		return true, fmt.Errorf("--update and --version must be used alone")
	}
	if options.version {
		fmt.Println(version)
		return true, nil
	}
	if options.target != "" {
		if options.requestID != "" {
			if !filepath.IsAbs(options.target) || options.parent != 0 || options.output != "" {
				return true, fmt.Errorf("invalid queued update arguments")
			}
			return true, runRequested(options.target, role, version, installationID, options.requestID, io.Discard)
		}
		if !filepath.IsAbs(options.target) || options.parent <= 0 || options.output == "" {
			return true, fmt.Errorf("invalid internal update arguments")
		}
		log, err := os.OpenFile(options.output, os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return true, fmt.Errorf("opening update output: %w", err)
		}
		defer log.Close()
		err = run(options.target, role, version, installationID, options.parent, log, nil)
		if err != nil {
			fmt.Fprintln(log, "Update failed:", err)
		}
		return true, err
	}
	target, err := os.Executable()
	if err != nil {
		return true, err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		return true, err
	}
	scratch, err := os.MkdirTemp("", "poker-tracker-update-")
	if err != nil {
		return true, err
	}
	defer os.RemoveAll(scratch)
	helper := filepath.Join(scratch, "update-helper")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	if err := copyFile(target, helper, 0700); err != nil {
		return true, err
	}
	output := filepath.Join(scratch, "result.txt")
	if err := os.WriteFile(output, nil, 0600); err != nil {
		return true, err
	}
	fmt.Println("Checking the latest stable release and updating this installation...")
	err = runHelper(helper, []string{"--update-target", target, "--update-parent", strconv.Itoa(os.Getpid()), "--update-output", output})
	data, readErr := os.ReadFile(output)
	if len(data) > 0 {
		fmt.Print(string(data))
	}
	if err != nil {
		return true, fmt.Errorf("update helper: %w", err)
	}
	return true, readErr
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}
type release struct {
	Tag        string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

func assetName(role, goos, arch string) (string, error) {
	if arch != "amd64" && arch != "arm64" {
		return "", fmt.Errorf("unsupported architecture %s", arch)
	}
	switch {
	case role == "combined" && goos == "windows":
		return "tracker-" + arch + ".exe", nil
	case role == "client" && goos == "windows":
		return "tracker-client-" + arch + ".exe", nil
	case role == "server" && (goos == "windows" || goos == "linux"):
		name := "tracker-server-" + goos + "-" + arch
		if goos == "windows" {
			name += ".exe"
		}
		return name, nil
	}
	return "", fmt.Errorf("unsupported update target %s/%s/%s", role, goos, arch)
}

func versionParts(text string) ([3]uint64, error) {
	var result [3]uint64
	pieces := strings.Split(strings.TrimPrefix(text, "v"), ".")
	if len(pieces) < 2 || len(pieces) > 3 {
		return result, fmt.Errorf("not a stable release version: %s", text)
	}
	for i, piece := range pieces {
		if piece == "" {
			return result, fmt.Errorf("invalid version %s", text)
		}
		for _, c := range piece {
			if c < '0' || c > '9' {
				return result, fmt.Errorf("invalid version %s", text)
			}
		}
		n, err := strconv.ParseUint(piece, 10, 64)
		if err != nil {
			return result, err
		}
		result[i] = n
	}
	return result, nil
}

func newer(latest, current string) (bool, error) {
	want, err := versionParts(latest)
	if err != nil {
		return false, err
	}
	have, err := versionParts(current)
	if err != nil {
		return false, fmt.Errorf("current build has no release version; install a released build first: %w", err)
	}
	for i := range want {
		if want[i] != have[i] {
			return want[i] > have[i], nil
		}
	}
	return false, nil
}

func get(client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "poker-tracker-updater")
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("download returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

func latest(client *http.Client, url string) (release, error) {
	var value release
	response, err := get(client, url)
	if err != nil {
		return value, err
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		return value, fmt.Errorf("reading release metadata: %w", err)
	}
	if value.Draft || value.Prerelease {
		return value, fmt.Errorf("GitHub did not return a stable release")
	}
	return value, nil
}

func findAsset(value release, name string) (releaseAsset, error) {
	for _, asset := range value.Assets {
		if asset.Name == name && asset.Size > 0 && asset.URL != "" {
			return asset, nil
		}
	}
	return releaseAsset{}, fmt.Errorf("release %s has no usable %s; use its installer", value.Tag, name)
}

func stage(client *http.Client, value release, name, dir string) (string, error) {
	asset, err := findAsset(value, name)
	if err != nil {
		return "", err
	}
	sums, err := findAsset(value, "SHA256SUMS")
	if err != nil {
		return "", err
	}
	response, err := get(client, sums.URL)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, sums.Size+1))
	response.Body.Close()
	if err != nil {
		return "", err
	}
	if int64(len(data)) != sums.Size {
		return "", fmt.Errorf("incomplete checksum manifest")
	}
	var expected string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			expected = fields[0]
		}
	}
	decoded, err := hex.DecodeString(expected)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("missing or invalid checksum for %s", name)
	}
	response, err = get(client, asset.URL)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	file, err := os.CreateTemp(dir, ".tracker-update-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, asset.Size+1))
	if err != nil {
		return "", err
	}
	if count != asset.Size {
		return "", fmt.Errorf("incomplete or oversized executable download")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected) {
		return "", fmt.Errorf("executable checksum mismatch")
	}
	if err := file.Chmod(0700); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

func run(target, role, version, id string, parent int, out io.Writer, job *config.UpdateJob) error {
	recoveredVersion, err := recoverPendingUpdate(target, role, id, parent)
	if err != nil {
		return fmt.Errorf("recovering previous update: %w", err)
	}
	if recoveredVersion != "" {
		version = recoveredVersion
	}
	name, err := assetName(role, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	url := latestURL
	if job != nil {
		if err := ValidateRequestVersion(job.Request.Version); err != nil {
			return err
		}
		url = "https://api.github.com/repos/vit-ui/poker-tracker/releases/tags/" + job.Request.Version
	}
	value, err := latest(http.DefaultClient, url)
	if err != nil {
		return fmt.Errorf("checking GitHub release: %w", err)
	}
	if job != nil && value.Tag != job.Request.Version {
		return fmt.Errorf("release metadata does not match requested version %s", job.Request.Version)
	}
	upgrade, err := newer(value.Tag, version)
	if err != nil {
		return err
	}
	if !upgrade {
		if job != nil {
			job.Version = version
		}
		fmt.Fprintf(out, "Already up to date (%s).\n", version)
		return nil
	}
	fmt.Fprintf(out, "Downloading %s (%s)...\n", value.Tag, name)
	staged, err := stage(http.DefaultClient, value, name, filepath.Dir(target))
	if err != nil {
		return fmt.Errorf("staging update: %w", err)
	}
	defer os.Remove(staged)
	// A release executable must identify itself before the running owner is touched.
	if runtime.GOOS == "windows" {
		renamed := staged + ".exe"
		if err := moveExecutable(staged, renamed); err != nil {
			return err
		}
		staged = renamed
		defer os.Remove(staged)
	}
	for _, probe := range []struct{ flag, want string }{{"--role", role}, {"--version", value.Tag}} {
		result, err := exec.Command(staged, probe.flag).Output()
		if err != nil {
			return fmt.Errorf("probing staged executable: %w", err)
		}
		if strings.TrimSpace(string(result)) != probe.want {
			return fmt.Errorf("staged executable %s mismatch", probe.flag)
		}
	}
	jobPath := filepath.Join(filepath.Dir(target), "requested_update.json")
	if err := waitForPokerStars(jobPath, role, job); err != nil {
		return err
	}
	if job != nil {
		job.State = config.UpdateRunning
		if err := config.SaveUpdateJob(jobPath, *job); err != nil {
			return err
		}
	}
	service, err := inspectService(target, role, id, parent)
	if err != nil {
		return err
	}
	defer service.close()
	installed, err := exec.Command(target, "--version").Output()
	if err != nil {
		return fmt.Errorf("reading installed version before replacement: %w", err)
	}
	version = strings.TrimSpace(string(installed))
	upgrade, err = newer(value.Tag, version)
	if err != nil {
		return err
	}
	if !upgrade {
		if job != nil {
			job.Version = version
		}
		fmt.Fprintf(out, "Already up to date (%s).\n", version)
		return nil
	}
	if err := service.prepareUpdate(staged, version, value.Tag); err != nil {
		return fmt.Errorf("preparing update recovery: %w", err)
	}
	if err := service.stop(); err != nil {
		restoreErr := service.restore()
		if restoreErr == nil {
			restoreErr = service.finishUpdate()
		}
		return errors.Join(fmt.Errorf("stopping installation: %w", err), restoreErr)
	}
	if err := replace(target, staged, service, value.Tag, version); err != nil {
		return err
	}
	if job != nil {
		job.Version = value.Tag
	}
	fmt.Fprintf(out, "Updated %s to %s.\n", role, value.Tag)
	return nil
}

func replace(target, staged string, service *serviceState, newVersion, oldVersion string) (result error) {
	old, err := service.previousExecutable()
	if err != nil {
		return abortUpdate(service, oldVersion, err)
	}
	if err := moveExecutable(target, old); err != nil {
		return abortUpdate(service, oldVersion, fmt.Errorf("saving previous executable: %w", err))
	}
	if err := moveExecutable(staged, target); err != nil {
		restoreErr := moveExecutable(old, target)
		if restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return abortUpdate(service, oldVersion, err)
	}
	if err := restartAndConfirm(service, newVersion); err != nil {
		stopErr := service.stop()
		if stopErr != nil {
			return errors.Join(fmt.Errorf("new executable startup failed: %w", err), stopErr, fmt.Errorf("previous executable retained at %s", old))
		}
		failed := staged
		moveErr := moveExecutable(target, failed)
		if moveErr != nil {
			return errors.Join(err, moveErr, fmt.Errorf("previous executable retained at %s", old))
		}
		restoreErr := moveExecutable(old, target)
		if restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return abortUpdate(service, oldVersion, fmt.Errorf("new executable could not be confirmed; restored previous executable: %w", err))
	}
	if err := service.confirmUpdate(); err != nil {
		return fmt.Errorf("persisting update confirmation; recovery remains armed: %w", err)
	}
	if err := service.finishUpdate(); err != nil {
		return fmt.Errorf("finishing update recovery: %w", err)
	}
	// Windows may retain the caller's mapped image until --update returns.
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stdout, "Previous executable retained at %s\n", old)
	}
	return nil
}

func copyFile(source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

func abortUpdate(service *serviceState, version string, cause error) error {
	err := restartAndConfirm(service, version)
	if err == nil {
		err = service.finishUpdate()
	}
	return errors.Join(cause, err)
}

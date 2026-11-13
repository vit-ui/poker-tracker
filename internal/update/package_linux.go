package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// The outer release tar contains exactly one makeself executable. Ignore other
// entries rather than unpacking paths supplied by the archive into the filesystem.
func unpackLinuxInstaller(archive, output string) error {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return fmt.Errorf("Linux package has no PokerTracker-Server-linux installer")
		}
		if err != nil {
			return err
		}
		if header.Name != "PokerTracker-Server-linux" {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("Linux installer entry is not a regular file")
		}
		out, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, reader)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
}

func extractPackage(archive, payload, output string) error {
	if payload != "tracker-server-amd64" && payload != "tracker-server-arm64" {
		return fmt.Errorf("unsupported Linux installer payload %s", payload)
	}
	self := filepath.Join(filepath.Dir(archive), "PokerTracker-Server-linux")
	if err := unpackLinuxInstaller(archive, self); err != nil {
		return err
	}
	out, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	// Makeself --tar exits after the tar operation. It cannot run install.sh,
	// register a service, or write any other archive member to disk.
	cmd := exec.Command("sh", self, "--tar", "xOf", "./"+payload)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reading Linux installer payload: %w: %s", err, stderr.String())
	}
	return out.Close()
}

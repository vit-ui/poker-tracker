package client

import (
	"fmt"
	"io"
	"os"

	"github.com/vit-ui/self-tracker/internal/config"
	"github.com/vit-ui/self-tracker/internal/transport"
)

func (c *Collector) streamScanFile(cmd transport.Command, configPath string, dead, finished <-chan struct{}) {
	send := func(chunk transport.ScanChunk) bool {
		chunk.RequestID = cmd.RequestID
		select {
		case c.link.Uploads <- transport.Upload{Scan: &chunk}:
			return true
		case <-dead:
		case <-finished:
		}
		return false
	}
	err := c.readScanFile(cmd.ScanPath, configPath, send)
	if err != nil {
		send(transport.ScanChunk{Done: true, Error: err.Error()})
	}
}

func (c *Collector) readScanFile(path, configPath string, send func(transport.ScanChunk) bool) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return err
	}
	name := cfg.Setup.PokerStarsScreenName
	if name == nil || *name == "" {
		return fmt.Errorf("set the PokerStars screen name before scanning")
	}
	path, err = config.ResolveCollectorScanPath(path)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening scan file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat scan file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("the scan path must be a nonempty regular file")
	}
	source := &io.LimitedReader{R: f, N: info.Size()}
	for source.N > 0 {
		buf := make([]byte, 256*1024)
		n, err := source.Read(buf)
		if n > 0 && !send(transport.ScanChunk{Bytes: buf[:n]}) {
			return nil
		}
		if err != nil && (err != io.EOF || source.N != 0) {
			return fmt.Errorf("reading scan file: %w", err)
		}
	}
	after, err := f.Stat()
	if err != nil {
		return fmt.Errorf("checking scan file after reading: %w", err)
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return fmt.Errorf("the file changed during the scan; try again when it is complete")
	}
	send(transport.ScanChunk{Done: true, ScreenName: *name})
	return nil
}

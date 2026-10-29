// Package logging configures bounded file logs for background tracker roles.
package logging

import (
	"io"
	"log/slog"
)

// Configure sends logs to path with one rollover. Open failure discards logs;
// hidden Windows consoles must never fall back to an invalid stderr handle.
func Configure(path string) func() {
	writer, err := newRotatingWriter(path, logMaxBytes)
	if err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		return func() {}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return func() { writer.Close() }
}

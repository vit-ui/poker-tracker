package logging

import (
	"io"
	"log/slog"
)

func Configure(path string) func() {
	writer, err := newRotatingWriter(path, logMaxBytes)
	if err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		return func() {}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return func() { writer.Close() }
}

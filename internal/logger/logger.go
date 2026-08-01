package logger

import (
	"log/slog"
	"os"
)

// Init sets the global slog default to a JSON handler on stdout, so log
// lines can be parsed as structured fields by Promtail/Loki.
// After calling this, use slog.Info / slog.Error / slog.Warn directly anywhere.
func Init() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
}

package logger

import (
	"io"
	"log/slog"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Init sets the global slog default to a JSON handler writing to both
// stdout (so `pm2 logs` still works) and a self-rotating log file (so
// Promtail can tail it directly, independent of pm2's log handling).
// After calling this, use slog.Info / slog.Error / slog.Warn directly anywhere.
func Init(logPath string) {
	fileWriter := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    100, // megabytes
		MaxBackups: 5,
		MaxAge:     14, // days
		Compress:   true,
	}

	out := io.MultiWriter(os.Stdout, fileWriter)
	slog.SetDefault(slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
}

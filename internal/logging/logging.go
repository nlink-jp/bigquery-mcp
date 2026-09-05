// Package logging builds the slog.Logger used by `bigquery-mcp serve`.
// It honors config.Server.LogLevel and config.Server.LogFile, and rotates the
// log file on startup (keeping a fixed number of generations) so we don't
// need an external rotator like lumberjack.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// KeepGenerations is the maximum number of rotated log files retained alongside
// the active file (i.e. .1 through .KeepGenerations).
const KeepGenerations = 5

// Setup returns a logger built from level and logFile.
//
// If logFile is empty, logs go to stderr only.
// Otherwise the existing file (and prior generations) are rotated, and logs
// are duplicated to stderr and the new file. The returned *os.File is the
// file handle the caller must Close on shutdown; it is nil for the
// stderr-only path.
func Setup(level string, logFile string) (*slog.Logger, *os.File, error) {
	lvl := parseLevel(level)
	opts := &slog.HandlerOptions{Level: lvl}

	if logFile == "" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil, nil
	}

	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log dir: %w", err)
	}
	if err := RotateOnStartup(logFile, KeepGenerations); err != nil {
		return nil, nil, fmt.Errorf("rotate log file: %w", err)
	}
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file: %w", err)
	}
	multi := io.MultiWriter(os.Stderr, f)
	return slog.New(slog.NewTextHandler(multi, opts)), f, nil
}

// RotateOnStartup renames path -> path.1, path.1 -> path.2, ..., dropping the
// file beyond keep. Missing files are silently ignored. Idempotent.
func RotateOnStartup(path string, keep int) error {
	if keep < 1 {
		return nil
	}
	// Drop the oldest if present.
	_ = os.Remove(fmt.Sprintf("%s.%d", path, keep))
	// Shift .{i} -> .{i+1} for i = keep-1 down to 1.
	for i := keep - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", path, i)
		to := fmt.Sprintf("%s.%d", path, i+1)
		_ = os.Rename(from, to)
	}
	// Current -> .1
	if _, err := os.Stat(path); err == nil {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	return nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "", "info":
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}

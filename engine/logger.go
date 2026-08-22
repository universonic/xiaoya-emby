package engine

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// logLevel is the dynamic minimum level of the default slog logger, so the
// level can be changed after startup (e.g. from the --log-level flag).
var logLevel = new(slog.LevelVar)

// logWriter is the destination of the default logger; kept as a variable for
// testability.
var logWriter io.Writer = os.Stderr

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{
		Level: logLevel,
	})))
}

// SetLogLevel sets the minimum log level from a case-insensitive name
// (debug, info, warn, error).
func SetLogLevel(s string) error {
	lvl, err := parseLogLevel(s)
	if err != nil {
		return err
	}
	logLevel.Set(lvl)
	return nil
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level %q (valid: debug, info, warn, error)", s)
	}
}

// defaultLogLevel returns the initial --log-level flag value: the LOG_LEVEL
// environment variable if set, otherwise "info".
func defaultLogLevel() string {
	if s := strings.TrimSpace(os.Getenv("LOG_LEVEL")); s != "" {
		return s
	}
	return "info"
}

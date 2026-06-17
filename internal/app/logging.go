package app

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// NewLogger builds the process logger.
//
// level accepts zerolog names (debug/info/warn/error/fatal) and the uppercase
// syslog-style names (DEBUG/INFO/WARNING/ERROR/CRITICAL) that TMS_LOG_LEVEL
// also recognizes, case-insensitively. An unknown level
// is an error — fail loud at startup rather than silently logging at the
// wrong level.
//
// format is one of:
//   - "json":    structured JSON to stderr (production / containers)
//   - "console": human-readable colored output (interactive use)
//   - "auto":    console when stderr is a terminal, JSON otherwise
func NewLogger(level, format string) (zerolog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return zerolog.Nop(), err
	}

	var out io.Writer = os.Stderr
	switch strings.ToLower(format) {
	case "console":
		out = consoleWriter()
	case "json", "":
		// keep raw stderr
	case "auto":
		if stderrIsTerminal() {
			out = consoleWriter()
		}
	default:
		return zerolog.Nop(), fmt.Errorf("unknown log format %q (want auto|console|json)", format)
	}

	logger := zerolog.New(out).Level(lvl).With().Timestamp().Logger()
	return logger, nil
}

func consoleWriter() io.Writer {
	return zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}
}

func stderrIsTerminal() bool {
	info, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// parseLevel maps both zerolog-style and uppercase syslog-style level names
// to a zerolog.Level.
func parseLevel(level string) (zerolog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "trace":
		return zerolog.TraceLevel, nil
	case "debug":
		return zerolog.DebugLevel, nil
	case "info", "":
		return zerolog.InfoLevel, nil
	case "warn", "warning":
		return zerolog.WarnLevel, nil
	case "error":
		return zerolog.ErrorLevel, nil
	case "fatal", "critical":
		return zerolog.FatalLevel, nil
	default:
		return zerolog.NoLevel, fmt.Errorf("unknown log level %q (want debug|info|warn|error)", level)
	}
}

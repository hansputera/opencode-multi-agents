package logger

import (
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// Logger wraps zerolog with our configuration
type Logger struct {
	zerolog.Logger
}

// New creates a new logger with the given level and format
func New(level, format string) *Logger {
	var output io.Writer = os.Stdout

	// Use console format for better readability in development
	if format == "console" {
		output = zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: time.RFC3339,
		}
	}

	// Parse log level
	var lvl zerolog.Level
	switch level {
	case "debug":
		lvl = zerolog.DebugLevel
	case "info":
		lvl = zerolog.InfoLevel
	case "warn":
		lvl = zerolog.WarnLevel
	case "error":
		lvl = zerolog.ErrorLevel
	default:
		lvl = zerolog.InfoLevel
	}

	zl := zerolog.New(output).
		Level(lvl).
		With().
		Timestamp().
		Logger()

	return &Logger{zl}
}

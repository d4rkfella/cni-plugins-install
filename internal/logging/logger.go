package logging

import (
	"os"

	"github.com/rs/zerolog"
)

// Logger represents a logger instance
type Logger struct {
	logger zerolog.Logger
}

// NewLogger creates a new logger with the specified log level
func NewLogger() *Logger {
	level := zerolog.InfoLevel
	switch GetLogLevel() {
	case "debug":
		level = zerolog.DebugLevel
	case "warn":
		level = zerolog.WarnLevel
	case "error":
		level = zerolog.ErrorLevel
	case "fatal":
		level = zerolog.FatalLevel
	}

	logger := zerolog.New(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: "2006-01-02 15:04:05",
	}).
		Level(level).
		With().
		Timestamp().
		Logger()

	return &Logger{logger: logger}
}

// GetLogLevel returns the log level from environment or default
func GetLogLevel() string {
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}
	return logLevel
}

// Debug logs a debug message
func (l *Logger) Debug() *zerolog.Event {
	return l.logger.Debug()
}

// Info logs an info message
func (l *Logger) Info() *zerolog.Event {
	return l.logger.Info()
}

// Warn logs a warning message
func (l *Logger) Warn() *zerolog.Event {
	return l.logger.Warn()
}

// Error logs an error message
func (l *Logger) Error() *zerolog.Event {
	return l.logger.Error()
}

// Fatal logs a fatal message and exits
func (l *Logger) Fatal() *zerolog.Event {
	return l.logger.Fatal()
}

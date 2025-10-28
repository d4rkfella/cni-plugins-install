package logging

import (
	"os"

	"github.com/rs/zerolog"
)

type Logger struct {
	logger zerolog.Logger
}

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

func GetLogLevel() string {
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}
	return logLevel
}

func (l *Logger) Debug() *zerolog.Event {
	return l.logger.Debug()
}

func (l *Logger) Info() *zerolog.Event {
	return l.logger.Info()
}

func (l *Logger) Warn() *zerolog.Event {
	return l.logger.Warn()
}

func (l *Logger) Error() *zerolog.Event {
	return l.logger.Error()
}

func (l *Logger) Fatal() *zerolog.Event {
	return l.logger.Fatal()
}

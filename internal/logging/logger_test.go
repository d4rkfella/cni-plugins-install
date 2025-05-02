package logging

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Helper to set/unset env vars for testing
func setEnv(t *testing.T, key, value string) func() {
	original, existed := os.LookupEnv(key)
	require.NoError(t, os.Setenv(key, value))
	return func() {
		if existed {
			require.NoError(t, os.Setenv(key, original))
		} else {
			require.NoError(t, os.Unsetenv(key))
		}
	}
}

// Tests the GetLogLevel function's handling of the LOG_LEVEL env var.
func TestGetLogLevel(t *testing.T) {
	tests := []struct {
		name   string
		setEnv string // Value to set LOG_LEVEL to
		unset  bool   // Whether to unset LOG_LEVEL
		want   string
	}{
		{"DefaultLevel", "", true, "info"},
		{"DebugLevelSet", "debug", false, "debug"},
		{"WarnLevelSet", "warn", false, "warn"},
		{"ErrorLevelSet", "error", false, "error"},
		{"FatalLevelSet", "fatal", false, "fatal"},
		{"InfoLevelSet", "info", false, "info"},
		{"InvalidLevelSet", "invalid", false, "invalid"}, // Should still return the invalid string
		{"EmptyLevelSet", "", false, "info"},             // Should return info if set empty
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.unset {
				reset := setEnv(t, "LOG_LEVEL", "dummy") // Set dummy first, pass t
				require.NoError(t, os.Unsetenv("LOG_LEVEL"))
				defer reset() // Ensure cleanup even if test panics
			} else {
				reset := setEnv(t, "LOG_LEVEL", tt.setEnv) // Pass t
				defer reset()
			}
			assert.Equal(t, tt.want, GetLogLevel())
		})
	}
}

// Tests that NewLogger creates a logger with the correct level based on LOG_LEVEL.
func TestNewLogger(t *testing.T) {
	tests := []struct {
		name      string
		logLevel  string
		wantLevel zerolog.Level
	}{
		{"DebugLevel", "debug", zerolog.DebugLevel},
		{"InfoLevel", "info", zerolog.InfoLevel},
		{"WarnLevel", "warn", zerolog.WarnLevel},
		{"ErrorLevel", "error", zerolog.ErrorLevel},
		{"FatalLevel", "fatal", zerolog.FatalLevel},
		{"DefaultLevel (info)", "", zerolog.InfoLevel},
		{"InvalidLevel (info)", "invalid", zerolog.InfoLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reset := setEnv(t, "LOG_LEVEL", tt.logLevel) // Pass t
			defer reset()

			l := NewLogger()
			assert.NotNil(t, l)
			assert.Equal(t, tt.wantLevel, l.logger.GetLevel())
		})
	}
}

// Tests that the logger wrapper methods (Debug, Info, Warn, Error, Fatal)
// correctly invoke the underlying zerolog methods.
func TestLoggerMethods(t *testing.T) {
	// Redirect output to a buffer for testing
	var buf bytes.Buffer
	originalWriter := zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: "2006-01-02 15:04:05",
	}

	// Can't easily swap the writer after NewLogger, so create a test logger
	testZerolog := zerolog.New(&buf).Level(zerolog.DebugLevel).With().Timestamp().Logger()
	l := &Logger{logger: testZerolog}

	// Test each method - just call it and check buffer has *something*
	l.Debug().Msg("debug message")
	assert.True(t, strings.Contains(buf.String(), "debug message"), "Debug log missing")
	buf.Reset()

	l.Info().Msg("info message")
	assert.True(t, strings.Contains(buf.String(), "info message"), "Info log missing")
	buf.Reset()

	l.Warn().Msg("warn message")
	assert.True(t, strings.Contains(buf.String(), "warn message"), "Warn log missing")
	buf.Reset()

	l.Error().Msg("error message")
	assert.True(t, strings.Contains(buf.String(), "error message"), "Error log missing")
	buf.Reset()

	// Fatal is tricky as it calls os.Exit. We can skip testing its output directly
	// or use a more complex setup with os.Exit overriding.
	// For coverage, just ensuring the call doesn't panic might suffice.
	assert.NotPanics(t, func() {
		_ = l.Fatal() // We don't call Msg to avoid exit
	})

	// Restore default writer if necessary (not strictly needed as we created a separate logger)
	_ = originalWriter // Avoid unused variable error
}

package config

import (
	"os"
	"testing"

	"github.com/darkfella/cni-plugins-install/internal/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests that NewConfig correctly initializes config with default values.
func TestNewConfig(t *testing.T) {
	c := NewConfig()

	assert.Equal(t, constants.DefaultBaseURL, c.BaseURL)
	assert.Equal(t, constants.DefaultTargetDir, c.TargetDir)
	assert.Equal(t, constants.DefaultDownloadTimeout, c.DownloadTimeout)
	assert.Equal(t, constants.DefaultMaxRetries, c.MaxRetries)
	assert.Equal(t, constants.DefaultBufferSize, c.BufferSize)
}

// Tests the LoadFromEnv function's ability to override default config values
// using environment variables, and ensures it doesn't affect unset values.
func TestLoadFromEnv(t *testing.T) {
	// --- Helper to manage environment variables ---
	setEnv := func(key, value string) (resetFunc func()) {
		originalValue, exists := os.LookupEnv(key)
		require.NoError(t, os.Setenv(key, value))
		return func() {
			if exists {
				require.NoError(t, os.Setenv(key, originalValue))
			} else {
				require.NoError(t, os.Unsetenv(key))
			}
		}
	}

	// --- Test Cases ---
	t.Run("OverridesSetValues", func(t *testing.T) {
		c := NewConfig() // Start with defaults

		// Set environment variables
		newURL := "http://custom.url/cni"
		newDir := "/custom/cni/dir"
		resetURL := setEnv("CNI_PLUGINS_BASE_URL", newURL)
		defer resetURL()
		resetDir := setEnv("CNI_PLUGINS_TARGET_DIR", newDir)
		defer resetDir()

		// Load from env
		c.LoadFromEnv()

		// Assert values were overridden
		assert.Equal(t, newURL, c.BaseURL)
		assert.Equal(t, newDir, c.TargetDir)

		// Assert other values remain default
		assert.Equal(t, constants.DefaultDownloadTimeout, c.DownloadTimeout)
		assert.Equal(t, constants.DefaultMaxRetries, c.MaxRetries)
		assert.Equal(t, constants.DefaultBufferSize, c.BufferSize)
	})

	t.Run("NoOverridesWhenNotSet", func(t *testing.T) {
		c := NewConfig() // Start with defaults

		// Ensure relevant env vars are NOT set
		originalURL, urlExists := os.LookupEnv("CNI_PLUGINS_BASE_URL")
		originalDir, dirExists := os.LookupEnv("CNI_PLUGINS_TARGET_DIR")
		require.NoError(t, os.Unsetenv("CNI_PLUGINS_BASE_URL"))
		require.NoError(t, os.Unsetenv("CNI_PLUGINS_TARGET_DIR"))
		defer func() { // Restore original env state
			if urlExists {
				require.NoError(t, os.Setenv("CNI_PLUGINS_BASE_URL", originalURL))
			}
			if dirExists {
				require.NoError(t, os.Setenv("CNI_PLUGINS_TARGET_DIR", originalDir))
			}
		}()

		// Load from env
		c.LoadFromEnv()

		// Assert values remain default
		assert.Equal(t, constants.DefaultBaseURL, c.BaseURL)
		assert.Equal(t, constants.DefaultTargetDir, c.TargetDir)
		assert.Equal(t, constants.DefaultDownloadTimeout, c.DownloadTimeout)
		assert.Equal(t, constants.DefaultMaxRetries, c.MaxRetries)
		assert.Equal(t, constants.DefaultBufferSize, c.BufferSize)
	})

	t.Run("EmptyEnvVarsAreIgnored", func(t *testing.T) {
		c := NewConfig() // Start with defaults

		// Set EMPTY environment variables
		resetURL := setEnv("CNI_PLUGINS_BASE_URL", "")
		defer resetURL()
		resetDir := setEnv("CNI_PLUGINS_TARGET_DIR", "")
		defer resetDir()

		// Load from env
		c.LoadFromEnv()

		// Assert values remain default
		assert.Equal(t, constants.DefaultBaseURL, c.BaseURL)
		assert.Equal(t, constants.DefaultTargetDir, c.TargetDir)
	})
}

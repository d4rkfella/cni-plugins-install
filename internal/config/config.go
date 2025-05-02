package config

import (
	"os"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/constants"
)

// Config represents the application configuration
type Config struct {
	BaseURL         string
	TargetDir       string
	DownloadTimeout time.Duration
	MaxRetries      int
	BufferSize      int
}

// NewConfig creates a new configuration with defaults
func NewConfig() *Config {
	return &Config{
		BaseURL:         constants.DefaultBaseURL,
		TargetDir:       constants.DefaultTargetDir,
		DownloadTimeout: constants.DefaultDownloadTimeout,
		MaxRetries:      constants.DefaultMaxRetries,
		BufferSize:      constants.DefaultBufferSize,
	}
}

// LoadFromEnv loads configuration from environment variables
func (c *Config) LoadFromEnv() {
	if baseURL := os.Getenv("CNI_PLUGINS_BASE_URL"); baseURL != "" {
		c.BaseURL = baseURL
	}

	if targetDir := os.Getenv("CNI_PLUGINS_TARGET_DIR"); targetDir != "" {
		c.TargetDir = targetDir
	}

}

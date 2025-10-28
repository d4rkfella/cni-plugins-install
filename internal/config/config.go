package config

import (
	"os"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/constants"
)

type Config struct {
	BaseURL         string
	TargetDir       string
	DownloadTimeout time.Duration
	MaxRetries      int
	BufferSize      int
}

func NewConfig() *Config {
	return &Config{
		BaseURL:         constants.DefaultBaseURL,
		TargetDir:       constants.DefaultTargetDir,
		DownloadTimeout: constants.DefaultDownloadTimeout,
		MaxRetries:      constants.DefaultMaxRetries,
		BufferSize:      constants.DefaultBufferSize,
	}
}

func (c *Config) LoadFromEnv() {
	if baseURL := os.Getenv("CNI_PLUGINS_BASE_URL"); baseURL != "" {
		c.BaseURL = baseURL
	}
	if targetDir := os.Getenv("CNI_PLUGINS_TARGET_DIR"); targetDir != "" {
		c.TargetDir = targetDir
	}
}

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/darkfella/cni-plugins-install/internal/config"
	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/darkfella/cni-plugins-install/pkg/artifact"
	"github.com/darkfella/cni-plugins-install/pkg/sync"
)

// run contains the core application logic, including setup and cleanup orchestration.
func run() error { // Removed parameters
	// --- Setup Phase ---
	logger := logging.NewLogger()

	// Create context with cancellation for signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel() // Ensure context cancel runs on exit

	// Load configuration
	cfg := config.NewConfig()
	cfg.LoadFromEnv()

	// Create artifact downloader instance
	artifactDownloader := artifact.NewDownloader(logger, nil, &artifact.Config{
		BaseURL:         cfg.BaseURL,
		DownloadTimeout: cfg.DownloadTimeout,
		MaxRetries:      cfg.MaxRetries,
		BufferSize:      cfg.BufferSize,
	})
	// Defer cleanup immediately after creation
	defer func() {
		if err := artifactDownloader.Cleanup(); err != nil {
			logger.Warn().Err(err).Msg("Error during deferred cleanup")
		}
	}()

	// --- Core Logic Phase ---

	// Get version from environment
	version := os.Getenv("CNI_PLUGINS_VERSION")
	if version == "" {
		return fmt.Errorf("CNI_PLUGINS_VERSION environment variable is required")
	}

	// Create sync client
	syncClient := sync.NewSync(logger)

	// Check if version is already installed
	logger.Info().Msg("Checking if version file exists and matches the requested version")
	versionMatches, err := syncClient.CheckVersion(ctx, cfg.TargetDir, version)
	if err != nil {
		return fmt.Errorf("failed to check installed version: %w", err)
	}

	if versionMatches {
		logger.Info().Str("version", version).Msg("Version matches")
		// Verify all plugins exist and have correct checksums
		pluginsValid, err := syncClient.VerifyPlugins(ctx, cfg.TargetDir)
		if err != nil {
			return fmt.Errorf("failed to verify plugins: %w", err)
		}
		if pluginsValid {
			logger.Info().Msg("All managed plugins verified successfully")
			logger.Info().Msg("All plugins are valid, no installation needed")
			return nil // Successful early exit
		}
		logger.Info().Msg("Plugin verification failed, will reinstall")
	}

	// --- Installation Flow ---

	// Download and extract CNI plugins
	logger.Info().Str("version", version).Msg("Downloading and extracting CNI plugins")
	if err := artifactDownloader.DownloadAndExtract(ctx, version, cfg.TargetDir); err != nil {
		return fmt.Errorf("failed to download and extract CNI plugins: %w", err)
	}

	// Sync files from staging to target directory
	logger.Info().Msg("Syncing files to target directory")
	if err := syncClient.SyncFiles(ctx, artifactDownloader.StagingDir(), cfg.TargetDir); err != nil {
		return fmt.Errorf("failed to sync files: %w", err)
	}

	// Save version information
	logger.Info().Msg("Saving version information")
	if err := syncClient.SaveVersion(ctx, cfg.TargetDir, version); err != nil {
		return fmt.Errorf("failed to save version information: %w", err)
	}

	// Final verification after install
	logger.Info().Msg("Verifying installed plugins post-installation")
	valid, err := syncClient.VerifyPlugins(ctx, cfg.TargetDir)
	if err != nil {
		return fmt.Errorf("failed to verify plugins post-installation: %w", err)
	}
	if !valid {
		return fmt.Errorf("plugin verification failed post-installation: plugins invalid after sync and save")
	}

	logger.Info().Str("version", version).Msg("CNI plugins installed successfully")
	return nil // Successful completion
}

func main() {
	// Call the main application logic function
	if err := run(); err != nil {
		// Log the error from run using a basic logger setup or stderr
		// Using fmt here as the main logger might be part of the run() setup
		// which failed, or just to keep main super simple.
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	// Implicit exit 0 on success
}

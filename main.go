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

func run() error {
	logger := logging.NewLogger()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := config.NewConfig()
	cfg.LoadFromEnv()

	artifactDownloader := artifact.NewDownloader(logger, nil, &artifact.Config{
		BaseURL:         cfg.BaseURL,
		DownloadTimeout: cfg.DownloadTimeout,
		MaxRetries:      cfg.MaxRetries,
		BufferSize:      cfg.BufferSize,
	})
	defer func() {
		if err := artifactDownloader.Cleanup(); err != nil {
			logger.Warn().Err(err).Msg("Error during deferred cleanup")
		}
	}()
	
	version := os.Getenv("CNI_PLUGINS_VERSION")
	if version == "" {
		return fmt.Errorf("CNI_PLUGINS_VERSION environment variable is required")
	}

	syncClient := sync.NewSync(logger)
	logger.Info().Msg("Checking if version file exists and matches the requested version")
	versionMatches, err := syncClient.CheckVersion(ctx, cfg.TargetDir, version)
	if err != nil {
		return fmt.Errorf("failed to check installed version: %w", err)
	}
	if versionMatches {
		logger.Info().Str("version", version).Msg("Version matches")
		pluginsValid, err := syncClient.VerifyPlugins(ctx, cfg.TargetDir)
		if err != nil {
			return fmt.Errorf("failed to verify plugins: %w", err)
		}
		if pluginsValid {
			logger.Info().Msg("All managed plugins verified successfully")
			logger.Info().Msg("All plugins are valid, no installation needed")
			return nil
		}
		logger.Info().Msg("Plugin verification failed, will reinstall")
	}

	logger.Info().Str("version", version).Msg("Downloading and extracting CNI plugins")
	if err := artifactDownloader.DownloadAndExtract(ctx, version, cfg.TargetDir); err != nil {
		return fmt.Errorf("failed to download and extract CNI plugins: %w", err)
	}

	logger.Info().Msg("Syncing files to target directory")
	if err := syncClient.SyncFiles(ctx, artifactDownloader.StagingDir(), cfg.TargetDir); err != nil {
		return fmt.Errorf("failed to sync files: %w", err)
	}

	logger.Info().Msg("Saving version information")
	if err := syncClient.SaveVersion(ctx, cfg.TargetDir, version); err != nil {
		return fmt.Errorf("failed to save version information: %w", err)
	}

	logger.Info().Msg("Verifying installed plugins post-installation")
	valid, err := syncClient.VerifyPlugins(ctx, cfg.TargetDir)
	if err != nil {
		return fmt.Errorf("failed to verify plugins post-installation: %w", err)
	}
	if !valid {
		return fmt.Errorf("plugin verification failed post-installation: plugins invalid after sync and save")
	}

	logger.Info().Str("version", version).Msg("CNI plugins installed successfully")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

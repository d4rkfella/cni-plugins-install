package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/constants"
	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/darkfella/cni-plugins-install/pkg/checksum"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
	"github.com/darkfella/cni-plugins-install/pkg/fs"
	"github.com/darkfella/cni-plugins-install/pkg/version"
)

// operation represents a file operation during sync
type operation struct {
	sourcePath string
	targetPath string
	backupPath string
	backedUp   bool
	fileMode   os.FileMode
}

// Sync represents a file synchronizer
type Sync struct {
	logger     *logging.Logger
	fileSystem fs.FileSystem
	cleanup    *fs.Cleanup
	versionMgr *version.Manager
}

// NewSync creates a new file synchronizer
func NewSync(logger *logging.Logger) *Sync {
	return &Sync{
		logger:     logger,
		fileSystem: fs.NewFileSystem(logger),
		cleanup:    fs.NewCleanup(logger),
		versionMgr: version.NewManager(logger, constants.ManagedPlugins),
	}
}

// verifyDirectories checks if source and target directories exist and are valid
func (s *Sync) verifyDirectories(sourceDir, targetDir string) error {
	if err := s.fileSystem.VerifyDirectory(sourceDir); err != nil {
		return errors.Wrap(err, "verify source directory")
	}

	if err := s.fileSystem.VerifyDirectory(targetDir); err != nil {
		return errors.Wrap(err, "verify target directory")
	}

	return nil
}

// SyncFiles synchronizes files from source to target directory
func (s *Sync) SyncFiles(ctx context.Context, sourceDir, targetDir string) error {
	s.logger.Info().Str("source", sourceDir).Str("target", targetDir).Msg("Starting file synchronization")

	if err := s.verifyDirectories(sourceDir, targetDir); err != nil {
		return errors.Wrap(err, "verify directories")
	}

	// Create backup directory for existing files
	backupDir := filepath.Join(targetDir, fmt.Sprintf("%s%s", constants.BackupPrefix, time.Now().Format("20060102T150405")))
	if err := s.fileSystem.CreateDirectory(backupDir, constants.DirPerm); err != nil {
		return errors.NewOperationError("create backup directory", err)
	}
	s.logger.Info().Str("backup_dir", backupDir).Msg("Created backup directory")

	// Define operations slice
	var operations []operation
	var processedCount int // Count how many managed plugins were actually found and processed

	// Process each MANAGED plugin
	for pluginName := range constants.ManagedPlugins {
		select {
		case <-ctx.Done():
			s.logger.Info().Msg("Operation cancelled, restoring backups...")
			if err := s.restoreBackups(operations); err != nil {
				s.logger.Error().Err(err).Msg("Failed to restore backups after cancellation")
			}
			return fmt.Errorf("operation cancelled: %w", ctx.Err())
		default:
		}

		// Construct paths for the current managed plugin
		sourcePath := filepath.Join(sourceDir, pluginName)
		targetPath := filepath.Join(targetDir, pluginName)
		backupPath := filepath.Join(backupDir, pluginName)

		// Check if the MANAGED plugin file actually exists in the source directory
		if !s.fileSystem.FileExists(sourcePath) {
			s.logger.Debug().Str("plugin", pluginName).Msg("Managed plugin not found in source directory, skipping")
			continue
		}

		// Check if source is a directory (shouldn't happen for plugins, but safety check)
		if s.fileSystem.IsDirectory(sourcePath) {
			s.logger.Warn().Str("plugin", pluginName).Msg("Managed plugin path is a directory in source, skipping")
			continue
		}

		// Increment count of plugins found and processed
		processedCount++

		// Get source file mode
		sourceMode, err := s.fileSystem.GetFileMode(sourcePath)
		if err != nil {
			return errors.NewOperationError("get source file mode", err)
		}

		// Check if target file exists and has same checksum
		targetExists := s.fileSystem.FileExists(targetPath)
		var skipFile bool
		if targetExists {
			// Compare checksums first
			sameChecksum, err := s.sameChecksum(ctx, sourcePath, targetPath)
			if err != nil {
				s.logger.Warn().Err(err).Str("file", pluginName).Msg("Failed to compare checksums, proceeding with sync")
				skipFile = false // Proceed if checksum fails
			} else {
				// Check mode only if checksums are the same
				if sameChecksum {
					targetMode, err := s.fileSystem.GetFileMode(targetPath)
					if err != nil {
						// If we can't get target mode, proceed with sync to be safe
						s.logger.Warn().Err(err).Str("file", pluginName).Msg("Failed to get target file mode, proceeding with sync")
						skipFile = false
					} else {
						// Skip only if both checksum and mode match
						skipFile = (sourceMode&0777 == targetMode&0777)
					}
				} else {
					// Checksums differ, don't skip
					skipFile = false
				}
			}

			if skipFile {
				s.logger.Info().Str("file", pluginName).Msg("Skipping unchanged file (content and mode match)")
				continue
			}
		}

		// Create backup of existing file
		backedUp := false
		var originalTargetMode os.FileMode // Store original mode for potential restore
		if targetExists {                  // Use the check from earlier
			// Get target file mode before backup
			originalTargetMode, err = s.fileSystem.GetFileMode(targetPath)
			if err != nil {
				// Log warning but attempt backup anyway? Or return error? Let's return error.
				return errors.NewOperationError("get target file mode before backup", err)
			}

			if err := s.fileSystem.MoveFile(targetPath, backupPath); err != nil {
				if restoreErr := s.restoreBackups(operations); restoreErr != nil {
					s.logger.Error().Err(restoreErr).Msg("Failed to restore backups after backup failure")
				}
				return errors.NewOperationError("backup file", err)
			}
			backedUp = true
			s.logger.Info().Str("file", pluginName).Str("backup", backupPath).Msg("Backed up existing file")
		}

		// Move file from source to target
		if err := s.fileSystem.MoveFile(sourcePath, targetPath); err != nil {
			if restoreErr := s.restoreBackups(operations); restoreErr != nil {
				s.logger.Error().Err(restoreErr).Msg("Failed to restore backups after move failure")
			}
			return errors.NewOperationError("move file", err)
		}

		// Set correct file mode
		if err := s.fileSystem.SetFileMode(targetPath, sourceMode); err != nil {
			// s.logger.Warn().Err(err).Str("file", pluginName).Msg("Failed to set file mode")
			// If setting the mode fails, we should treat it as an error and restore
			if restoreErr := s.restoreBackups(operations); restoreErr != nil {
				s.logger.Error().Err(restoreErr).Msg("Failed to restore backups after SetFileMode failure")
			}
			// Also need to move the incorrectly moded target file back to source?
			// Or just return the error? Let's return error for now, restore handles backed up state.
			return errors.NewOperationError("set file mode", err)
		}

		operations = append(operations, operation{
			sourcePath: sourcePath,
			targetPath: targetPath,
			backupPath: backupPath,
			backedUp:   backedUp,
			fileMode:   originalTargetMode, // Use the stored original mode
		})
		s.logger.Info().Str("file", pluginName).Msg("Moved file to target directory")
	}

	s.logger.Info().Int("processed_count", processedCount).Int("managed_plugin_total", len(constants.ManagedPlugins)).Msg("Finished processing managed plugins")

	// Clean up backup directory if empty
	backupFiles, err := s.fileSystem.ListDirectory(backupDir)
	if err == nil && len(backupFiles) == 0 {
		s.logger.Info().Str("backup_dir", backupDir).Msg("Removing empty backup directory")
		if err := s.fileSystem.RemoveDirectory(backupDir); err != nil {
			s.logger.Warn().Err(err).Str("backup_dir", backupDir).Msg("Failed to clean up empty backup directory")
		}
	} else if err != nil {
		// Log if we couldn't list the backup dir to check if empty
		s.logger.Warn().Err(err).Str("backup_dir", backupDir).Msg("Failed to list backup directory for cleanup check")
	}

	s.logger.Info().Msg("File synchronization completed successfully")
	return nil
}

// restoreBackups restores all backed up files in reverse order
func (s *Sync) restoreBackups(operations []operation) error {
	for i := len(operations) - 1; i >= 0; i-- {
		op := operations[i]
		if op.backedUp {
			if err := s.fileSystem.MoveFile(op.backupPath, op.targetPath); err != nil {
				s.logger.Error().Err(err).Str("file", op.targetPath).Msg("Failed to restore backup")
				// Continue restoring other files even if one fails
				continue
			}
			s.logger.Info().Str("file", op.targetPath).Msg("Restored backup")
		}
	}
	return nil
}

// sameChecksum checks if two files have the same SHA256 checksum
func (s *Sync) sameChecksum(ctx context.Context, path1, path2 string) (bool, error) {
	return checksum.CompareFilesSHA256(ctx, path1, path2)
}

// Cleanup performs cleanup operations
func (s *Sync) Cleanup() error {
	return s.cleanup.Execute(s.fileSystem)
}

// SaveVersion saves the installed version information
func (s *Sync) SaveVersion(ctx context.Context, targetDir, version string) error {
	return s.versionMgr.SaveVersion(ctx, targetDir, version)
}

// CheckVersion checks if the current version is already installed
func (s *Sync) CheckVersion(ctx context.Context, targetDir, version string) (bool, error) {
	return s.versionMgr.CheckVersion(ctx, targetDir, version)
}

// VerifyPlugins verifies that all our managed plugins are valid and have correct permissions
func (s *Sync) VerifyPlugins(ctx context.Context, targetDir string) (bool, error) {
	return s.versionMgr.VerifyPlugins(ctx, targetDir)
}

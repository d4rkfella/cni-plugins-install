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

type operation struct {
	sourcePath string
	targetPath string
	backupPath string
	backedUp   bool
	fileMode   os.FileMode
}

type Sync struct {
	logger     *logging.Logger
	fileSystem fs.FileSystem
	cleanup    *fs.Cleanup
	versionMgr *version.Manager
}

func NewSync(logger *logging.Logger) *Sync {
	return &Sync{
		logger:     logger,
		fileSystem: fs.NewFileSystem(logger),
		cleanup:    fs.NewCleanup(logger),
		versionMgr: version.NewManager(logger, constants.ManagedPlugins),
	}
}

func (s *Sync) verifyDirectories(sourceDir, targetDir string) error {
	if err := s.fileSystem.VerifyDirectory(sourceDir); err != nil {
		return errors.Wrap(err, "verify source directory")
	}

	if err := s.fileSystem.VerifyDirectory(targetDir); err != nil {
		return errors.Wrap(err, "verify target directory")
	}

	return nil
}

func (s *Sync) SyncFiles(ctx context.Context, sourceDir, targetDir string) error {
	s.logger.Info().Str("source", sourceDir).Str("target", targetDir).Msg("Starting file synchronization")

	if err := s.verifyDirectories(sourceDir, targetDir); err != nil {
		return errors.Wrap(err, "verify directories")
	}

	backupDir := filepath.Join(targetDir, fmt.Sprintf("%s%s", constants.BackupPrefix, time.Now().Format("20060102T150405")))
	if err := s.fileSystem.CreateDirectory(backupDir, constants.DirPerm); err != nil {
		return errors.NewOperationError("create backup directory", err)
	}
	s.logger.Info().Str("backup_dir", backupDir).Msg("Created backup directory")

	var operations []operation
	var processedCount int

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

		sourcePath := filepath.Join(sourceDir, pluginName)
		targetPath := filepath.Join(targetDir, pluginName)
		backupPath := filepath.Join(backupDir, pluginName)

		if !s.fileSystem.FileExists(sourcePath) {
			s.logger.Debug().Str("plugin", pluginName).Msg("Managed plugin not found in source directory, skipping")
			continue
		}

		if s.fileSystem.IsDirectory(sourcePath) {
			s.logger.Warn().Str("plugin", pluginName).Msg("Managed plugin source path is a directory, skipping")
			continue
		}

		processedCount++

		sourceMode, err := s.fileSystem.GetFileMode(sourcePath)
		if err != nil {
			return errors.NewOperationError("get source file mode", err)
		}

		targetExists := s.fileSystem.FileExists(targetPath)
		var skipFile bool
		if targetExists {
			sameChecksum, err := s.sameChecksum(ctx, sourcePath, targetPath)
			if err != nil {
				s.logger.Warn().Err(err).Str("file", pluginName).Msg("Failed to compare checksums, proceeding with sync")
				skipFile = false
			} else {
				if sameChecksum {
					targetMode, err := s.fileSystem.GetFileMode(targetPath)
					if err != nil {
						s.logger.Warn().Err(err).Str("file", pluginName).Msg("Failed to get target file mode, proceeding with sync")
						skipFile = false
					} else {
						skipFile = (sourceMode&0777 == targetMode&0777)
					}
				} else {
					skipFile = false
				}
			}

			if skipFile {
				s.logger.Info().Str("file", pluginName).Msg("Skipping unchanged file (content and mode match)")
				continue
			}
		}

		backedUp := false
		var originalTargetMode os.FileMode
		if targetExists {
			originalTargetMode, err = s.fileSystem.GetFileMode(targetPath)
			if err != nil {
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

		if err := s.fileSystem.MoveFile(sourcePath, targetPath); err != nil {
			if restoreErr := s.restoreBackups(operations); restoreErr != nil {
				s.logger.Error().Err(restoreErr).Msg("Failed to restore backups after move failure")
			}
			return errors.NewOperationError("move file", err)
		}

		if err := s.fileSystem.SetFileMode(targetPath, sourceMode); err != nil {
			if restoreErr := s.restoreBackups(operations); restoreErr != nil {
				s.logger.Error().Err(restoreErr).Msg("Failed to restore backups after SetFileMode failure")
			}
			return errors.NewOperationError("set file mode", err)
		}

		operations = append(operations, operation{
			sourcePath: sourcePath,
			targetPath: targetPath,
			backupPath: backupPath,
			backedUp:   backedUp,
			fileMode:   originalTargetMode,
		})
		s.logger.Info().Str("file", pluginName).Msg("Moved file to target directory")
	}

	s.logger.Info().Int("processed_count", processedCount).Int("managed_plugin_total", len(constants.ManagedPlugins)).Msg("Finished processing managed plugins")

	if err := s.fileSystem.RemoveDirectory(backupDir); err != nil {
	    s.logger.Warn().
	        Err(err).
	        Str("backup_dir", backupDir).
	        Msg("Failed to clean up backup directory")
	} else {
	    s.logger.Info().
	        Str("backup_dir", backupDir).
	        Msg("Removed backup directory")
	}

	s.logger.Info().Msg("File synchronization completed successfully")
	return nil
}

func (s *Sync) restoreBackups(operations []operation) error {
	for i := len(operations) - 1; i >= 0; i-- {
		op := operations[i]
		if op.backedUp {
			if err := s.fileSystem.MoveFile(op.backupPath, op.targetPath); err != nil {
				s.logger.Error().Err(err).Str("file", op.targetPath).Msg("Failed to restore backup")
				continue
			}
			s.logger.Info().Str("file", op.targetPath).Msg("Restored backup")
		}
	}
	return nil
}

func (s *Sync) sameChecksum(ctx context.Context, path1, path2 string) (bool, error) {
	return checksum.CompareFilesSHA256(ctx, path1, path2)
}

func (s *Sync) SaveVersion(ctx context.Context, targetDir, version string) error {
	return s.versionMgr.SaveVersion(ctx, targetDir, version)
}

func (s *Sync) CheckVersion(ctx context.Context, targetDir, version string) (bool, error) {
	return s.versionMgr.CheckVersion(ctx, targetDir, version)
}

func (s *Sync) VerifyPlugins(ctx context.Context, targetDir string) (bool, error) {
	return s.versionMgr.VerifyPlugins(ctx, targetDir)
}

package version

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/constants"
	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/darkfella/cni-plugins-install/pkg/checksum"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
	"github.com/darkfella/cni-plugins-install/pkg/fs"
)

type Info struct {
	Version     string            `json:"version"`
	InstalledAt time.Time         `json:"installed_at"`
	Plugins     map[string]string `json:"plugins"` // plugin name -> checksum
}

type Manager struct {
	logger     *logging.Logger
	fileSystem fs.FileSystem
	plugins    map[string]bool
}

func NewManager(logger *logging.Logger, plugins map[string]bool) *Manager {
	return &Manager{
		logger:     logger,
		fileSystem: fs.NewFileSystem(logger),
		plugins:    plugins,
	}
}

func (m *Manager) SaveVersion(ctx context.Context, targetDir, version string) error {
	files, err := m.fileSystem.ListDirectory(targetDir)
	if err != nil {
		return errors.Wrap(err, "list directory")
	}

	plugins := make(map[string]string)
	for _, file := range files {
		if !m.plugins[file] {
			continue
		}

		path := filepath.Join(targetDir, file)
		if m.fileSystem.IsDirectory(path) {
			continue
		}

		checksum, err := checksum.CalculateFileSHA256(ctx, path)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("calculate checksum for %s", file))
		}
		plugins[file] = checksum
	}

	info := Info{
		Version:     version,
		InstalledAt: time.Now(),
		Plugins:     plugins,
	}

	data, err := json.Marshal(info)
	if err != nil {
		return errors.Wrap(err, "marshal version info")
	}

	versionFile := filepath.Join(targetDir, ".version")
	if err := m.fileSystem.WriteFileAtomic(versionFile, strings.NewReader(string(data)), constants.FilePerm); err != nil {
		return errors.Wrap(err, "write version file")
	}

	return nil
}

func (m *Manager) CheckVersion(ctx context.Context, targetDir, version string) (bool, error) {
	versionFile := filepath.Join(targetDir, ".version")
	if !m.fileSystem.FileExists(versionFile) {
		return false, nil
	}

	data, err := os.ReadFile(versionFile)
	if err != nil {
		m.logger.Warn().Err(err).Str("file", versionFile).Msg("Failed to read existing .version file, proceeding with installation")
		return false, nil
	}

	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return false, errors.Wrap(err, "parse version file")
	}

	if info.Version != version {
		m.logger.Info().Str("current_version", info.Version).Str("requested_version", version).Msg("Version mismatch, will install new version")
		return false, nil
	}

	return true, nil
}

func (m *Manager) VerifyPlugins(ctx context.Context, targetDir string) (bool, error) {
	var installedInfo Info
	checksumsVerified := false

	versionFile := filepath.Join(targetDir, ".version")
	if m.fileSystem.FileExists(versionFile) {
		data, err := os.ReadFile(versionFile)
		if err != nil {
			return false, errors.Wrap(err, "read version file")
		}
		if err := json.Unmarshal(data, &installedInfo); err != nil {
			return false, errors.Wrap(err, "parse version file")
		}
		checksumsVerified = true
	}

	for pluginName := range m.plugins {
		path := filepath.Join(targetDir, pluginName)

		if !m.fileSystem.FileExists(path) {
			m.logger.Warn().Str("file", pluginName).Msg("Managed plugin file does not exist")
			return false, nil
		}

		if m.fileSystem.IsDirectory(path) {
			m.logger.Warn().Str("file", pluginName).Msg("Managed plugin path is a directory")
			continue
		}

		if checksumsVerified {
			expectedHash, ok := installedInfo.Plugins[pluginName]
			if !ok {
				m.logger.Warn().Str("file", pluginName).Msg("Plugin missing from version file checksums")
				return false, nil
			}
			if err := checksum.VerifyFileSHA256(ctx, path, expectedHash); err != nil {
				m.logger.Warn().Err(err).Str("file", pluginName).Msg("Plugin checksum mismatch")
				return false, nil
			}
		}

		isExec, err := m.fileSystem.IsExecutable(path)
		if err != nil {
			return false, errors.Wrap(err, fmt.Sprintf("checking executable status for %s", pluginName))
		}
		if !isExec {
			m.logger.Warn().Str("file", pluginName).Msg("Plugin file is not executable")
			return false, nil
		}
	}

	return true, nil
}

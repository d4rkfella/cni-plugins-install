package fs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
	"github.com/darkfella/cni-plugins-install/pkg/validator"
)

// FileSystem defines the interface for filesystem operations
type FileSystem interface {
	ListDirectory(path string) ([]string, error)
	IsDirectory(path string) bool
	FileExists(path string) bool
	WriteFileAtomic(path string, data io.Reader, perm os.FileMode) error
	IsExecutable(path string) (bool, error)
	CreateDirectory(path string, perm os.FileMode) error
	RemoveDirectory(path string) error
	GetFileSize(path string) (int64, error)
	GetFileMode(path string) (os.FileMode, error)
	SetFileMode(path string, mode os.FileMode) error
	MoveFile(src, dst string) error
	SecureRemove(path string) error
	VerifyDirectory(path string) error
	VerifyFile(path string) error
}

// fileSystem implements the FileSystem interface using standard os calls
type fileSystem struct {
	logger    *logging.Logger
	validator *validator.Validator
}

// NewFileSystem creates a new FileSystem instance
func NewFileSystem(logger *logging.Logger) FileSystem {
	return &fileSystem{
		logger:    logger,
		validator: validator.NewValidator(logger),
	}
}

// CreateDirectory creates a directory and its parents if they don't exist
func (fs *fileSystem) CreateDirectory(path string, perm os.FileMode) error {
	if err := fs.validator.ValidatePath(path); err != nil {
		return err
	}
	return os.MkdirAll(path, perm)
}

// RemoveDirectory removes a directory and its contents
func (fs *fileSystem) RemoveDirectory(path string) error {
	if err := fs.validator.ValidatePath(path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// ListDirectory lists the contents of a directory
func (fs *fileSystem) ListDirectory(path string) (names []string, err error) {
	if err := fs.validator.ValidateDirectory(path); err != nil {
		return nil, err
	}

	dir, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrap(err, "open directory")
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(closeErr, "close directory")
		}
	}()

	names, err = dir.Readdirnames(-1)
	if err != nil {
		return nil, errors.Wrap(err, "read directory names")
	}

	return names, nil
}

// FileExists checks if a file exists
func (fs *fileSystem) FileExists(path string) bool {
	_, err := os.Lstat(path)
	return !os.IsNotExist(err)
}

// IsDirectory checks if a path is a directory
func (fs *fileSystem) IsDirectory(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// GetFileSize returns the size of a file
func (fs *fileSystem) GetFileSize(path string) (int64, error) {
	if err := fs.validator.ValidateFile(path); err != nil {
		return 0, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return 0, errors.Wrap(err, "stat file")
	}

	return info.Size(), nil
}

// GetFileMode returns the mode of a file
func (fs *fileSystem) GetFileMode(path string) (os.FileMode, error) {
	if err := fs.validator.ValidatePath(path); err != nil {
		return 0, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return 0, errors.Wrap(err, "stat file")
	}

	return info.Mode(), nil
}

// SetFileMode sets the mode of a file
func (fs *fileSystem) SetFileMode(path string, mode os.FileMode) error {
	if err := fs.validator.ValidatePath(path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// MoveFile moves a file from source to destination
func (fs *fileSystem) MoveFile(src, dst string) error {
	if err := fs.validator.ValidatePath(src); err != nil {
		return err
	}
	if err := fs.validator.ValidatePath(dst); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// WriteFileAtomic writes data to a file atomically using a temporary file
func (fs *fileSystem) WriteFileAtomic(path string, data io.Reader, perm os.FileMode) error {
	if err := fs.validator.ValidatePath(path); err != nil {
		return err
	}
	// Add nil reader check
	if data == nil {
		return errors.NewOperationError("write file atomic", fmt.Errorf("input reader cannot be nil"))
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tempFile, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return errors.Wrap(err, "create temporary file")
	}
	// Ensure temp file is closed and removed even if errors occur later
	defer func() {
		if closeErr := tempFile.Close(); closeErr != nil {
			fs.logger.Warn().Err(closeErr).Str("file", tempFile.Name()).Msg("Error closing temp file during cleanup")
		}
		if removeErr := fs.SecureRemove(tempFile.Name()); removeErr != nil {
			fs.logger.Warn().Err(removeErr).Str("file", tempFile.Name()).Msg("Error removing temp file during cleanup")
		}
	}()

	if err := os.Chmod(tempFile.Name(), perm); err != nil {
		return errors.Wrap(err, "chmod temporary file")
	}

	if _, err := io.Copy(tempFile, data); err != nil {
		return errors.Wrap(err, "write to temporary file")
	}

	if err := tempFile.Sync(); err != nil {
		return errors.Wrap(err, "sync temporary file")
	}

	// Atomically rename temp file to target path
	if err := os.Rename(tempFile.Name(), path); err != nil {
		return errors.Wrap(err, "rename temporary file")
	}

	return nil
}

// SecureRemove removes a file securely by first making it writable if necessary
func (fs *fileSystem) SecureRemove(path string) error {
	if path == "" {
		return fmt.Errorf("remove file: path is empty")
	}

	// Check if file exists
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			fs.logger.Debug().Str("path", path).Msg("File does not exist, skipping removal")
			return nil // File doesn't exist, nothing to remove
		}
		return errors.Wrap(err, "stat file")
	}

	// If file is not writable, make it writable first
	if info.Mode()&0200 == 0 {
		fs.logger.Warn().Str("path", path).Msg("File is not writable, attempting to make it writable")
		if err := os.Chmod(path, info.Mode()|0200); err != nil {
			return errors.Wrap(err, "make file writable")
		}
	}

	return os.Remove(path)
}

// VerifyDirectory verifies that a path exists and is a directory
func (fs *fileSystem) VerifyDirectory(path string) error {
	return fs.validator.ValidateDirectory(path)
}

// VerifyFile verifies that a path exists and is a file
func (fs *fileSystem) VerifyFile(path string) error {
	return fs.validator.ValidateFile(path)
}

// IsExecutable checks if a file is executable
func (fs *fileSystem) IsExecutable(path string) (bool, error) {
	if err := fs.validator.ValidateFile(path); err != nil {
		return false, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return false, errors.Wrap(err, "stat file")
	}

	mode := info.Mode()
	if mode&0111 == 0 {
		return false, nil
	}

	return true, nil
}

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

type fileSystem struct {
	logger    *logging.Logger
	validator *validator.Validator
}

func NewFileSystem(logger *logging.Logger) FileSystem {
	return &fileSystem{
		logger:    logger,
		validator: validator.NewValidator(logger),
	}
}

func (fs *fileSystem) CreateDirectory(path string, perm os.FileMode) error {
	if err := fs.validator.ValidatePath(path); err != nil {
		return err
	}
	return os.MkdirAll(path, perm)
}

func (fs *fileSystem) RemoveDirectory(path string) error {
	if err := fs.validator.ValidatePath(path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

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

func (fs *fileSystem) FileExists(path string) bool {
	_, err := os.Lstat(path)
	return !os.IsNotExist(err)
}

func (fs *fileSystem) IsDirectory(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

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

func (fs *fileSystem) SetFileMode(path string, mode os.FileMode) error {
	if err := fs.validator.ValidatePath(path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func (fs *fileSystem) MoveFile(src, dst string) error {
	if err := fs.validator.ValidatePath(src); err != nil {
		return err
	}
	if err := fs.validator.ValidatePath(dst); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func (fs *fileSystem) WriteFileAtomic(path string, data io.Reader, perm os.FileMode) error {
	if err := fs.validator.ValidatePath(path); err != nil {
		return err
	}
	if data == nil {
		return errors.NewOperationError("write file atomic", fmt.Errorf("input reader cannot be nil"))
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tempFile, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return errors.Wrap(err, "create temporary file")
	}
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

	if err := os.Rename(tempFile.Name(), path); err != nil {
		return errors.Wrap(err, "rename temporary file")
	}

	return nil
}

func (fs *fileSystem) SecureRemove(path string) error {
	if path == "" {
		return fmt.Errorf("remove file: path is empty")
	}

	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			fs.logger.Debug().Str("path", path).Msg("File does not exist, skipping removal")
			return nil
		}
		return errors.Wrap(err, "stat file")
	}

	if info.Mode()&0200 == 0 {
		fs.logger.Warn().Str("path", path).Msg("File is not writable, attempting to make it writable")
		if err := os.Chmod(path, info.Mode()|0200); err != nil {
			return errors.Wrap(err, "make file writable")
		}
	}

	return os.Remove(path)
}

func (fs *fileSystem) VerifyDirectory(path string) error {
	return fs.validator.ValidateDirectory(path)
}

func (fs *fileSystem) VerifyFile(path string) error {
	return fs.validator.ValidateFile(path)
}

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

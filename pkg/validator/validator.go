package validator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
)

type Validator struct {
	logger *logging.Logger
}

func NewValidator(logger *logging.Logger) *Validator {
	return &Validator{
		logger: logger,
	}
}

func (v *Validator) ValidateRoot() error {
	if os.Geteuid() != 0 {
		return errors.NewOperationError("validate root", fmt.Errorf("must run as root"))
	}
	return nil
}

func (v *Validator) ValidatePlatform(platform string) error {
	parts := strings.Split(platform, "/")
	if len(parts) != 2 {
		return errors.NewOperationError("validate platform", fmt.Errorf("invalid platform format: %s", platform))
	}

	os, arch := parts[0], parts[1]
	if os != "linux" {
		return errors.NewOperationError("validate platform", fmt.Errorf("unsupported operating system: %s", os))
	}

	supportedArchs := map[string]bool{
		"amd64": true,
		"arm64": true,
	}

	if !supportedArchs[arch] {
		return errors.NewOperationError("validate platform", fmt.Errorf("unsupported architecture: %s", arch))
	}

	return nil
}

func (v *Validator) ValidatePath(path string) error {
	if path == "" {
		return errors.NewOperationError("validate path", fmt.Errorf("path is empty"))
	}

	if !filepath.IsAbs(path) {
		return errors.NewOperationError("validate path", fmt.Errorf("path is not absolute: %s", path))
	}

	return nil
}

func (v *Validator) ValidateDirectory(path string) error {
	if err := v.ValidatePath(path); err != nil {
		return err
	}

	info, err := os.Stat(path)
	if err != nil {
		return errors.Wrap(err, "stat directory")
	}

	if !info.IsDir() {
		return errors.NewOperationError("validate directory", fmt.Errorf("not a directory: %s", path))
	}

	if info.Mode()&0200 == 0 {
		return errors.NewOperationError("validate directory", fmt.Errorf("directory is not writable: %s", path))
	}

	return nil
}

func (v *Validator) ValidateFile(path string) error {
	if err := v.ValidatePath(path); err != nil {
		return err
	}

	info, err := os.Stat(path)
	if err != nil {
		return errors.Wrap(err, "stat file")
	}

	if info.IsDir() {
		return errors.NewOperationError("validate file", fmt.Errorf("not a file: %s", path))
	}

	if info.Mode()&0400 == 0 {
		return errors.NewOperationError("validate file", fmt.Errorf("file is not readable: %s", path))
	}

	return nil
}

func (v *Validator) ValidateExecutable(path string) error {
	if err := v.ValidateFile(path); err != nil {
		return err
	}

	info, err := os.Stat(path)
	if err != nil {
		return errors.Wrap(err, "stat file")
	}

	if info.Mode()&0111 == 0 {
		return errors.NewOperationError("validate executable", fmt.Errorf("file is not executable: %s", path))
	}

	return nil
}

func (v *Validator) ValidateWritable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return errors.Wrap(err, "stat path")
	}

	if info.IsDir() && info.Mode()&0200 == 0 {
		return errors.NewOperationError("validate writable", fmt.Errorf("directory is not writable: %s", path))
	}

	if !info.IsDir() && info.Mode()&0200 == 0 {
		return errors.NewOperationError("validate writable", fmt.Errorf("file is not writable: %s", path))
	}

	return nil
}

func (v *Validator) ValidateVersion(version string) error {
	if version == "" {
		return errors.NewOperationError("validate version", fmt.Errorf("version is empty"))
	}

	if !strings.HasPrefix(version, "v") {
		return errors.NewOperationError("validate version", fmt.Errorf("version must start with 'v': %s", version))
	}

	parts := strings.Split(version[1:], ".")
	if len(parts) != 3 {
		return errors.NewOperationError("validate version", fmt.Errorf("invalid version format: %s", version))
	}

	return nil
}

func (v *Validator) ValidateURL(url string) error {
	if url == "" {
		return errors.NewOperationError("validate url", fmt.Errorf("url is empty"))
	}

	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return errors.NewOperationError("validate url", fmt.Errorf("invalid url scheme: %s", url))
	}

	return nil
}

func (v *Validator) ValidateConfig(config interface{}) error {
	return nil
}

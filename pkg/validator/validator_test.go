package validator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/darkfella/cni-plugins-install/internal/logging"
)

func TestNewValidator(t *testing.T) {
	logger := logging.NewLogger()
	v := NewValidator(logger)
	if v == nil {
		t.Error("NewValidator() returned nil")
		return
	}
	if v.logger != logger {
		t.Error("NewValidator() did not set logger correctly")
	}
}

func TestValidatePlatform(t *testing.T) {
	v := NewValidator(logging.NewLogger())
	tests := []struct {
		name      string
		platform  string
		wantError bool
	}{
		{
			name:      "valid linux/amd64",
			platform:  "linux/amd64",
			wantError: false,
		},
		{
			name:      "valid linux/arm64",
			platform:  "linux/arm64",
			wantError: false,
		},
		{
			name:      "invalid os",
			platform:  "windows/amd64",
			wantError: true,
		},
		{
			name:      "invalid arch",
			platform:  "linux/x86",
			wantError: true,
		},
		{
			name:      "invalid format",
			platform:  "linux-amd64",
			wantError: true,
		},
		{
			name:      "empty string",
			platform:  "",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidatePlatform(tt.platform)
			if (err != nil) != tt.wantError {
				t.Errorf("ValidatePlatform() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestValidatePath(t *testing.T) {
	v := NewValidator(logging.NewLogger())
	tests := []struct {
		name      string
		path      string
		wantError bool
	}{
		{
			name:      "valid absolute path",
			path:      "/tmp/test",
			wantError: false,
		},
		{
			name:      "relative path",
			path:      "test/path",
			wantError: true,
		},
		{
			name:      "empty path",
			path:      "",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidatePath(tt.path)
			if (err != nil) != tt.wantError {
				t.Errorf("ValidatePath() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestValidateDirectory(t *testing.T) {
	v := NewValidator(logging.NewLogger())

	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "validator-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Logf("Error cleaning up temp dir %s: %v", tempDir, err)
		}
	}()

	// Create a temporary file for testing
	tempFile := filepath.Join(tempDir, "test.txt")
	if err := os.WriteFile(tempFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	tests := []struct {
		name      string
		path      string
		wantError bool
	}{
		{
			name:      "valid directory",
			path:      tempDir,
			wantError: false,
		},
		{
			name:      "file instead of directory",
			path:      tempFile,
			wantError: true,
		},
		{
			name:      "non-existent directory",
			path:      "/non/existent/dir",
			wantError: true,
		},
	}

	// Add a case for non-writable directory
	readOnlyDir := filepath.Join(tempDir, "readonly-dir")
	require.NoError(t, os.Mkdir(readOnlyDir, 0555)) // Create read-only directory
	tests = append(tests, struct {
		name      string
		path      string
		wantError bool
	}{
		name:      "non-writable directory",
		path:      readOnlyDir,
		wantError: true,
	})
	// Ensure permissions are restored for cleanup
	defer func() {
		_ = os.Chmod(readOnlyDir, 0755)
	}()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateDirectory(tt.path)
			if (err != nil) != tt.wantError {
				t.Errorf("ValidateDirectory() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestValidateFile(t *testing.T) {
	v := NewValidator(logging.NewLogger())

	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "validator-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Logf("Error cleaning up temp dir %s: %v", tempDir, err)
		}
	}()

	// Create test files with different permissions
	readableFile := filepath.Join(tempDir, "readable.txt")
	if err := os.WriteFile(readableFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create readable file: %v", err)
	}

	// Create an unreadable file for testing
	unreadableFile := filepath.Join(tempDir, "unreadable.txt")
	require.NoError(t, os.WriteFile(unreadableFile, []byte("cant read me"), 0200)) // Write-only
	// Ensure permissions are restored for cleanup
	defer func() {
		_ = os.Chmod(unreadableFile, 0600)
	}()

	tests := []struct {
		name      string
		path      string
		wantError bool
	}{
		{
			name:      "valid file",
			path:      readableFile,
			wantError: false,
		},
		{
			name:      "directory instead of file",
			path:      tempDir,
			wantError: true,
		},
		{
			name:      "non-existent file",
			path:      "/non/existent/file.txt",
			wantError: true,
		},
		{
			name:      "unreadable file",
			path:      unreadableFile,
			wantError: true,
		},
	}

	// Add a case for a directory path
	tests = append(tests, struct {
		name      string
		path      string
		wantError bool
	}{
		name:      "directory path",
		path:      tempDir, // Use the temp directory created in setup
		wantError: true,    // ValidateFile should fail as it's not a file
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateFile(tt.path)
			if (err != nil) != tt.wantError {
				t.Errorf("ValidateFile() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestValidateExecutable(t *testing.T) {
	v := NewValidator(logging.NewLogger())

	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "validator-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Logf("Error cleaning up temp dir %s: %v", tempDir, err)
		}
	}()

	// Create test files with different permissions
	executableFile := filepath.Join(tempDir, "executable")
	if err := os.WriteFile(executableFile, []byte("#!/bin/sh\necho test"), 0755); err != nil {
		t.Fatalf("Failed to create executable file: %v", err)
	}

	nonExecutableFile := filepath.Join(tempDir, "non-executable")
	if err := os.WriteFile(nonExecutableFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create non-executable file: %v", err)
	}

	tests := []struct {
		name      string
		path      string
		wantError bool
	}{
		{
			name:      "executable file",
			path:      executableFile,
			wantError: false,
		},
		{
			name:      "non-executable file",
			path:      nonExecutableFile,
			wantError: true,
		},
		{
			name:      "non-existent file",
			path:      "/non/existent/executable",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateExecutable(tt.path)
			if (err != nil) != tt.wantError {
				t.Errorf("ValidateExecutable() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestValidateWritable(t *testing.T) {
	v := NewValidator(logging.NewLogger())

	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "validator-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Logf("Error cleaning up temp dir %s: %v", tempDir, err)
		}
	}()

	// Create test files with different permissions
	writableFile := filepath.Join(tempDir, "writable.txt")
	if err := os.WriteFile(writableFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create writable file: %v", err)
	}

	readOnlyFile := filepath.Join(tempDir, "readonly.txt")
	if err := os.WriteFile(readOnlyFile, []byte("test"), 0444); err != nil {
		t.Fatalf("Failed to create read-only file: %v", err)
	}

	tests := []struct {
		name      string
		path      string
		wantError bool
	}{
		{
			name:      "writable file",
			path:      writableFile,
			wantError: false,
		},
		{
			name:      "read-only file",
			path:      readOnlyFile,
			wantError: true,
		},
		{
			name:      "writable directory",
			path:      tempDir,
			wantError: false,
		},
		{
			name:      "non-existent path",
			path:      "/non/existent/path",
			wantError: true,
		},
	}

	// Add case for read-only directory
	readOnlyDir := filepath.Join(tempDir, "readonly-dir")
	require.NoError(t, os.Mkdir(readOnlyDir, 0555)) // Create read-only directory
	tests = append(tests, struct {
		name      string
		path      string
		wantError bool
	}{
		name:      "read-only directory",
		path:      readOnlyDir,
		wantError: true,
	})
	// Ensure permissions are restored for cleanup
	defer func() {
		_ = os.Chmod(readOnlyDir, 0755)
	}()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateWritable(tt.path)
			if (err != nil) != tt.wantError {
				t.Errorf("ValidateWritable() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestValidateVersion(t *testing.T) {
	v := NewValidator(logging.NewLogger())
	tests := []struct {
		name      string
		version   string
		wantError bool
	}{
		{
			name:      "valid version",
			version:   "v1.2.3",
			wantError: false,
		},
		{
			name:      "missing v prefix",
			version:   "1.2.3",
			wantError: true,
		},
		{
			name:      "invalid format",
			version:   "v1.2",
			wantError: true,
		},
		{
			name:      "empty version",
			version:   "",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateVersion(tt.version)
			if (err != nil) != tt.wantError {
				t.Errorf("ValidateVersion() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestValidateURL(t *testing.T) {
	v := NewValidator(logging.NewLogger())
	tests := []struct {
		name      string
		url       string
		wantError bool
	}{
		{
			name:      "valid http url",
			url:       "http://example.com",
			wantError: false,
		},
		{
			name:      "valid https url",
			url:       "https://example.com",
			wantError: false,
		},
		{
			name:      "invalid scheme",
			url:       "ftp://example.com",
			wantError: true,
		},
		{
			name:      "empty url",
			url:       "",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateURL(tt.url)
			if (err != nil) != tt.wantError {
				t.Errorf("ValidateURL() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestValidateRoot(t *testing.T) {
	v := NewValidator(logging.NewLogger())

	// Since we can't reliably test root privileges in a test environment,
	// we'll just verify that the function returns an error when not root
	err := v.ValidateRoot()
	if os.Geteuid() == 0 {
		if err != nil {
			t.Errorf("ValidateRoot() returned error when running as root: %v", err)
		}
	} else {
		if err == nil {
			t.Error("ValidateRoot() did not return error when not running as root")
		}
	}
}

func TestValidateConfig(t *testing.T) {
	v := NewValidator(logging.NewLogger())

	// Since ValidateConfig is a TODO, we'll just verify it returns nil for now
	err := v.ValidateConfig(nil)
	if err != nil {
		t.Errorf("ValidateConfig() returned error: %v", err)
	}
}

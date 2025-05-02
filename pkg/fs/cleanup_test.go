package fs

import (
	"context"
	goerrors "errors" // Alias standard Go errors
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestCleanup creates necessary components for cleanup tests.
func setupTestCleanup(t *testing.T) (*Cleanup, FileSystem, string) {
	t.Helper()
	logger := logging.NewLogger()
	// Use DefaultFileSystem as the concrete implementation for testing
	fs := NewFileSystem(logger)
	cleanup := NewCleanup(logger)

	// Create a temporary directory for testing artifacts
	tempDir, err := os.MkdirTemp("", "cleanup-test-*")
	require.NoError(t, err, "Failed to create temp directory")

	return cleanup, fs, tempDir
}

// cleanupTest removes the temporary directory created for testing.
func cleanupTest(t *testing.T, tempDir string) {
	t.Helper()
	// Ensure the directory is writable before removing (for permission tests)
	_ = filepath.Walk(tempDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Ignore errors during walk
		}
		if info.Mode().Perm()&0200 == 0 { // Check if write bit is not set
			_ = os.Chmod(path, info.Mode()|0200) // Add write permission
		}
		return nil
	})
	err := os.RemoveAll(tempDir)
	require.NoError(t, err, "Failed to remove temp directory")
}

// TestNewCleanup verifies the constructor initializes the struct correctly.
func TestNewCleanup(t *testing.T) {
	logger := logging.NewLogger()
	cleanup := NewCleanup(logger)

	assert.NotNil(t, cleanup, "Cleanup instance should not be nil")
	assert.Equal(t, logger, cleanup.logger, "Logger should be set")
	assert.NotNil(t, cleanup.validator, "Validator should be initialized")
	assert.NotNil(t, cleanup.items, "Items slice should be initialized (empty)")
	assert.Empty(t, cleanup.items, "Items slice should be empty initially")
	assert.NotNil(t, cleanup.results, "Results slice should be initialized (empty)")
	assert.Empty(t, cleanup.results, "Results slice should be empty initially")
}

// Helper to create a test file
func createTestFile(t *testing.T, path string, content string, mode os.FileMode) {
	t.Helper()
	err := os.WriteFile(path, []byte(content), mode)
	require.NoError(t, err, "Failed to create test file %s", path)
}

// Helper to create a test directory
func createTestDir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	err := os.Mkdir(path, mode)
	require.NoError(t, err, "Failed to create test directory %s", path)
}

// TestAddItem verifies items are added to the internal queue.
func TestAddItem(t *testing.T) {
	cleanup, _, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	testPath := filepath.Join(tempDir, "testfile.txt")
	cleanup.AddItem(testPath, "file", 1)

	cleanup.mu.Lock() // Lock to safely access internal state for test verification
	require.Len(t, cleanup.items, 1, "Should have one item after AddItem")
	assert.Equal(t, testPath, cleanup.items[0].Path)
	assert.Equal(t, "file", cleanup.items[0].Type)
	assert.Equal(t, 1, cleanup.items[0].Priority)
	cleanup.mu.Unlock()
}

// TestAddDirectory verifies the AddDirectory helper.
func TestAddDirectory(t *testing.T) {
	cleanup, _, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	testPath := filepath.Join(tempDir, "testdir")
	cleanup.AddDirectory(testPath)

	cleanup.mu.Lock()
	require.Len(t, cleanup.items, 1, "Should have one item after AddDirectory")
	assert.Equal(t, testPath, cleanup.items[0].Path)
	assert.Equal(t, "directory", cleanup.items[0].Type)
	assert.Equal(t, 0, cleanup.items[0].Priority) // Default priority for AddDirectory
	cleanup.mu.Unlock()
}

// TestExecute_Success tests successful cleanup of files and directories.
func TestExecute_Success(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create items to clean
	testFile := filepath.Join(tempDir, "file_to_delete.txt")
	testDir := filepath.Join(tempDir, "dir_to_delete")
	tempFile := filepath.Join(tempDir, "temp_to_delete.tmp")

	createTestFile(t, testFile, "content", 0644)
	createTestDir(t, testDir, 0755)
	createTestFile(t, tempFile, "temp content", 0644)

	// Add items
	cleanup.AddItem(testFile, "file", 0)
	cleanup.AddDirectory(testDir) // Uses AddDirectory helper
	cleanup.AddItem(tempFile, "temp", 1)

	// Execute cleanup
	err := cleanup.Execute(fs)
	require.NoError(t, err, "Execute should succeed when all items are cleaned")

	// Verify items are gone
	assert.False(t, fs.FileExists(testFile), "Test file should be deleted")
	assert.False(t, fs.FileExists(testDir), "Test directory should be deleted")
	assert.False(t, fs.FileExists(tempFile), "Temp file should be deleted")

	// Verify internal results (though not publicly accessible)
	cleanup.mu.Lock()
	assert.Len(t, cleanup.results, 3, "Should have results for all 3 items")
	for _, res := range cleanup.results {
		assert.True(t, res.Success, "Result should indicate success for path: %s", res.Path)
		assert.NoError(t, res.Error, "Result error should be nil for path: %s", res.Path)
	}
	assert.Greater(t, cleanup.cleanupTime, time.Duration(0), "Cleanup time should be recorded")
	assert.Equal(t, 3, cleanup.cleanupCount, "Cleanup count should be 3")
	cleanup.mu.Unlock()
}

// TestExecuteWithContext_Cancel tests cleanup cancellation via context.
func TestExecuteWithContext_Cancel(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create many files to increase chance of cancellation during execution
	numFiles := 20
	paths := make([]string, numFiles)
	for i := 0; i < numFiles; i++ {
		path := filepath.Join(tempDir, fmt.Sprintf("cancel_test_%d.txt", i))
		createTestFile(t, path, "delete me", 0644)
		cleanup.AddItem(path, "file", 0)
		paths[i] = path
	}

	// Create a context that cancels very quickly
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	// Execute cleanup
	err := cleanup.ExecuteWithContext(ctx, fs)
	require.Error(t, err, "Execute should return an error when context is cancelled")
	// Check if the error is context.DeadlineExceeded or context.Canceled
	assert.True(t, goerrors.Is(err, context.DeadlineExceeded) || goerrors.Is(err, context.Canceled),
		"Error should be context.DeadlineExceeded or context.Canceled")

	// Verify *some* files might still exist
	existCount := 0
	for _, p := range paths {
		if fs.FileExists(p) {
			existCount++
		}
	}
	assert.Greater(t, existCount, 0, "Expected some files to remain after cancellation")

	// Verify internal results reflect the cancellation
	cleanup.mu.Lock()
	assert.NotEmpty(t, cleanup.results, "Should have at least one result before cancellation")
	// The last result might be the cancellation error
	lastResult := cleanup.results[len(cleanup.results)-1]
	if !lastResult.Success {
		assert.True(t, goerrors.Is(lastResult.Error, context.DeadlineExceeded) || goerrors.Is(lastResult.Error, context.Canceled),
			"Last error result should be context error")
	}
	cleanup.mu.Unlock()
}

// TestExecute_NonExistent tests cleanup of items that don't exist.
func TestExecute_NonExistent(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	file := filepath.Join(tempDir, "i_dont_exist.txt")
	dir := filepath.Join(tempDir, "i_dont_exist_dir")

	cleanup.AddItem(file, "file", 0)
	cleanup.AddDirectory(dir)

	err := cleanup.Execute(fs)
	// Cleaning non-existent items is considered success
	require.NoError(t, err, "Execute should succeed even if items don't exist")

	cleanup.mu.Lock()
	assert.Len(t, cleanup.results, 2)
	for _, res := range cleanup.results {
		assert.True(t, res.Success, "Result should be success for non-existent item: %s", res.Path)
	}
	cleanup.mu.Unlock()
}

// TestExecute_PermissionError tests cleanup failure due to file permissions.
func TestExecute_PermissionError(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	// Skip cleanupTest here as it makes files writable, manually clean later
	// defer cleanupTest(t, tempDir)

	// Create a read-only file
	readOnlyFile := filepath.Join(tempDir, "read_only.txt")
	createTestFile(t, readOnlyFile, "cant delete me", 0444) // Read-only
	// Ensure it's cleaned up even if test fails
	defer func() {
		_ = os.Chmod(readOnlyFile, 0644)
		_ = os.Remove(readOnlyFile)
		_ = os.RemoveAll(tempDir)
	}()

	cleanup.AddItem(readOnlyFile, "file", 0)

	err := cleanup.Execute(fs)
	require.Error(t, err, "Execute should fail when a file is not writable")
	assert.Contains(t, err.Error(), "failed to clean up 1 items")

	// Verify the file still exists
	assert.True(t, fs.FileExists(readOnlyFile), "Read-only file should still exist")

	// Verify internal results
	cleanup.mu.Lock()
	require.Len(t, cleanup.results, 1)
	assert.False(t, cleanup.results[0].Success, "Result should indicate failure")
	assert.Error(t, cleanup.results[0].Error, "Result error should be set")
	assert.Contains(t, cleanup.results[0].Error.Error(), "file is not writable", "Error message mismatch")
	cleanup.mu.Unlock()
}

// TestExecute_InvalidPath tests cleanup failure due to invalid path.
func TestExecute_InvalidPath(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	relativePath := "relative/path.txt"
	cleanup.AddItem(relativePath, "file", 0)

	err := cleanup.Execute(fs)
	require.Error(t, err, "Execute should fail with invalid path")
	assert.Contains(t, err.Error(), "failed to clean up 1 items")

	// Verify internal result
	cleanup.mu.Lock()
	require.Len(t, cleanup.results, 1)
	assert.False(t, cleanup.results[0].Success)
	assert.Error(t, cleanup.results[0].Error)
	assert.Contains(t, cleanup.results[0].Error.Error(), "validate path failed: path is not absolute")
	cleanup.mu.Unlock()
}

// TestExecute_NilFileSystem tests execution with a nil FileSystem.
func TestExecute_NilFileSystem(t *testing.T) {
	logger := logging.NewLogger()
	cleanup := NewCleanup(logger)

	err := cleanup.Execute(nil)
	require.Error(t, err, "Execute should fail with nil FileSystem")
	assert.Contains(t, err.Error(), "nil filesystem provided")

	err = cleanup.ExecuteWithContext(context.Background(), nil)
	require.Error(t, err, "ExecuteWithContext should fail with nil FileSystem")
	assert.Contains(t, err.Error(), "nil filesystem provided")
}

package fs

import (
	"context"
	goerrors "errors" // Alias standard Go errors
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestCleanup(t *testing.T) (*Cleanup, FileSystem, string) {
	logger := logging.NewLogger()
	fs := NewFileSystem(logger)
	cleanup := NewCleanup(logger)
	cleanup.SetFileSystem(fs)

	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "cleanup-test-*")
	require.NoError(t, err)

	return cleanup, fs, tempDir
}

func cleanupTest(t *testing.T, tempDir string) {
	err := os.RemoveAll(tempDir)
	require.NoError(t, err)
}

func TestNewCleanup(t *testing.T) {
	logger := logging.NewLogger()
	cleanup := NewCleanup(logger)

	assert.NotNil(t, cleanup)
	assert.NotNil(t, cleanup.logger)
	assert.NotNil(t, cleanup.validator)
	assert.Empty(t, cleanup.items)
	assert.Empty(t, cleanup.results)
}

func TestAddItem(t *testing.T) {
	// Test adding different types of items
	testCases := []struct {
		name     string
		path     string
		itemType string
		priority int
	}{
		{"file", "test.txt", "file", 0},
		{"directory", "test-dir", "directory", 0},
		{"temp", "temp.txt", "temp", 1},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cleanup, _, tempDir := setupTestCleanup(t)
			defer cleanupTest(t, tempDir)

			fullPath := filepath.Join(tempDir, tc.path)
			cleanup.AddItem(fullPath, tc.itemType, tc.priority)

			assert.Len(t, cleanup.items, 1, "Should have exactly one item")
			assert.Equal(t, fullPath, cleanup.items[0].Path, "Path should match")
			assert.Equal(t, tc.itemType, cleanup.items[0].Type, "Type should match")
			assert.Equal(t, tc.priority, cleanup.items[0].Priority, "Priority should match")
		})
	}
}

func TestExecute(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files and directories
	testFile := filepath.Join(tempDir, "test.txt")
	testDir := filepath.Join(tempDir, "test-dir")
	tempFile := filepath.Join(tempDir, "temp.txt")

	require.NoError(t, os.WriteFile(testFile, []byte("test"), 0644))
	require.NoError(t, os.Mkdir(testDir, 0755))
	require.NoError(t, os.WriteFile(tempFile, []byte("temp"), 0644))

	// Add items to cleanup
	cleanup.AddFile(testFile)
	cleanup.AddDirectory(testDir)
	cleanup.AddTempFile(tempFile)

	// Execute cleanup
	err := cleanup.Execute(fs)
	require.NoError(t, err)

	// Verify items are cleaned up (using the interface method)
	assert.False(t, fs.FileExists(testFile))
	assert.False(t, fs.FileExists(testDir)) // Check directory existence via FileExists
	assert.False(t, fs.FileExists(tempFile))

	// Verify results
	results := cleanup.GetResults()
	assert.Len(t, results, 3)
	for _, result := range results {
		assert.True(t, result.Success)
	}

	// Verify stats
	total, success, duration := cleanup.GetStats()
	assert.Equal(t, 3, total)
	assert.Equal(t, 3, success)
	assert.Greater(t, duration, time.Duration(0))
}

func TestExecuteWithContext(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files
	for i := 0; i < 5; i++ {
		file := filepath.Join(tempDir, fmt.Sprintf("test%d.txt", i))
		require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
		cleanup.AddFile(file)
	}

	// Create a context that cancels after a short delay
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Execute cleanup with context
	err := cleanup.ExecuteWithContext(ctx, fs)
	assert.NoError(t, err)

	// Verify results
	results := cleanup.GetResults()
	assert.Len(t, results, 5)
	for _, result := range results {
		assert.True(t, result.Success)
	}
}

func TestExecuteWithContextTimeout(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create a large number of test files to ensure cleanup takes some time
	numFiles := 100
	for i := 0; i < numFiles; i++ {
		file := filepath.Join(tempDir, fmt.Sprintf("test%d.txt", i))
		require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
		cleanup.AddFile(file)
	}

	// Create a context with a very short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	// Execute cleanup with short timeout
	err := cleanup.ExecuteWithContext(ctx, fs)
	assert.Error(t, err, "Should return error when context times out")
	// Use aliased standard errors.Is for context errors
	assert.True(t, goerrors.Is(err, context.DeadlineExceeded), "Error should be context.DeadlineExceeded")

	// Some files should still exist due to timeout
	existingFiles := 0
	for i := 0; i < numFiles; i++ {
		if fs.FileExists(filepath.Join(tempDir, fmt.Sprintf("test%d.txt", i))) { // Use interface method
			existingFiles++
		}
	}
	assert.Greater(t, existingFiles, 0, "Some files should still exist due to timeout")
}

func TestCleanupPriority(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files with different priorities
	lowPriority := filepath.Join(tempDir, "low.txt")
	highPriority := filepath.Join(tempDir, "high.txt")
	mediumPriority := filepath.Join(tempDir, "medium.txt")

	require.NoError(t, os.WriteFile(lowPriority, []byte("low"), 0644))
	require.NoError(t, os.WriteFile(highPriority, []byte("high"), 0644))
	require.NoError(t, os.WriteFile(mediumPriority, []byte("medium"), 0644))

	// Add items with different priorities
	cleanup.AddItem(lowPriority, "file", 0)
	cleanup.AddItem(highPriority, "file", 2)
	cleanup.AddItem(mediumPriority, "file", 1)

	// Execute cleanup
	err := cleanup.Execute(fs)
	require.NoError(t, err)

	// Verify all files are cleaned up
	assert.False(t, fs.FileExists(lowPriority))    // Use interface method
	assert.False(t, fs.FileExists(highPriority))   // Use interface method
	assert.False(t, fs.FileExists(mediumPriority)) // Use interface method

	// Verify results are in priority order
	results := cleanup.GetResults()
	assert.Len(t, results, 3)
	assert.Equal(t, highPriority, results[0].Path)
	assert.Equal(t, mediumPriority, results[1].Path)
	assert.Equal(t, lowPriority, results[2].Path)
}

func TestCleanupNonExistentItems(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Add non-existent items
	nonExistentFile := filepath.Join(tempDir, "nonexistent.txt")
	nonExistentDir := filepath.Join(tempDir, "nonexistent-dir")

	cleanup.AddFile(nonExistentFile)
	cleanup.AddDirectory(nonExistentDir)

	// Execute cleanup
	err := cleanup.Execute(fs)
	require.NoError(t, err)

	// Verify results
	results := cleanup.GetResults()
	assert.Len(t, results, 2)
	for _, result := range results {
		assert.True(t, result.Success)
	}
}

func TestCleanupWithErrors(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create a read-only file (cleanup should fail)
	lockedFile := filepath.Join(tempDir, "locked.txt")
	require.NoError(t, os.WriteFile(lockedFile, []byte("locked"), 0444)) // Read-only

	// Create a normal file that should be cleaned up
	normalFile := filepath.Join(tempDir, "normal.txt")
	require.NoError(t, os.WriteFile(normalFile, []byte("normal"), 0644))

	// Add the items to cleanup
	cleanup.AddFile(lockedFile)
	cleanup.AddFile(normalFile)

	// Execute cleanup
	err := cleanup.Execute(fs)
	require.Error(t, err, "Execute should return an error for cleanup failures")

	// Verify locked item still exists, normal one is gone
	assert.True(t, fs.FileExists(lockedFile), "Read-only file should still exist")
	assert.False(t, fs.FileExists(normalFile), "Normal file should be removed")

	// Verify results
	results := cleanup.GetResults()
	assert.Len(t, results, 2)

	foundLocked := false
	foundNormal := false
	for _, res := range results {
		switch res.Path {
		case lockedFile:
			assert.False(t, res.Success, "Cleanup of locked file should fail")
			assert.Error(t, res.Error)
			foundLocked = true
		case normalFile:
			assert.True(t, res.Success, "Cleanup of normal file should succeed")
			assert.NoError(t, res.Error)
			foundNormal = true
		}
	}
	assert.True(t, foundLocked, "Result for locked file not found")
	assert.True(t, foundNormal, "Result for normal file not found")

	// Manually make locked file writable for cleanupTest
	require.NoError(t, os.Chmod(lockedFile, 0644), "Failed to make locked file writable for cleanup")
}

func TestSetTempDir(t *testing.T) {
	cleanup, _, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Test setting temp directory
	cleanup.SetTempDir(tempDir)
	assert.Equal(t, tempDir, cleanup.tempDir, "Temp directory should be set")

	// Test setting empty temp directory
	cleanup.SetTempDir("")
	assert.Empty(t, cleanup.tempDir, "Temp directory should be empty")
}

func TestExecuteWithContextCancellation(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files
	for i := 0; i < 5; i++ {
		file := filepath.Join(tempDir, fmt.Sprintf("test%d.txt", i))
		require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
		cleanup.AddFile(file)
	}

	// Create a context that cancels immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Execute cleanup with cancelled context
	err := cleanup.ExecuteWithContext(ctx, fs)
	assert.Error(t, err)
	assert.Equal(t, context.Canceled, err)

	// Verify some files might still exist due to cancellation
	existingFiles := 0
	for i := 0; i < 5; i++ {
		if fs.FileExists(filepath.Join(tempDir, fmt.Sprintf("test%d.txt", i))) {
			existingFiles++
		}
	}
	assert.Greater(t, existingFiles, 0, "Some files should still exist due to cancellation")
}

func TestCleanupItemValidation(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Test invalid paths
	invalidPaths := []struct {
		path     string
		errorMsg string
	}{
		{"", "validate path failed: path is empty"},
		{"relative/path.txt", "validate path failed: path is not absolute: relative/path.txt"},
		{"../parent/path.txt", "validate path failed: path is not absolute: ../parent/path.txt"},
		{"*", "validate path failed: path is not absolute: *"},
	}

	for _, tc := range invalidPaths {
		t.Run(fmt.Sprintf("invalid path: %s", tc.path), func(t *testing.T) {
			// Reset items for each test
			cleanup.items = nil
			cleanup.AddItem(tc.path, "file", 0)

			// Execute cleanup to trigger validation
			err := cleanup.Execute(fs)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "cleanup failed: failed to clean up 1 items")

			// Check the actual error in the results
			results := cleanup.GetResults()
			assert.Len(t, results, 1)
			assert.Error(t, results[0].Error)
			assert.Contains(t, results[0].Error.Error(), tc.errorMsg)
		})
	}
}

func TestCleanupWithConcurrentAccess(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files
	numFiles := 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	files := make([]string, numFiles)

	// Create files first
	for i := 0; i < numFiles; i++ {
		file := filepath.Join(tempDir, fmt.Sprintf("test%d.txt", i))
		files[i] = file
		require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
	}

	// Add files to cleanup
	for _, file := range files {
		cleanup.AddFile(file)
	}

	// Run multiple cleanup operations concurrently
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			err := cleanup.Execute(fs)
			mu.Unlock()
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		}()
	}

	wg.Wait()

	// Verify all files are cleaned up
	for _, file := range files {
		assert.False(t, fs.FileExists(file))
	}

	// Verify results
	results := cleanup.GetResults()
	assert.NotEmpty(t, results)

	// Verify stats
	total, success, duration := cleanup.GetStats()
	assert.Greater(t, total, 0)
	assert.Greater(t, success, 0)
	assert.Greater(t, duration, time.Duration(0))
}

func TestCleanupWithLargeNumberOfItems(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create a large number of test files
	numFiles := 1000
	for i := 0; i < numFiles; i++ {
		file := filepath.Join(tempDir, fmt.Sprintf("test%d.txt", i))
		require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
		cleanup.AddFile(file)
	}

	// Execute cleanup
	err := cleanup.Execute(fs)
	require.NoError(t, err)

	// Verify all files are cleaned up
	for i := 0; i < numFiles; i++ {
		assert.False(t, fs.FileExists(filepath.Join(tempDir, fmt.Sprintf("test%d.txt", i))))
	}

	// Verify results
	results := cleanup.GetResults()
	assert.Len(t, results, numFiles)
	for _, result := range results {
		assert.True(t, result.Success)
	}

	// Verify stats
	total, success, duration := cleanup.GetStats()
	assert.Equal(t, numFiles, total)
	assert.Equal(t, numFiles, success)
	assert.Greater(t, duration, time.Duration(0))
}

func TestCleanupResultsConsistency(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files
	numFiles := 10
	for i := 0; i < numFiles; i++ {
		file := filepath.Join(tempDir, fmt.Sprintf("test%d.txt", i))
		require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
		cleanup.AddFile(file)
	}

	// Execute cleanup multiple times
	for i := 0; i < 3; i++ {
		err := cleanup.Execute(fs)
		require.NoError(t, err)

		// Verify results are consistent
		results := cleanup.GetResults()
		assert.Len(t, results, numFiles)
		for _, result := range results {
			assert.True(t, result.Success)
		}

		// Verify stats are consistent
		total, success, duration := cleanup.GetStats()
		assert.Equal(t, numFiles, total)
		assert.Equal(t, numFiles, success)
		assert.Greater(t, duration, time.Duration(0))
	}
}

func TestCleanupWithInvalidFileSystem(t *testing.T) {
	cleanup, _, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test file
	file := filepath.Join(tempDir, "test.txt")
	require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
	cleanup.AddFile(file)

	// Execute cleanup with nil file system
	err := cleanup.Execute(nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil filesystem provided")

	// Create a new cleanup instance without setting filesystem
	cleanup = NewCleanup(cleanup.logger)
	cleanup.AddFile(file)

	// Execute cleanup with nil filesystem
	err = cleanup.Execute(nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil filesystem provided")
}

func TestSortCleanupItems(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files with different priorities
	files := []struct {
		path     string
		priority int
	}{
		{filepath.Join(tempDir, "low.txt"), 0},
		{filepath.Join(tempDir, "high.txt"), 2},
		{filepath.Join(tempDir, "medium.txt"), 1},
		{filepath.Join(tempDir, "low2.txt"), 0},
		{filepath.Join(tempDir, "high2.txt"), 2},
	}

	// Add items in random order
	for _, file := range files {
		cleanup.AddItem(file.path, "file", file.priority)
	}

	// Execute cleanup to trigger sorting
	err := cleanup.Execute(fs)
	require.NoError(t, err)

	// Get the results
	results := cleanup.GetResults()
	assert.Len(t, results, len(files))

	// Verify results are in priority order (descending)
	prevPriority := 2 // Start with highest priority
	for _, result := range results {
		// Find the original priority for this path
		var priority int
		for _, file := range files {
			if file.path == result.Path {
				priority = file.priority
				break
			}
		}
		assert.LessOrEqual(t, priority, prevPriority, "Results should be in descending priority order")
		prevPriority = priority
	}
}

func TestCleanupWithDifferentFileTypes(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files and directories
	file := filepath.Join(tempDir, "test.txt")
	dir := filepath.Join(tempDir, "test-dir")
	temp := filepath.Join(tempDir, "temp.txt")

	require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
	require.NoError(t, os.Mkdir(dir, 0755))
	require.NoError(t, os.WriteFile(temp, []byte("temp"), 0644))

	// Add items with different types
	cleanup.AddFile(file)
	cleanup.AddDirectory(dir)
	cleanup.AddTempFile(temp)

	// Execute cleanup
	err := cleanup.Execute(fs)
	require.NoError(t, err)

	// Verify all items are cleaned up
	assert.False(t, fs.FileExists(file))
	assert.False(t, fs.FileExists(dir))
	assert.False(t, fs.FileExists(temp))

	// Verify results
	results := cleanup.GetResults()
	assert.Len(t, results, 3)
	for _, result := range results {
		assert.True(t, result.Success)
	}

	// Verify stats
	total, success, duration := cleanup.GetStats()
	assert.Equal(t, 3, total)
	assert.Equal(t, 3, success)
	assert.Greater(t, duration, time.Duration(0))
}

func TestCleanupWithFilePermissions(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create a read-only file
	file := filepath.Join(tempDir, "readonly.txt")
	require.NoError(t, os.WriteFile(file, []byte("test"), 0444)) // Read-only permissions
	cleanup.AddFile(file)

	// Execute cleanup
	err := cleanup.Execute(fs)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cleanup failed: failed to clean up 1 items")

	// Verify results
	results := cleanup.GetResults()
	assert.Len(t, results, 1)
	assert.False(t, results[0].Success)
	assert.Contains(t, results[0].Error.Error(), "cleanup failed: file is not writable")
}

func TestCleanupWithEmptyItems(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Execute cleanup with no items
	err := cleanup.Execute(fs)
	require.NoError(t, err)

	// Verify results
	results := cleanup.GetResults()
	assert.Empty(t, results)

	// Verify stats
	total, success, duration := cleanup.GetStats()
	assert.Equal(t, 0, total)
	assert.Equal(t, 0, success)
	assert.Greater(t, duration, time.Duration(0))
}

func TestCleanupWithContextCancellation(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files
	numFiles := 10
	for i := 0; i < numFiles; i++ {
		file := filepath.Join(tempDir, fmt.Sprintf("test%d.txt", i))
		require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
		cleanup.AddFile(file)
	}

	// Create a context that cancels after a short delay
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Execute cleanup with context
	err := cleanup.ExecuteWithContext(ctx, fs)
	assert.NoError(t, err)

	// Verify results
	results := cleanup.GetResults()
	assert.Len(t, results, numFiles)
	for _, result := range results {
		assert.True(t, result.Success)
	}
}

func TestCleanupWithMixedResults(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create a mix of valid and invalid files
	validFile := filepath.Join(tempDir, "valid.txt")
	readOnlyFile := filepath.Join(tempDir, "readonly.txt")
	nonExistentFile := filepath.Join(tempDir, "nonexistent.txt")

	require.NoError(t, os.WriteFile(validFile, []byte("test"), 0644))
	require.NoError(t, os.WriteFile(readOnlyFile, []byte("test"), 0444)) // Read-only

	cleanup.AddFile(validFile)
	cleanup.AddFile(readOnlyFile)
	cleanup.AddFile(nonExistentFile)

	// Execute cleanup
	err := cleanup.Execute(fs)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to clean up")

	// Verify results
	results := cleanup.GetResults()
	assert.Len(t, results, 3)

	// Count success/failure
	successCount := 0
	failureCount := 0
	for _, result := range results {
		if result.Success {
			successCount++
		} else {
			failureCount++
		}
	}

	assert.Equal(t, 2, successCount) // valid file and non-existent file
	assert.Equal(t, 1, failureCount) // read-only file

	// Verify stats
	total, success, duration := cleanup.GetStats()
	assert.Equal(t, 3, total)
	assert.Equal(t, 2, success)
	assert.Greater(t, duration, time.Duration(0))
}

func TestExecuteWithContextAndTempDir(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files
	testFile := filepath.Join(tempDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("test"), 0644))
	cleanup.AddFile(testFile)

	// Set up temp directory with some files
	tempDirPath := filepath.Join(tempDir, "temp-dir")
	require.NoError(t, os.Mkdir(tempDirPath, 0755))
	tempFile := filepath.Join(tempDirPath, "temp.txt")
	require.NoError(t, os.WriteFile(tempFile, []byte("temp"), 0644))
	cleanup.SetTempDir(tempDirPath)

	// Create a context that will not timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Execute cleanup with context
	err := cleanup.ExecuteWithContext(ctx, fs)
	require.NoError(t, err)

	// Verify both regular file and temp directory are cleaned up
	assert.False(t, fs.FileExists(testFile))
	assert.False(t, fs.FileExists(tempDirPath))

	// Verify results include both operations
	results := cleanup.GetResults()
	assert.Len(t, results, 2)
	for _, result := range results {
		assert.True(t, result.Success)
	}
}

func TestExecuteWithContextTempDirError(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create test files
	testFile := filepath.Join(tempDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("test"), 0644))
	cleanup.AddFile(testFile)

	// Set up temp directory with read-only permissions
	tempDirPath := filepath.Join(tempDir, "temp-dir")
	require.NoError(t, os.Mkdir(tempDirPath, 0755))
	require.NoError(t, os.Chmod(tempDirPath, 0555)) // Read-only
	cleanup.SetTempDir(tempDirPath)

	// Create a context that will not timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Execute cleanup with context
	err := cleanup.ExecuteWithContext(ctx, fs)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to clean up")

	// Regular file should still be cleaned up
	assert.False(t, fs.FileExists(testFile))

	// Temp directory should still exist (cleanup failed)
	assert.True(t, fs.FileExists(tempDirPath))

	// Clean up the test directory
	require.NoError(t, os.Chmod(tempDirPath, 0755))
}

func TestCleanupWithUnknownItemType(t *testing.T) {
	cleanup, fs, tempDir := setupTestCleanup(t)
	defer cleanupTest(t, tempDir)

	// Create a test file
	testFile := filepath.Join(tempDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("test"), 0644))

	// Add item with unknown type
	cleanup.AddItem(testFile, "unknown", 0)

	// Execute cleanup
	err := cleanup.Execute(fs)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to clean up 1 items")

	// File should still exist since cleanup failed
	assert.True(t, fs.FileExists(testFile))

	// Verify results
	results := cleanup.GetResults()
	assert.Len(t, results, 1)
	assert.False(t, results[0].Success)
	assert.Contains(t, results[0].Error.Error(), "unknown item type")
}

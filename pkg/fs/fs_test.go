package fs

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestFileSystem creates a temporary directory and returns a new FileSystem instance.
// It ensures the temp directory has full permissions.
func setupTestFileSystem(t *testing.T) (FileSystem, string) {
	logger := logging.NewLogger()
	fs := NewFileSystem(logger)

	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "fs-test-*")
	require.NoError(t, err)

	// Ensure the temp directory has full permissions
	err = os.Chmod(tempDir, 0777)
	require.NoError(t, err)

	return fs, tempDir
}

// cleanupFSTest ensures the temp directory and its contents are writable
// before attempting to remove them.
func cleanupFSTest(t *testing.T, tempDir string) {
	// Make the directory and its contents writable before removal
	require.NoError(t, filepath.Walk(tempDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Logf("Error accessing path %s during cleanup walk: %v", path, err)
			return err
		}
		if info != nil && info.Mode()&0200 == 0 {
			if chmodErr := os.Chmod(path, info.Mode()|0200); chmodErr != nil {
				t.Logf("Error making path %s writable during cleanup: %v", path, chmodErr)
			}
		}
		return nil
	}))

	err := os.RemoveAll(tempDir)
	require.NoError(t, err)
}

// Tests the constructor for the fileSystem type.
func TestNewFileSystem(t *testing.T) {
	logger := logging.NewLogger()
	fs := NewFileSystem(logger)

	assert.NotNil(t, fs)

	// Type assert to check internal fields if necessary
	fsImpl, ok := fs.(*fileSystem)
	require.True(t, ok, "NewFileSystem did not return the expected underlying type")

	assert.NotNil(t, fsImpl.logger)
	assert.NotNil(t, fsImpl.validator)
}

// Tests the CreateDirectory function, including nested dirs and error cases.
func TestCreateDirectory(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	testCases := []struct {
		name        string
		path        string
		perm        os.FileMode
		setupFunc   func(string) error
		shouldError bool
	}{
		{
			name:        "Create simple directory",
			path:        filepath.Join(tempDir, "test-dir"),
			perm:        0755,
			shouldError: false,
		},
		{
			name:        "Create nested directories",
			path:        filepath.Join(tempDir, "nested", "dir"),
			perm:        0755,
			shouldError: false,
		},
		{
			name:        "Empty path",
			path:        "",
			perm:        0755,
			shouldError: true,
		},
		{
			name: "Create in read-only parent",
			path: filepath.Join(tempDir, "readonly", "dir"),
			perm: 0755,
			setupFunc: func(path string) error {
				parent := filepath.Dir(path)
				if err := os.MkdirAll(parent, 0755); err != nil {
					return err
				}
				return os.Chmod(parent, 0500)
			},
			shouldError: true,
		},
		{
			name: "Create directory with file in path",
			path: filepath.Join(tempDir, "file", "dir"),
			perm: 0755,
			setupFunc: func(path string) error {
				return os.WriteFile(filepath.Dir(path), []byte("test"), 0644)
			},
			shouldError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setupFunc != nil {
				require.NoError(t, tc.setupFunc(tc.path))
			}

			err := fs.CreateDirectory(tc.path, tc.perm)
			if tc.shouldError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.True(t, fs.IsDirectory(tc.path))

			info, err := os.Stat(tc.path)
			require.NoError(t, err)
			assert.Equal(t, tc.perm, info.Mode()&0777)
		})
	}
}

// Tests the RemoveDirectory function, including empty, non-empty, and error cases.
func TestRemoveDirectory(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	testCases := []struct {
		name        string
		path        string
		setupFunc   func(string) error
		shouldError bool
	}{
		{
			name: "Remove empty directory",
			path: filepath.Join(tempDir, "empty-dir"),
			setupFunc: func(path string) error {
				return os.MkdirAll(path, 0755)
			},
			shouldError: false,
		},
		{
			name: "Remove directory with contents",
			path: filepath.Join(tempDir, "dir-with-contents"),
			setupFunc: func(path string) error {
				if err := os.MkdirAll(path, 0755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(path, "file.txt"), []byte("test"), 0644)
			},
			shouldError: false,
		},
		{
			name:        "Empty path",
			path:        "",
			shouldError: true,
		},
		{
			name: "Remove read-only directory",
			path: filepath.Join(tempDir, "readonly-dir"),
			setupFunc: func(path string) error {
				if err := os.MkdirAll(path, 0755); err != nil {
					return err
				}
				return os.Chmod(path, 0500)
			},
			shouldError: false,
		},
		{
			name:        "Remove non-existent directory",
			path:        filepath.Join(tempDir, "nonexistent"),
			shouldError: false,
		},
		{
			name: "Remove file instead of directory",
			path: filepath.Join(tempDir, "file.txt"),
			setupFunc: func(path string) error {
				return os.WriteFile(path, []byte("test"), 0644)
			},
			shouldError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setupFunc != nil {
				require.NoError(t, tc.setupFunc(tc.path))
			}

			err := fs.RemoveDirectory(tc.path)
			if tc.shouldError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.False(t, fs.FileExists(tc.path))
		})
	}
}

// Tests the ListDirectory function, including error cases.
func TestListDirectory(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	// Create test files
	files := []string{"file1.txt", "file2.txt", "file3.txt"}
	for _, file := range files {
		path := filepath.Join(tempDir, file)
		require.NoError(t, os.WriteFile(path, []byte("test"), 0644))
	}

	// Test listing directory
	listed, err := fs.ListDirectory(tempDir)
	require.NoError(t, err)
	assert.Len(t, listed, len(files))
	for _, file := range files {
		assert.Contains(t, listed, file)
	}

	// Test listing non-existent directory
	_, err = fs.ListDirectory(filepath.Join(tempDir, "nonexistent"))
	assert.Error(t, err)

	// Test listing with invalid path
	_, err = fs.ListDirectory("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")

	// Test listing a file instead of a directory
	filePath := filepath.Join(tempDir, "not_a_dir.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("test"), 0644))
	_, err = fs.ListDirectory(filePath)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

// Tests the IsDirectory function.
func TestIsDirectory(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	// Test with directory
	assert.True(t, fs.IsDirectory(tempDir))

	// Test with file
	testFile := filepath.Join(tempDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("test"), 0644))
	assert.False(t, fs.IsDirectory(testFile))

	// Test with non-existent path
	assert.False(t, fs.IsDirectory(filepath.Join(tempDir, "nonexistent")))
}

// Tests the GetFileSize function.
func TestGetFileSize(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	testCases := []struct {
		name         string
		path         string
		setupFunc    func(string) error
		expectedSize int64
		shouldError  bool
	}{
		{
			name: "Regular file",
			path: filepath.Join(tempDir, "regular.txt"),
			setupFunc: func(path string) error {
				return os.WriteFile(path, []byte("test data"), 0644)
			},
			expectedSize: 9,
			shouldError:  false,
		},
		{
			name: "Empty file",
			path: filepath.Join(tempDir, "empty.txt"),
			setupFunc: func(path string) error {
				return os.WriteFile(path, []byte{}, 0644)
			},
			expectedSize: 0,
			shouldError:  false,
		},
		{
			name:        "Non-existent file",
			path:        filepath.Join(tempDir, "nonexistent.txt"),
			shouldError: true,
		},
		{
			name: "Directory instead of file",
			path: filepath.Join(tempDir, "dir"),
			setupFunc: func(path string) error {
				return os.MkdirAll(path, 0755)
			},
			shouldError: true,
		},
		{
			name:        "Empty path",
			path:        "",
			shouldError: true,
		},
		{
			name: "Symlink to file",
			path: filepath.Join(tempDir, "link.txt"),
			setupFunc: func(path string) error {
				target := filepath.Join(tempDir, "target.txt")
				if err := os.WriteFile(target, []byte("test data"), 0644); err != nil {
					return err
				}
				return os.Symlink(target, path)
			},
			expectedSize: 9,
			shouldError:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setupFunc != nil {
				require.NoError(t, tc.setupFunc(tc.path))
			}

			size, err := fs.GetFileSize(tc.path)
			if tc.shouldError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.expectedSize, size)
		})
	}
}

// Tests the GetFileMode function.
func TestGetFileMode(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	testCases := []struct {
		name         string
		path         string
		setupFunc    func(string) error
		expectedMode os.FileMode
		shouldError  bool
	}{
		{
			name: "Regular file with read/write",
			path: filepath.Join(tempDir, "regular.txt"),
			setupFunc: func(path string) error {
				return os.WriteFile(path, []byte("test"), 0644)
			},
			expectedMode: 0644,
			shouldError:  false,
		},
		{
			name: "Executable file",
			path: filepath.Join(tempDir, "executable"),
			setupFunc: func(path string) error {
				return os.WriteFile(path, []byte("#!/bin/sh"), 0755)
			},
			expectedMode: 0755,
			shouldError:  false,
		},
		{
			name: "Directory",
			path: filepath.Join(tempDir, "dir"),
			setupFunc: func(path string) error {
				return os.MkdirAll(path, 0755)
			},
			expectedMode: os.ModeDir | 0755,
			shouldError:  false,
		},
		{
			name:        "Non-existent file",
			path:        filepath.Join(tempDir, "nonexistent"),
			shouldError: true,
		},
		{
			name:        "Empty path",
			path:        "",
			shouldError: true,
		},
		{
			name: "Symlink",
			path: filepath.Join(tempDir, "link"),
			setupFunc: func(path string) error {
				target := filepath.Join(tempDir, "target")
				if err := os.WriteFile(target, []byte("test"), 0644); err != nil {
					return err
				}
				return os.Symlink(target, path)
			},
			expectedMode: 0644,
			shouldError:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setupFunc != nil {
				require.NoError(t, tc.setupFunc(tc.path))
			}

			mode, err := fs.GetFileMode(tc.path)
			if tc.shouldError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			if tc.expectedMode&os.ModeDir != 0 {
				assert.True(t, mode.IsDir())
				assert.Equal(t, tc.expectedMode&0777, mode&0777)
			} else {
				assert.Equal(t, tc.expectedMode, mode&0777)
			}
		})
	}
}

// Tests the SetFileMode function.
func TestSetFileMode(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	// Test with regular file
	testFile := filepath.Join(tempDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("test"), 0644))

	// Change mode and verify
	newMode := os.FileMode(0600)
	err := fs.SetFileMode(testFile, newMode)
	require.NoError(t, err)

	mode, err := fs.GetFileMode(testFile)
	require.NoError(t, err)
	assert.Equal(t, newMode, mode&0777)

	// Test with non-existent file
	err = fs.SetFileMode(filepath.Join(tempDir, "nonexistent"), 0644)
	assert.Error(t, err)

	// Test with invalid path
	err = fs.SetFileMode("", 0644)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")
}

// Tests the MoveFile function.
func TestMoveFile(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	// Create source file
	srcFile := filepath.Join(tempDir, "source.txt")
	testData := []byte("test data")
	require.NoError(t, os.WriteFile(srcFile, testData, 0644))

	// Test moving file
	dstFile := filepath.Join(tempDir, "destination.txt")
	err := fs.MoveFile(srcFile, dstFile)
	require.NoError(t, err)

	// Verify source is gone and destination exists
	assert.False(t, fs.FileExists(srcFile))
	assert.True(t, fs.FileExists(dstFile))

	// Verify content
	content, err := os.ReadFile(dstFile)
	require.NoError(t, err)
	assert.Equal(t, testData, content)

	// Test with non-existent source
	err = fs.MoveFile(filepath.Join(tempDir, "nonexistent"), dstFile)
	assert.Error(t, err)

	// Test with invalid paths
	err = fs.MoveFile("", dstFile)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")

	err = fs.MoveFile(dstFile, "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")
}

// Tests the WriteFileAtomic function.
func TestWriteFileAtomic(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	testCases := []struct {
		name        string
		path        string
		reader      io.Reader
		mode        os.FileMode
		setupFunc   func(string) error
		shouldError bool
	}{
		{
			name:        "Valid atomic write",
			path:        filepath.Join(tempDir, "atomic1.txt"),
			reader:      bytes.NewReader([]byte("atomic test data")),
			mode:        0644,
			shouldError: false,
		},
		{
			name:        "Empty path",
			path:        "",
			reader:      bytes.NewReader([]byte("test data")),
			mode:        0644,
			shouldError: true,
		},
		{
			name:        "Nil reader",
			path:        filepath.Join(tempDir, "atomic-nil-reader.txt"),
			mode:        0644,
			reader:      nil,
			shouldError: true,
		},
		{
			name:   "Parent directory creation",
			path:   filepath.Join(tempDir, "subdir", "atomic3.txt"),
			reader: bytes.NewReader([]byte("nested test data")),
			mode:   0644,
			setupFunc: func(path string) error {
				return os.MkdirAll(filepath.Dir(path), 0755)
			},
			shouldError: false,
		},
		{
			name:   "Existing file overwrite",
			path:   filepath.Join(tempDir, "atomic4.txt"),
			reader: bytes.NewReader([]byte("new data")),
			mode:   0644,
			setupFunc: func(path string) error {
				return os.WriteFile(path, []byte("old data"), 0644)
			},
			shouldError: false,
		},
		{
			name:   "Read-only parent directory",
			path:   filepath.Join(tempDir, "readonly", "atomic5.txt"),
			reader: bytes.NewReader([]byte("test data")),
			mode:   0644,
			setupFunc: func(path string) error {
				dir := filepath.Dir(path)
				if err := os.MkdirAll(dir, 0755); err != nil {
					return err
				}
				return os.Chmod(dir, 0500)
			},
			shouldError: true,
		},
		{
			name:   "Simulate rename failure (target is dir)",
			path:   filepath.Join(tempDir, "atomic-cleanup-fail"),
			reader: bytes.NewReader([]byte("some data")),
			mode:   0644,
			setupFunc: func(path string) error {
				return os.Mkdir(path, 0755)
			},
			shouldError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Re-initialize reader for each sub-test if it's a bytes.Reader
			var currentReader io.Reader
			var expectedBytes []byte
			if br, ok := tc.reader.(*bytes.Reader); ok {
				contentBytes, _ := io.ReadAll(br)
				_, err := br.Seek(0, io.SeekStart)
				require.NoError(t, err)
				currentReader = bytes.NewReader(contentBytes)
				expectedBytes = contentBytes
			} else {
				currentReader = tc.reader
				if currentReader != nil {
					buf := &bytes.Buffer{}
					_, err := io.Copy(buf, currentReader)
					if err == nil {
						expectedBytes = buf.Bytes()
						if seeker, ok := currentReader.(io.Seeker); ok {
							_, err = seeker.Seek(0, io.SeekStart)
							require.NoError(t, err)
						} else {
							t.Logf("Warning: Reader for test '%s' is not a *bytes.Reader and not seekable. Comparison might be unreliable if read externally.", tc.name)
							currentReader = bytes.NewReader(expectedBytes)
						}
					} else {
						t.Logf("Warning: Could not read expected bytes from reader for test '%s': %v", tc.name, err)
					}
				}
			}

			if tc.setupFunc != nil {
				require.NoError(t, tc.setupFunc(tc.path))
			}

			err := fs.WriteFileAtomic(tc.path, currentReader, tc.mode)
			if tc.shouldError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			content, err := os.ReadFile(tc.path)
			require.NoError(t, err)

			assert.Equal(t, expectedBytes, content)

			info, err := os.Stat(tc.path)
			require.NoError(t, err)
			assert.Equal(t, tc.mode, info.Mode()&0777)

			// Verify temp file is cleaned up
			tmpPath := tc.path + ".tmp"
			assert.False(t, fs.FileExists(tmpPath))
		})
	}
}

// Tests the SecureRemove function, including handling of read-only files.
func TestSecureRemove(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	testCases := []struct {
		name        string
		path        string
		setupFunc   func(string) error
		verifyFunc  func(string) error
		shouldError bool
	}{
		{
			name: "Remove regular file",
			path: filepath.Join(tempDir, "regular.txt"),
			setupFunc: func(path string) error {
				return os.WriteFile(path, []byte("test"), 0644)
			},
			shouldError: false,
		},
		{
			name: "Remove read-only file",
			path: filepath.Join(tempDir, "readonly.txt"),
			setupFunc: func(path string) error {
				if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
					return err
				}
				return os.Chmod(path, 0444)
			},
			shouldError: false,
		},
		{
			name: "Remove file in read-only directory with chmod error",
			path: filepath.Join(tempDir, "readonly", "file.txt"),
			setupFunc: func(path string) error {
				dir := filepath.Dir(path)
				if err := os.MkdirAll(dir, 0755); err != nil {
					return err
				}
				if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
					return err
				}
				// Make both the directory and its parent read-only
				if err := os.Chmod(dir, 0555); err != nil {
					return err
				}
				return os.Chmod(filepath.Dir(dir), 0555)
			},
			shouldError: true,
		},
		{
			name:        "Remove non-existent file",
			path:        filepath.Join(tempDir, "nonexistent.txt"),
			shouldError: false,
		},
		{
			name:        "Empty path",
			path:        "",
			shouldError: true,
		},
		{
			name: "Remove directory",
			path: filepath.Join(tempDir, "dir"),
			setupFunc: func(path string) error {
				if err := os.MkdirAll(path, 0755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(path, "file.txt"), []byte("test"), 0644)
			},
			shouldError: true,
		},
		{
			name: "Remove symlink",
			path: filepath.Join(tempDir, "link"),
			setupFunc: func(path string) error {
				target := filepath.Join(tempDir, "target.txt")
				if err := os.WriteFile(target, []byte("test"), 0644); err != nil {
					return err
				}
				return os.Symlink(target, path)
			},
			verifyFunc: func(path string) error {
				target := filepath.Join(tempDir, "target.txt")
				if _, err := os.Stat(target); err != nil {
					return err
				}
				return nil
			},
			shouldError: false,
		},
		{
			name: "Remove file with stat error",
			path: filepath.Join(tempDir, "stat-error.txt"),
			setupFunc: func(path string) error {
				// Create a symlink to a non-existent file
				return os.Symlink("/nonexistent", path)
			},
			shouldError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset permissions on temp directory before each test
			require.NoError(t, os.Chmod(tempDir, 0777))

			if tc.setupFunc != nil {
				err := tc.setupFunc(tc.path)
				if err != nil {
					t.Fatalf("Setup failed: %v", err)
				}
			}

			err := fs.SecureRemove(tc.path)
			if tc.shouldError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.False(t, fs.FileExists(tc.path))

			if tc.verifyFunc != nil {
				require.NoError(t, tc.verifyFunc(tc.path))
			}
		})
	}
}

// Tests the VerifyDirectory function.
func TestVerifyDirectory(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	// Test with valid directory
	err := fs.VerifyDirectory(tempDir)
	require.NoError(t, err)

	// Test with file
	testFile := filepath.Join(tempDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("test"), 0644))
	err = fs.VerifyDirectory(testFile)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")

	// Test with non-existent path
	err = fs.VerifyDirectory(filepath.Join(tempDir, "nonexistent"))
	assert.Error(t, err)

	// Test with invalid path
	err = fs.VerifyDirectory("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")
}

// Tests the VerifyFile function.
func TestVerifyFile(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	// Test with valid file
	testFile := filepath.Join(tempDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("test"), 0644))
	err := fs.VerifyFile(testFile)
	require.NoError(t, err)

	// Test with directory
	err = fs.VerifyFile(tempDir)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a file")

	// Test with non-existent path
	err = fs.VerifyFile(filepath.Join(tempDir, "nonexistent"))
	assert.Error(t, err)

	// Test with invalid path
	err = fs.VerifyFile("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")
}

// Tests the IsExecutable function.
func TestIsExecutable(t *testing.T) {
	fs, tempDir := setupTestFileSystem(t)
	defer cleanupFSTest(t, tempDir)

	// Create executable file
	execPath := filepath.Join(tempDir, "executable.sh")
	require.NoError(t, os.WriteFile(execPath, []byte("#!/bin/bash\necho hello"), 0755))

	// Create non-executable file
	nonExecPath := filepath.Join(tempDir, "nonexecutable.txt")
	require.NoError(t, os.WriteFile(nonExecPath, []byte("hello"), 0644))

	// Test executable file
	isExec, err := fs.IsExecutable(execPath)
	assert.NoError(t, err)
	assert.True(t, isExec, "File with 0755 mode should be executable")

	// Test non-executable file
	isExec, err = fs.IsExecutable(nonExecPath)
	assert.NoError(t, err)
	assert.False(t, isExec, "File with 0644 mode should not be executable")

	// Test non-existent file
	isExec, err = fs.IsExecutable(filepath.Join(tempDir, "nonexistent"))
	assert.Error(t, err) // Expect stat error
	assert.False(t, isExec)

	// Test directory
	isExec, err = fs.IsExecutable(tempDir)
	assert.Error(t, err, "IsExecutable should return an error for a directory")
	assert.Contains(t, err.Error(), "not a file") // Check for the specific validation error
	assert.False(t, isExec, "Directory should not be reported as executable by this check")

	// Test error case (e.g., permission denied to stat)
	unreadableFile := filepath.Join(tempDir, "unreadable.txt")
	require.NoError(t, os.WriteFile(unreadableFile, []byte("test"), 0644))
	require.NoError(t, os.Chmod(tempDir, 0000)) // Make parent unreadable
	isExec, err = fs.IsExecutable(unreadableFile)
	assert.Error(t, err)
	assert.False(t, isExec)
	require.NoError(t, os.Chmod(tempDir, 0755)) // Restore permissions for cleanup
}

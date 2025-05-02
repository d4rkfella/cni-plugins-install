package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewExtractor(t *testing.T) {
	logger := logging.NewLogger()
	e := NewExtractor(logger)
	require.NotNil(t, e)
	assert.NotNil(t, e.logger)
	assert.NotNil(t, e.fileSystem)
}

// Helper to create a dummy tar.gz file for testing
func createTestTarGz(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	tarPath := filepath.Join(dir, "test.tar.gz")
	tf, err := os.Create(tarPath)
	require.NoError(t, err)
	defer func() {
		if err := tf.Close(); err != nil {
			t.Logf("Error closing tar.gz file handle: %v", err)
		}
	}()

	gzw := gzip.NewWriter(tf)
	defer func() {
		if err := gzw.Close(); err != nil {
			t.Logf("Error closing gzip writer: %v", err)
		}
	}()

	tw := tar.NewWriter(gzw)
	defer func() {
		if err := tw.Close(); err != nil {
			t.Logf("Error closing tar writer: %v", err)
		}
	}()

	for name, content := range files {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0644,
			Size:    int64(len(content)),
			ModTime: time.Now(),
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err = tw.Write([]byte(content))
		require.NoError(t, err)
	}
	return tarPath
}

// Helper to create a dummy zip file for testing, now supports adding directories
// If content is empty string, it creates a directory entry.
func createTestZip(t *testing.T, dir string, items map[string]string) string {
	t.Helper()
	zipPath := filepath.Join(dir, "test.zip")
	zf, err := os.Create(zipPath)
	require.NoError(t, err)
	defer func() {
		if err := zf.Close(); err != nil {
			t.Logf("Error closing zip file handle: %v", err)
		}
	}()

	zw := zip.NewWriter(zf)
	defer func() {
		if err := zw.Close(); err != nil {
			t.Logf("Error closing zip writer: %v", err)
		}
	}()

	for name, content := range items {
		// If content is empty string, treat as directory
		if content == "" {
			// Ensure directory names end with /
			dirName := name
			if dirName[len(dirName)-1] != '/' {
				dirName += "/"
			}
			_, err := zw.Create(dirName)
			require.NoError(t, err)
		} else {
			fw, err := zw.Create(name)
			require.NoError(t, err)
			_, err = fw.Write([]byte(content))
			require.NoError(t, err)
		}
	}
	return zipPath
}

// setupExtractorTest creates temp directories (source for archive, target for extraction)
// and returns a new Extractor instance.
func setupExtractorTest(t *testing.T) (e *Extractor, srcDir, targetDir string) {
	logger := logging.NewLogger()
	e = NewExtractor(logger)

	srcDir, err := os.MkdirTemp("", "extract-test-src-*")
	require.NoError(t, err)

	targetDir, err = os.MkdirTemp("", "extract-test-tgt-*")
	require.NoError(t, err)

	return e, srcDir, targetDir
}

// cleanupExtractorTest removes the temporary source and target directories.
func cleanupExtractorTest(t *testing.T, srcDir, targetDir string) {
	if srcDir != "" {
		require.NoError(t, os.RemoveAll(srcDir))
	}
	if targetDir != "" {
		require.NoError(t, os.RemoveAll(targetDir))
	}
}

func TestExtract(t *testing.T) {
	// Tests successful extraction of a .tar.gz archive.
	t.Run("SuccessTarGz", func(t *testing.T) {
		e, srcDir, targetDir := setupExtractorTest(t)
		defer cleanupExtractorTest(t, srcDir, targetDir)

		// Files to archive
		files := map[string]string{
			"file1.txt":        "content1",
			"subdir/file2.txt": "content2", // Test subdirectory structure
		}
		archivePath := createTestTarGz(t, srcDir, files)

		// Extract
		err := e.Extract(context.Background(), archivePath, targetDir)
		require.NoError(t, err)

		// Verify extracted files (only base names should be extracted)
		expectedFiles := map[string]string{
			"file1.txt": "content1",
			"file2.txt": "content2", // Base name only
		}

		dirEntries, err := os.ReadDir(targetDir)
		require.NoError(t, err)
		assert.Len(t, dirEntries, len(expectedFiles))

		for name, expectedContent := range expectedFiles {
			extractedPath := filepath.Join(targetDir, name)
			contentBytes, err := os.ReadFile(extractedPath)
			require.NoError(t, err, "Error reading extracted file %s", name)
			assert.Equal(t, expectedContent, string(contentBytes), "Content mismatch for %s", name)

			// Check default mode (should be 0644 from helper)
			info, err := os.Stat(extractedPath)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0644), info.Mode()&0777)
		}
	})

	// Tests successful extraction of a .zip archive containing files and directories.
	t.Run("SuccessZipWithDir", func(t *testing.T) { // Renamed and modified test
		e, srcDir, targetDir := setupExtractorTest(t)
		defer cleanupExtractorTest(t, srcDir, targetDir)

		// Items to archive (file and an empty string for directory)
		items := map[string]string{
			"fileA.txt":      "contentA",
			"dir1/":          "", // Add a directory entry
			"dir1/fileB.txt": "contentB",
		}
		archivePath := createTestZip(t, srcDir, items)

		// Extract
		err := e.Extract(context.Background(), archivePath, targetDir)
		require.NoError(t, err)

		// Verify extracted files and directory (only base names/dirs extracted)
		expectedFiles := map[string]string{
			"fileA.txt": "contentA",
			"fileB.txt": "contentB",
		}
		expectedDirs := []string{"dir1"}

		dirEntries, err := os.ReadDir(targetDir)
		require.NoError(t, err)
		assert.Len(t, dirEntries, len(expectedFiles)+len(expectedDirs))

		foundDirs := 0
		foundFiles := 0
		for _, entry := range dirEntries {
			if entry.IsDir() {
				assert.Contains(t, expectedDirs, entry.Name(), "Unexpected directory found")
				foundDirs++
			} else {
				name := entry.Name()
				expectedContent, ok := expectedFiles[name]
				require.True(t, ok, "Unexpected file found: %s", name)
				extractedPath := filepath.Join(targetDir, name)
				contentBytes, err := os.ReadFile(extractedPath)
				require.NoError(t, err, "Error reading extracted file %s", name)
				assert.Equal(t, expectedContent, string(contentBytes), "Content mismatch for %s", name)
				foundFiles++
			}
		}
		assert.Equal(t, len(expectedDirs), foundDirs, "Did not find expected number of directories")
		assert.Equal(t, len(expectedFiles), foundFiles, "Did not find expected number of files")
	})

	// Tests error handling for unsupported archive formats.
	t.Run("ErrorUnsupportedFormat", func(t *testing.T) {
		e, srcDir, targetDir := setupExtractorTest(t)
		defer cleanupExtractorTest(t, srcDir, targetDir)

		archivePath := filepath.Join(srcDir, "test.rar") // Unsupported
		require.NoError(t, os.WriteFile(archivePath, []byte("dummy"), 0644))

		err := e.Extract(context.Background(), archivePath, targetDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported archive format: .rar")
	})

	// Tests error handling when the source archive file does not exist.
	t.Run("ErrorArchiveNotFound", func(t *testing.T) {
		e, srcDir, targetDir := setupExtractorTest(t)
		defer cleanupExtractorTest(t, srcDir, targetDir)

		archivePath := filepath.Join(srcDir, "nonexistent.tar.gz")

		err := e.Extract(context.Background(), archivePath, targetDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "open archive")
		assert.Contains(t, err.Error(), "no such file or directory")
	})

	// Tests error handling when the target directory is not writable.
	t.Run("ErrorTargetNotWritable", func(t *testing.T) {
		e, srcDir, targetDir := setupExtractorTest(t)
		defer cleanupExtractorTest(t, srcDir, targetDir)

		// Files to archive
		files := map[string]string{"file1.txt": "content1"}
		archivePath := createTestTarGz(t, srcDir, files)

		// Make target dir read-only
		require.NoError(t, os.Chmod(targetDir, 0555))
		// Defer ensuring the directory is writable again for cleanup
		defer func() {
			if err := os.Chmod(targetDir, 0755); err != nil {
				t.Logf("Error restoring permissions for cleanup on %s: %v", targetDir, err)
			}
		}()

		// Extract - WriteFileAtomic should fail
		err := e.Extract(context.Background(), archivePath, targetDir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "write file file1.txt")  // Check for wrapper error
		assert.Contains(t, err.Error(), "create temporary file") // Check for inner WriteFileAtomic error
		assert.Contains(t, err.Error(), "permission denied")
	})

	// Tests context cancellation during tar.gz extraction.
	t.Run("CancelTarGz", func(t *testing.T) {
		e, srcDir, targetDir := setupExtractorTest(t)
		defer cleanupExtractorTest(t, srcDir, targetDir)

		// Create a simple archive
		files := map[string]string{"file1.txt": "content1"}
		archivePath := createTestTarGz(t, srcDir, files)

		// Create a cancellable context
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		// Extract
		err := e.Extract(ctx, archivePath, targetDir)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	})

	// Tests context cancellation during zip extraction.
	t.Run("CancelZip", func(t *testing.T) {
		e, srcDir, targetDir := setupExtractorTest(t)
		defer cleanupExtractorTest(t, srcDir, targetDir)

		// Create a simple archive
		files := map[string]string{"fileA.txt": "contentA"}
		archivePath := createTestZip(t, srcDir, files)

		// Create a cancellable context
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		// Extract
		err := e.Extract(ctx, archivePath, targetDir)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	})

	// More sub-tests here...
}

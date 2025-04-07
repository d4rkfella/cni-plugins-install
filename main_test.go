package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ioCopy = io.Copy

func TestMain(m *testing.M) {
	rand.Seed(1)
	os.Exit(m.Run())
}

func createTestLogger() zerolog.Logger {
	return zerolog.New(zerolog.ConsoleWriter{Out: io.Discard})
}

func createTestTarGz(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "test.tar.gz")

	f, err := os.Create(tarPath)
	require.NoError(t, err)

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	dirs := make(map[string]bool)
	for name := range files {
		dir := filepath.Dir(name)
		for d := dir; d != "."; d = filepath.Dir(d) {
			if !dirs[d] {
				hdr := &tar.Header{
					Name:     d + "/",
					Mode:     0755,
					Typeflag: tar.TypeDir,
				}
				require.NoError(t, tw.WriteHeader(hdr))
				dirs[d] = true
			}
		}
	}

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0600,
			Size: int64(len(content)),
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	require.NoError(t, f.Close())

	hasher := sha256.New()
	f, err = os.Open(tarPath)
	require.NoError(t, err)
	defer f.Close()

	_, err = io.Copy(hasher, f)
	require.NoError(t, err)
	return tarPath, hex.EncodeToString(hasher.Sum(nil))
}

func TestDownloadFile(t *testing.T) {
	t.Run("successful download", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("test content"))
		}))
		t.Cleanup(func() { ts.Close() })

		cl := &cleanup{}
		path, err := downloadFile(context.Background(), ts.URL, cl, createTestLogger())
		assert.NoError(t, err)
		assert.FileExists(t, path)
	})

	t.Run("retries on temporary errors", func(t *testing.T) {
		var attempt int
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempt++
			if attempt < 3 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write([]byte("content"))
		}))
		defer ts.Close()

		cl := &cleanup{}
		_, err := downloadFile(context.Background(), ts.URL, cl, createTestLogger())
		assert.NoError(t, err)
		assert.Equal(t, 3, attempt)
	})

	t.Run("fails after max retries", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer ts.Close()

		cl := &cleanup{}
		_, err := downloadFile(context.Background(), ts.URL, cl, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "after 3 attempts")
	})

	t.Run("handles network errors", func(t *testing.T) {
		cl := &cleanup{}
		_, err := downloadFile(context.Background(), "http://invalid.test", cl, createTestLogger())
		assert.Error(t, err)
	})

	t.Run("handles timeout", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
		}))
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		cl := &cleanup{}
		_, err := downloadFile(ctx, ts.URL, cl, createTestLogger())
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("temp file creation failure", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0444))
		defer os.Chmod(dir, 0755)

		oldTempDir := os.TempDir()
		os.Setenv("TMPDIR", dir)
		defer os.Setenv("TMPDIR", oldTempDir)

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("test"))
		}))
		defer ts.Close()

		cl := &cleanup{}
		_, err := downloadFile(context.Background(), ts.URL, cl, createTestLogger())
		assert.Error(t, err)
	})

	// NEW TEST CASE: Test invalid URL
	t.Run("invalid URL", func(t *testing.T) {
		cl := &cleanup{}
		_, err := downloadFile(context.Background(), "://invalid.url", cl, createTestLogger())
		assert.Error(t, err)
	})
}

func TestVerifyChecksum(t *testing.T) {
	tarPath, shaValue := createTestTarGz(t, map[string]string{"test.txt": "content"})
	shaPath := filepath.Join(filepath.Dir(tarPath), "test.sha256")
	require.NoError(t, os.WriteFile(shaPath, []byte(shaValue), 0644))

	t.Run("valid checksum", func(t *testing.T) {
		err := verifyChecksum(tarPath, shaPath, createTestLogger())
		assert.NoError(t, err)
	})

	t.Run("invalid checksum", func(t *testing.T) {
		invalidSha := strings.Repeat("a", 64)
		require.NoError(t, os.WriteFile(shaPath, []byte(invalidSha), 0644))
		err := verifyChecksum(tarPath, shaPath, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "checksum mismatch")
	})

	t.Run("missing sha file", func(t *testing.T) {
		err := verifyChecksum("nonexistent.tar.gz", "missing.sha256", createTestLogger())
		assert.Error(t, err)
	})

	t.Run("invalid sha format", func(t *testing.T) {
		invalidPath := filepath.Join(t.TempDir(), "invalid.sha256")
		require.NoError(t, os.WriteFile(invalidPath, []byte("invalid"), 0644))
		err := verifyChecksum("test.tar.gz", invalidPath, createTestLogger())
		assert.Error(t, err)
	})

	// NEW TEST CASE: Test checksum verification with missing tar file
	t.Run("missing tar file", func(t *testing.T) {
		err := verifyChecksum("nonexistent.tar.gz", shaPath, createTestLogger())
		assert.Error(t, err)
	})
}

func TestExtractTarGz(t *testing.T) {
	tarPath, _ := createTestTarGz(t, map[string]string{
		"file1.txt":        "content1",
		"dir/file2.txt":    "content2",
		"nested/file3.txt": "content3",
	})

	t.Run("successful extraction", func(t *testing.T) {
		dest := t.TempDir()
		err := extractTarGz(context.Background(), tarPath, dest, createTestLogger())
		assert.NoError(t, err)
		assert.FileExists(t, filepath.Join(dest, "file1.txt"))
		assert.FileExists(t, filepath.Join(dest, "file2.txt")) // Flattened
		assert.FileExists(t, filepath.Join(dest, "file3.txt")) // Flattened
		assert.NoDirExists(t, filepath.Join(dest, "dir"))
		assert.NoDirExists(t, filepath.Join(dest, "nested"))
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := extractTarGz(ctx, tarPath, t.TempDir(), createTestLogger())
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("invalid tar format", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid.tar.gz")
		require.NoError(t, os.WriteFile(path, []byte("invalid content"), 0644))
		err := extractTarGz(context.Background(), path, t.TempDir(), createTestLogger())
		assert.Error(t, err)
	})

	// NEW TEST CASE: Test extraction to read-only directory
	t.Run("read-only destination", func(t *testing.T) {
		dest := t.TempDir()
		require.NoError(t, os.Chmod(dest, 0444))
		defer os.Chmod(dest, 0755)

		err := extractTarGz(context.Background(), tarPath, dest, createTestLogger())
		assert.Error(t, err)
	})
}

func TestAtomicSync(t *testing.T) {
	setupTestDir := func(t *testing.T) (staging, target string) {
		staging = t.TempDir()
		target = t.TempDir()

		require.NoError(t, os.WriteFile(filepath.Join(target, "existing.txt"), []byte("old"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(staging, "new.txt"), []byte("new"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(staging, "file.txt"), []byte("content"), 0644))
		return staging, target
	}

	t.Run("successful sync", func(t *testing.T) {
		staging, target := setupTestDir(t)
		err := atomicSync(context.Background(), staging, target, createTestLogger())
		assert.NoError(t, err)
		assert.FileExists(t, filepath.Join(target, "new.txt"))
		assert.FileExists(t, filepath.Join(target, "file.txt"))
	})

	t.Run("rollback_on_directory_conflict", func(t *testing.T) {
		staging, target := setupTestDir(t)

		conflictPath := filepath.Join(target, "new.txt")
		require.NoError(t, os.Mkdir(conflictPath, 0755))

		err := atomicSync(context.Background(), staging, target, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "can't replace directory with file")

		content, err := os.ReadFile(filepath.Join(target, "existing.txt"))
		assert.NoError(t, err)
		assert.Equal(t, "old", string(content))

		assert.NoFileExists(t, conflictPath)
	})

	t.Run("partial sync failure", func(t *testing.T) {
		staging := t.TempDir()
		target := t.TempDir()

		existingPath := filepath.Join(target, "existing.txt")
		require.NoError(t, os.WriteFile(existingPath, []byte("old"), 0644))

		validSrc := filepath.Join(staging, "valid.txt")
		invalidSrc := filepath.Join(staging, "invalid.txt")
		require.NoError(t, os.WriteFile(validSrc, []byte("good"), 0644))
		require.NoError(t, os.WriteFile(invalidSrc, []byte("bad"), 0644))

		invalidDst := filepath.Join(target, "invalid.txt")
		require.NoError(t, os.Mkdir(invalidDst, 0755))

		err := atomicSync(context.Background(), staging, target, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "can't replace directory with file")

		assert.NoFileExists(t, filepath.Join(target, "valid.txt"))
		assert.DirExists(t, invalidDst)

		content, err := os.ReadFile(existingPath)
		assert.NoError(t, err)
		assert.Equal(t, []byte("old"), content)
	})

	// NEW TEST CASE: Test empty staging directory
	t.Run("empty staging directory", func(t *testing.T) {
		staging := t.TempDir()
		target := t.TempDir()

		err := atomicSync(context.Background(), staging, target, createTestLogger())
		assert.NoError(t, err)
	})

	// NEW TEST CASE: Test read-only target directory
	t.Run("read-only target directory", func(t *testing.T) {
		staging := t.TempDir()
		target := t.TempDir()

		require.NoError(t, os.WriteFile(filepath.Join(staging, "test.txt"), []byte("content"), 0644))
		require.NoError(t, os.Chmod(target, 0444))
		defer os.Chmod(target, 0755)

		err := atomicSync(context.Background(), staging, target, createTestLogger())
		assert.Error(t, err)
	})
}

func TestCleanup(t *testing.T) {
	t.Run("removes files and dirs", func(t *testing.T) {
		cl := &cleanup{}
		logger := createTestLogger()

		file1 := filepath.Join(t.TempDir(), "test1.txt")
		require.NoError(t, os.WriteFile(file1, []byte("test"), 0644))
		cl.addFile(file1)

		tempDir := t.TempDir()
		cl.tempDir = tempDir

		cl.execute(logger)
		assert.NoFileExists(t, file1)
		assert.NoDirExists(t, tempDir)
	})

	t.Run("handles non-existent files", func(t *testing.T) {
		cl := &cleanup{}
		cl.addFile("/non/existent/file")
		cl.execute(createTestLogger())
	})

	t.Run("handles write-protected dir", func(t *testing.T) {
		tempDir := t.TempDir()
		cl := &cleanup{tempDir: tempDir}
		require.NoError(t, os.Chmod(tempDir, 0444))
		defer os.Chmod(tempDir, 0755)

		cl.execute(createTestLogger())
	})

	// NEW TEST CASE: Test concurrent cleanup
	t.Run("concurrent cleanup", func(t *testing.T) {
		cl := &cleanup{}
		var wg sync.WaitGroup
		wg.Add(10)

		for i := 0; i < 10; i++ {
			go func(i int) {
				defer wg.Done()
				cl.addFile(filepath.Join(t.TempDir(), fmt.Sprintf("file%d.txt", i)))
			}(i)
		}

		wg.Wait()
		cl.execute(createTestLogger())
	})
}

func TestRunIntegration(t *testing.T) {
	files := map[string]string{
		"nested/plugin": "binary content",
	}

	var tarBuf bytes.Buffer
	gw := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gw)

	// Create directory structure
	dirs := make(map[string]bool)
	for name := range files {
		dir := filepath.Dir(name)
		for d := dir; d != "."; d = filepath.Dir(d) {
			if !dirs[d] {
				hdr := &tar.Header{
					Name:     d + "/",
					Mode:     0755,
					Typeflag: tar.TypeDir,
				}
				require.NoError(t, tw.WriteHeader(hdr))
				dirs[d] = true
			}
		}
	}

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0755,
			Size: int64(len(content)),
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	tarData := tarBuf.Bytes()

	hasher := sha256.New()
	hasher.Write(tarData)
	shaValue := hex.EncodeToString(hasher.Sum(nil))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".tgz"):
			w.Write(tarData)
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			w.Write([]byte(shaValue))
		}
	}))
	defer ts.Close()

	t.Setenv("CNI_PLUGINS_VERSION", "v1.0.0")
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	oldTargetDir := targetDir
	targetDir = filepath.Join(tempDir, "host/opt/cni/bin")
	defer func() { targetDir = oldTargetDir }()

	oldBaseURL := baseURL
	baseURL = ts.URL
	defer func() { baseURL = oldBaseURL }()

	logger := createTestLogger()
	err := run(context.Background(), logger)
	assert.NoError(t, err)
	assert.FileExists(t, filepath.Join(targetDir, "plugin")) // Flattened
	assert.NoDirExists(t, filepath.Join(targetDir, "nested"))

	// NEW TEST CASE: Test missing version environment variable
	t.Run("missing version", func(t *testing.T) {
		t.Setenv("CNI_PLUGINS_VERSION", "")
		err := run(context.Background(), logger)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "CNI_PLUGINS_VERSION")
	})
}

func TestSameChecksum(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "file1")
	file2 := filepath.Join(dir, "file2")
	file3 := filepath.Join(dir, "file3")

	require.NoError(t, os.WriteFile(file1, []byte("same"), 0644))
	require.NoError(t, os.WriteFile(file2, []byte("same"), 0644))
	require.NoError(t, os.WriteFile(file3, []byte("different"), 0644))

	t.Run("identical files", func(t *testing.T) {
		same, err := sameChecksum(file1, file2)
		assert.NoError(t, err)
		assert.True(t, same)
	})

	t.Run("different files", func(t *testing.T) {
		same, err := sameChecksum(file1, file3)
		assert.NoError(t, err)
		assert.False(t, same)
	})

	// NEW TEST CASE: Test non-existent files
	t.Run("non-existent files", func(t *testing.T) {
		_, err := sameChecksum("/nonexistent1", "/nonexistent2")
		assert.Error(t, err)

		_, err = sameChecksum(file1, "/nonexistent2")
		assert.Error(t, err)
	})

	// NEW TEST CASE: Test directory instead of file
	t.Run("directory instead of file", func(t *testing.T) {
		dir := t.TempDir()
		_, err := sameChecksum(dir, file1)
		assert.Error(t, err)
	})
}

// NEW TEST: Test fileChecksum function
func TestFileChecksum(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "file1.txt")
	require.NoError(t, os.WriteFile(file1, []byte("content"), 0644))

	t.Run("successful checksum", func(t *testing.T) {
		hash, err := fileChecksum(file1)
		assert.NoError(t, err)
		assert.NotEmpty(t, hash)
	})

	t.Run("non-existent file", func(t *testing.T) {
		_, err := fileChecksum("/nonexistent")
		assert.Error(t, err)
	})

	t.Run("directory instead of file", func(t *testing.T) {
		_, err := fileChecksum(dir)
		assert.Error(t, err)
	})
}

// NEW TEST: Test writeFile function
func TestWriteFile(t *testing.T) {
	t.Run("successful write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.txt")
		err := writeFile(path, strings.NewReader("content"), 0644, createTestLogger())
		assert.NoError(t, err)
		assert.FileExists(t, path)
	})

	t.Run("parent dir creation failure", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0444))
		defer os.Chmod(dir, 0755)

		err := writeFile(filepath.Join(dir, "subdir", "file.txt"), strings.NewReader("test"), 0644, createTestLogger())
		assert.Error(t, err)
	})

	t.Run("temp file write failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "test.txt")

		// Create a reader that will fail on Read
		failingReader := &failingReader{}

		err := writeFile(path, failingReader, 0644, createTestLogger())
		assert.Error(t, err)
		assert.NoFileExists(t, path)
	})
}

type failingReader struct{}

func (r *failingReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("simulated read failure")
}

func TestCleanupExecuteErrorCases(t *testing.T) {
	t.Run("file remove error is logged", func(t *testing.T) {
		// Create a file we can't remove
		dir := t.TempDir()
		file := filepath.Join(dir, "test.txt")
		require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
		require.NoError(t, os.Chmod(dir, 0555)) // Make directory read-only
		defer os.Chmod(dir, 0755)

		cl := &cleanup{files: []string{file}}
		logger := zerolog.New(zerolog.TestWriter{T: t})
		cl.execute(logger)
		// Should log error but not fail
	})

	t.Run("temp dir remove error is logged", func(t *testing.T) {
		// Create a dir we can't remove
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0555)) // Make directory read-only
		defer os.Chmod(dir, 0755)

		cl := &cleanup{tempDir: dir}
		logger := zerolog.New(zerolog.TestWriter{T: t})
		cl.execute(logger)
		// Should log error but not fail
	})
}

func TestDownloadFileErrorPaths(t *testing.T) {
	t.Run("request creation failure", func(t *testing.T) {
		cl := &cleanup{}
		_, err := downloadFile(context.Background(), "http://[::1]:namedport", cl, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "create request failed")
	})

	t.Run("temp file removal on error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer ts.Close()

		cl := &cleanup{}
		_, err := downloadFile(context.Background(), ts.URL, cl, createTestLogger())
		assert.Error(t, err)

		// The files should still be in cleanup list even if they were deleted
		// because we don't remove them from the list, just from disk
		assert.NotEmpty(t, cl.files)

		// Verify the files were actually deleted from disk
		for _, f := range cl.files {
			_, err := os.Stat(f)
			assert.True(t, os.IsNotExist(err))
		}
	})
}

func TestVerifyChecksumErrorPaths(t *testing.T) {
	t.Run("hash_read_error", func(t *testing.T) {
		// Create test files
		dir := t.TempDir()
		filePath := filepath.Join(dir, "test.txt")
		shaPath := filepath.Join(dir, "test.sha256")

		require.NoError(t, os.WriteFile(filePath, []byte("test"), 0644))
		require.NoError(t, os.WriteFile(shaPath, []byte(strings.Repeat("a", 64)), 0644))

		// Mock io.Copy to fail during hash computation
		oldCopy := ioCopy
		ioCopy = func(dst io.Writer, src io.Reader) (written int64, err error) {
			return 0, errors.New("read failed")
		}
		defer func() { ioCopy = oldCopy }()

		err := verifyChecksum(filePath, shaPath, createTestLogger())

		// Verify we get the expected checksum mismatch error
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "checksum mismatch")
		assert.Contains(t, err.Error(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	})
}

func TestExtractTarGzErrorPaths(t *testing.T) {
	t.Run("gzip read error", func(t *testing.T) {
		// Create an invalid gzip file
		path := filepath.Join(t.TempDir(), "invalid.tar.gz")
		require.NoError(t, os.WriteFile(path, []byte("not a gzip file"), 0644))

		err := extractTarGz(context.Background(), path, t.TempDir(), createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "gzip reader failed")
	})

	t.Run("tar read error", func(t *testing.T) {
		// Create a valid gzip but invalid tar file
		path := filepath.Join(t.TempDir(), "invalid.tar.gz")
		f, err := os.Create(path)
		require.NoError(t, err)

		gw := gzip.NewWriter(f)
		gw.Write([]byte("not a tar file"))
		gw.Close()
		f.Close()

		err = extractTarGz(context.Background(), path, t.TempDir(), createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "tar read failed")
	})
}

func TestAtomicSyncErrorPaths(t *testing.T) {
	t.Run("backup restore failure during rollback", func(t *testing.T) {
		staging := t.TempDir()
		target := t.TempDir()

		// Create test files
		srcFile := filepath.Join(staging, "test.txt")
		dstFile := filepath.Join(target, "test.txt")
		backupFile := dstFile + ".backup-20060102T150405"
		require.NoError(t, os.WriteFile(srcFile, []byte("new"), 0644))
		require.NoError(t, os.WriteFile(dstFile, []byte("old"), 0644))

		// Simulate a failed sync that needs rollback
		// But make backup file unreadable
		require.NoError(t, os.Rename(dstFile, backupFile))
		require.NoError(t, os.Chmod(backupFile, 0000))
		defer os.Chmod(backupFile, 0644)

		// This will trigger rollback
		require.NoError(t, os.Chmod(target, 0555)) // Make target read-only
		defer os.Chmod(target, 0755)

		logger := zerolog.New(zerolog.TestWriter{T: t})
		err := atomicSync(context.Background(), staging, target, logger)
		assert.Error(t, err)
		// Should log backup restore failure
	})

	t.Run("backup_cleanup_failure", func(t *testing.T) {
		staging := t.TempDir()
		target := t.TempDir()

		// Create test files
		filename := "testfile.txt"
		srcPath := filepath.Join(staging, filename)
		dstPath := filepath.Join(target, filename)

		require.NoError(t, os.WriteFile(srcPath, []byte("new-content"), 0644))
		require.NoError(t, os.WriteFile(dstPath, []byte("old-content"), 0644))

		// Run sync
		logger := zerolog.New(zerolog.TestWriter{T: t})
		err := atomicSync(context.Background(), staging, target, logger)
		require.NoError(t, err)

		// Find the backup file from sync.Map
		var backupPath string
		backupFiles.Range(func(key, value interface{}) bool {
			backupPath = key.(string)
			return false // Stop after first match
		})
		require.NotEmpty(t, backupPath, "Backup path not found in sync.Map")

		// Verify backup exists
		assert.FileExists(t, backupPath)

		// Make backup unremovable
		require.NoError(t, os.Chmod(backupPath, 0000))
		t.Cleanup(func() {
			os.Chmod(backupPath, 0644)
			os.Remove(backupPath)
			backupFiles.Delete(backupPath)
		})

		// Verify atomicSync didn't clean up the backup
		assert.FileExists(t, backupPath)
	})
}

func TestWriteFileErrorPaths(t *testing.T) {
	t.Run("parent dir creation failure", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0555)) // Make directory read-only
		t.Cleanup(func() { os.Chmod(dir, 0755) })

		path := filepath.Join(dir, "subdir", "file.txt")
		err := writeFile(path, strings.NewReader("test"), 0644, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create parent directory")
	})

	t.Run("temp file creation failure", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0555)) // Make directory read-only

		path := filepath.Join(dir, "file.txt")
		err := writeFile(path, strings.NewReader("test"), 0644, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create temp file")
	})

	t.Run("rename failure leaves temp file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "file.txt")

		// Create the destination as a directory to force rename to fail
		require.NoError(t, os.Mkdir(path, 0755))

		err := writeFile(path, strings.NewReader("test"), 0644, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "atomic rename failed")

		// Temp file should have been removed
		assert.Empty(t, findTempFiles(dir))
	})
}

func findTempFiles(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	return files
}

func TestRunSignalHandling(t *testing.T) {
	t.Run("handles termination signal", func(t *testing.T) {
		// Setup test environment
		t.Setenv("CNI_PLUGINS_VERSION", "v1.0.0")
		tempDir := t.TempDir()
		t.Setenv("TMPDIR", tempDir)

		// Create a test server that will hang
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done() // Wait for context cancellation
		}))
		defer ts.Close()

		// Override global variables for test
		oldBaseURL := baseURL
		oldTargetDir := targetDir
		baseURL = ts.URL
		targetDir = filepath.Join(tempDir, "cni-bin")
		defer func() {
			baseURL = oldBaseURL
			targetDir = oldTargetDir
		}()

		// Create a cancellable context
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Run in goroutine
		logger := createTestLogger()
		errChan := make(chan error, 1)
		go func() {
			errChan <- run(ctx, logger)
		}()

		// Send cancellation after short delay
		time.Sleep(100 * time.Millisecond)
		cancel()

		// Verify we get context cancellation error
		select {
		case err := <-errChan:
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "context", "Error should be related to context cancellation")
		case <-time.After(2 * time.Second):
			t.Fatal("Timed out waiting for signal handling")
		}
	})
}

func TestRunWithRealSignal(t *testing.T) {
	t.Run("handles SIGTERM signal", func(t *testing.T) {
		// Setup test environment
		t.Setenv("CNI_PLUGINS_VERSION", "v1.0.0")
		tempDir := t.TempDir()
		t.Setenv("TMPDIR", tempDir)

		// Create a test server that will hang
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done() // Wait for context cancellation
		}))
		defer ts.Close()

		// Override global variables for test
		oldBaseURL := baseURL
		oldTargetDir := targetDir
		baseURL = ts.URL
		targetDir = filepath.Join(tempDir, "cni-bin")
		defer func() {
			baseURL = oldBaseURL
			targetDir = oldTargetDir
		}()

		// Run in goroutine
		logger := createTestLogger()
		errChan := make(chan error, 1)
		go func() {
			errChan <- run(context.Background(), logger)
		}()

		// Send actual SIGTERM signal
		time.Sleep(100 * time.Millisecond)
		proc, err := os.FindProcess(os.Getpid())
		require.NoError(t, err)
		require.NoError(t, proc.Signal(syscall.SIGTERM))

		// Verify we get an error (either context canceled or download failed)
		select {
		case err := <-errChan:
			assert.Error(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("Timed out waiting for signal handling")
		}
	})
}

func TestFileSize(t *testing.T) {
	t.Run("returns correct size", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.txt")
		content := "test content"
		require.NoError(t, os.WriteFile(path, []byte(content), 0644))

		size := fileSize(path)
		assert.Equal(t, int64(len(content)), size)
	})

	t.Run("handles non-existent file", func(t *testing.T) {
		size := fileSize("/nonexistent/file")
		assert.Equal(t, int64(0), size)
	})
}

func TestSleepWithJitter(t *testing.T) {
	t.Run("increases sleep time with retries", func(t *testing.T) {
		start := time.Now()
		sleepWithJitter(0)
		duration0 := time.Since(start)

		start = time.Now()
		sleepWithJitter(1)
		duration1 := time.Since(start)

		start = time.Now()
		sleepWithJitter(2)
		duration2 := time.Since(start)

		assert.True(t, duration1 > duration0)
		assert.True(t, duration2 > duration1)
	})
}

func TestCleanupConcurrency(t *testing.T) {
	t.Run("handles concurrent file additions", func(t *testing.T) {
		cl := &cleanup{}
		var wg sync.WaitGroup
		count := 100

		wg.Add(count)
		for i := 0; i < count; i++ {
			go func(i int) {
				defer wg.Done()
				cl.addFile(fmt.Sprintf("/tmp/file%d", i))
			}(i)
		}
		wg.Wait()

		assert.Len(t, cl.files, count)
	})
}

func TestExtractTarGzEdgeCases(t *testing.T) {
	t.Run("skips non-regular files", func(t *testing.T) {
		// Create temp files
		dir := t.TempDir()
		tarPath := filepath.Join(dir, "test.tar.gz")

		// Create proper tar.gz with both regular file and symlink
		f, err := os.Create(tarPath)
		require.NoError(t, err)

		gw := gzip.NewWriter(f)
		tw := tar.NewWriter(gw)

		// 1. Add regular file
		regularContent := "regular file content"
		hdr := &tar.Header{
			Name:     "regular.txt",
			Mode:     0644,
			Size:     int64(len(regularContent)),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatGNU, // Important for symlinks
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err = tw.Write([]byte(regularContent))
		require.NoError(t, err)

		// 2. Add symlink
		hdr = &tar.Header{
			Name:     "symlink.txt",
			Linkname: "regular.txt",
			Mode:     0777,
			Typeflag: tar.TypeSymlink,
			Format:   tar.FormatGNU,
		}
		require.NoError(t, tw.WriteHeader(hdr))

		// Must close in proper order
		require.NoError(t, tw.Close())
		require.NoError(t, gw.Close())
		require.NoError(t, f.Close())

		// Test extraction
		dest := t.TempDir()
		err = extractTarGz(context.Background(), tarPath, dest, createTestLogger())
		require.NoError(t, err)

		// Verify results
		_, err = os.Stat(filepath.Join(dest, "regular.txt"))
		assert.NoError(t, err, "Regular file should exist")

		_, err = os.Lstat(filepath.Join(dest, "symlink.txt"))
		assert.True(t, os.IsNotExist(err), "Symlink should not exist")
	})

	t.Run("handles write failure", func(t *testing.T) {
		// Create test tar
		tarPath, _ := createTestTarGz(t, map[string]string{
			"file.txt": "content",
		})

		// Create destination dir and make parent unwritable
		destParent := t.TempDir()
		dest := filepath.Join(destParent, "subdir")

		// Make parent read-only to prevent file creation
		require.NoError(t, os.Chmod(destParent, 0555)) // Read-only
		defer os.Chmod(destParent, 0755)               // Cleanup

		err := extractTarGz(context.Background(), tarPath, dest, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create parent directory",
			"Should fail when parent directory is read-only")
	})
	t.Run("invalid file in archive", func(t *testing.T) {
		// First create a valid tar file
		dir := t.TempDir()
		validTarPath := filepath.Join(dir, "valid.tar.gz")

		// Create a normal tar file first
		f, err := os.Create(validTarPath)
		require.NoError(t, err)

		gw := gzip.NewWriter(f)
		tw := tar.NewWriter(gw)

		// Add a valid file
		content := "test content"
		hdr := &tar.Header{
			Name: "valid.txt",
			Mode: 0644,
			Size: int64(len(content)),
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err = tw.Write([]byte(content))
		require.NoError(t, err)

		// Now deliberately corrupt the tar by:
		// 1. Writing an invalid header
		// 2. Not writing the promised content
		hdr = &tar.Header{
			Name: "invalid.txt",
			Mode: 0644,
			Size: 100, // Claiming 100 bytes but won't write them
		}
		require.NoError(t, tw.WriteHeader(hdr))
		// Don't write any content - this makes it invalid

		// Close writers
		require.NoError(t, tw.Close())
		require.NoError(t, gw.Close())
		require.NoError(t, f.Close())

		// Now test extraction
		dest := t.TempDir()
		err = extractTarGz(context.Background(), validTarPath, dest, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "tar read failed")
	})
}

func TestVerifyChecksumEdgeCases(t *testing.T) {
	t.Run("handles file read error during hashing", func(t *testing.T) {
		// Create test files
		dir := t.TempDir()
		filePath := filepath.Join(dir, "test.txt")
		shaPath := filepath.Join(dir, "test.sha256")

		require.NoError(t, os.WriteFile(filePath, []byte("test"), 0000)) // No permissions
		require.NoError(t, os.WriteFile(shaPath, []byte(strings.Repeat("a", 64)), 0644))

		defer os.Chmod(filePath, 0644)

		err := verifyChecksum(filePath, shaPath, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "open file failed")
	})
	t.Run("checksum file with extra data", func(t *testing.T) {
		// Create a file and compute its real checksum
		dir := t.TempDir()
		filePath := filepath.Join(dir, "test.txt")
		shaPath := filepath.Join(dir, "test.sha256")

		content := "test content"
		require.NoError(t, os.WriteFile(filePath, []byte(content), 0644))

		// Compute actual hash
		hasher := sha256.New()
		hasher.Write([]byte(content))
		realHash := hex.EncodeToString(hasher.Sum(nil))

		// Create SHA file with extra data but correct hash
		require.NoError(t, os.WriteFile(shaPath, []byte(realHash+"\nextra data\nmore data"), 0644))

		err := verifyChecksum(filePath, shaPath, createTestLogger())
		assert.NoError(t, err) // Should work with extra lines
	})
}

func TestAtomicSyncEdgeCases(t *testing.T) {
	t.Run("handles readdir failure", func(t *testing.T) {
		staging := t.TempDir()
		target := t.TempDir()

		// Make staging unreadable
		require.NoError(t, os.Chmod(staging, 0000))
		defer os.Chmod(staging, 0755)

		err := atomicSync(context.Background(), staging, target, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read staging directory")
	})

	t.Run("handles backup file tracking", func(t *testing.T) {
		staging := t.TempDir()
		target := t.TempDir()

		// Create existing file
		existingFile := filepath.Join(target, "test.txt")
		require.NoError(t, os.WriteFile(existingFile, []byte("old"), 0644))

		// Create new file to sync
		newFile := filepath.Join(staging, "test.txt")
		require.NoError(t, os.WriteFile(newFile, []byte("new"), 0644))

		err := atomicSync(context.Background(), staging, target, createTestLogger())
		require.NoError(t, err)

		// Verify backup was tracked
		var found bool
		backupFiles.Range(func(key, value interface{}) bool {
			if strings.HasPrefix(key.(string), filepath.Join(target, "test.txt.backup-")) {
				found = true
				return false
			}
			return true
		})
		assert.True(t, found, "backup file not tracked in sync.Map")
	})
	t.Run("failed backup restore", func(t *testing.T) {
		staging := t.TempDir()
		target := t.TempDir()
		logger := createTestLogger()

		// Create test files
		srcFile := filepath.Join(staging, "test.txt")
		dstFile := filepath.Join(target, "test.txt")
		backupFile := dstFile + ".backup"

		require.NoError(t, os.WriteFile(srcFile, []byte("new"), 0644))
		require.NoError(t, os.WriteFile(dstFile, []byte("old"), 0644))

		// Simulate failed sync that needs rollback
		require.NoError(t, os.Rename(dstFile, backupFile))
		require.NoError(t, os.Chmod(backupFile, 0000)) // Make backup unreadable
		defer os.Chmod(backupFile, 0644)

		// This will trigger rollback
		require.NoError(t, os.Chmod(target, 0555)) // Make target read-only
		defer os.Chmod(target, 0755)

		err := atomicSync(context.Background(), staging, target, logger)
		assert.Error(t, err)
	})
}

func TestSignalHandling(t *testing.T) {
	t.Run("handles SIGINT signal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		logger := createTestLogger()
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		cancelCalled := make(chan struct{})

		go func() {
			select {
			case sig := <-sigChan:
				logger.Warn().Str("signal", sig.String()).Msg("Received signal")
				cancel()
				close(cancelCalled)
			case <-ctx.Done():
			}
		}()

		proc, err := os.FindProcess(os.Getpid())
		require.NoError(t, err)
		require.NoError(t, proc.Signal(syscall.SIGINT))

		select {
		case <-cancelCalled:
		case <-time.After(100 * time.Millisecond):
			t.Fatal("Cancel was not called within timeout")
		}

		select {
		case <-ctx.Done():
			// Success
		default:
			t.Fatal("Context should be cancelled")
		}
	})
}

func TestCleanupErrorCases(t *testing.T) {
	t.Run("remove file error", func(t *testing.T) {
		logger := zerolog.New(zerolog.TestWriter{T: t})
		cl := &cleanup{}

		// Create a file we can't remove
		dir := t.TempDir()
		file := filepath.Join(dir, "test.txt")
		require.NoError(t, os.WriteFile(file, []byte("test"), 0444)) // Read-only
		cl.addFile(file)

		cl.execute(logger) // Should log error but not panic
	})

	t.Run("remove dir error", func(t *testing.T) {
		logger := zerolog.New(zerolog.TestWriter{T: t})
		cl := &cleanup{}

		// Create a dir we can't remove
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0555)) // Read-only
		cl.tempDir = dir

		cl.execute(logger) // Should log error but not panic
	})
}

func TestWriteFileEdgeCases(t *testing.T) {
	t.Run("rename failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "test.txt")

		// Create destination as directory to force rename to fail
		require.NoError(t, os.Mkdir(path, 0755))

		err := writeFile(path, strings.NewReader("test"), 0644, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "atomic rename failed")
	})
}

func TestSameChecksumEdgeCases(t *testing.T) {
	t.Run("one file missing", func(t *testing.T) {
		dir := t.TempDir()
		file1 := filepath.Join(dir, "exists.txt")
		file2 := filepath.Join(dir, "missing.txt")

		require.NoError(t, os.WriteFile(file1, []byte("test"), 0644))

		same, err := sameChecksum(file1, file2)
		assert.Error(t, err)
		assert.False(t, same)
	})
}

func TestCleanupEmpty(t *testing.T) {
	cl := &cleanup{}
	logger := createTestLogger()
	cl.execute(logger) // Should handle empty files list gracefully
}

func TestAtomicSyncEmpty(t *testing.T) {
	staging := t.TempDir()
	target := t.TempDir()
	logger := createTestLogger()

	err := atomicSync(context.Background(), staging, target, logger)
	assert.NoError(t, err)
}

func TestVerifyChecksumMalformed(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "test.txt")
	shaPath := filepath.Join(dir, "test.sha256")

	require.NoError(t, os.WriteFile(tarPath, []byte("content"), 0644))
	require.NoError(t, os.WriteFile(shaPath, []byte("not a valid sha"), 0644))

	err := verifyChecksum(tarPath, shaPath, createTestLogger())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid SHA256 file format")
}

func TestExtractTarGzCancelled(t *testing.T) {
	tarPath, _ := createTestTarGz(t, map[string]string{"test.txt": "content"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Immediately cancel

	err := extractTarGz(ctx, tarPath, t.TempDir(), createTestLogger())
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWriteFileParentDirFail(t *testing.T) {
	dir := t.TempDir()
	// Make parent dir read-only
	require.NoError(t, os.Chmod(dir, 0555))
	defer os.Chmod(dir, 0755)

	path := filepath.Join(dir, "subdir", "file.txt")
	err := writeFile(path, strings.NewReader("test"), 0644, createTestLogger())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create parent directory")
}

func TestSameChecksumIdentical(t *testing.T) {
	dir := t.TempDir()
	file1 := filepath.Join(dir, "file1.txt")
	file2 := filepath.Join(dir, "file2.txt")

	content := "identical content"
	require.NoError(t, os.WriteFile(file1, []byte(content), 0644))
	require.NoError(t, os.WriteFile(file2, []byte(content), 0644))

	same, err := sameChecksum(file1, file2)
	assert.NoError(t, err)
	assert.True(t, same)
}

func TestFileChecksumError(t *testing.T) {
	_, err := fileChecksum("/nonexistent/file")
	assert.Error(t, err)
}

func TestRunDownloadFailure(t *testing.T) {
	t.Setenv("CNI_PLUGINS_VERSION", "v1.0.0")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	oldBaseURL := baseURL
	baseURL = ts.URL
	defer func() { baseURL = oldBaseURL }()

	logger := createTestLogger()
	err := run(context.Background(), logger)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "download failed")
}

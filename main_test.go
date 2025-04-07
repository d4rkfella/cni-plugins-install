package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		defer ts.Close()

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
	err := run(logger)
	assert.NoError(t, err)
	assert.FileExists(t, filepath.Join(targetDir, "plugin")) // Flattened
	assert.NoDirExists(t, filepath.Join(targetDir, "nested"))
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
}

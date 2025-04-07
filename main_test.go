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
}

func TestExtractTarGz(t *testing.T) {
	tarPath, _ := createTestTarGz(t, map[string]string{
		"file1.txt":       "content1",
		"dir/file2.txt":   "content2",
		"nested/file3.txt": "content3",
	})

	t.Run("successful extraction", func(t *testing.T) {
		dest := t.TempDir()
		err := extractTarGz(context.Background(), tarPath, dest, createTestLogger())
		assert.NoError(t, err)
		assert.FileExists(t, filepath.Join(dest, "file1.txt"))
		assert.FileExists(t, filepath.Join(dest, "dir/file2.txt"))
		assert.FileExists(t, filepath.Join(dest, "nested/file3.txt"))
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := extractTarGz(ctx, tarPath, t.TempDir(), createTestLogger())
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestAtomicSync(t *testing.T) {
	setupTestDir := func(t *testing.T) (staging, target string) {
		staging = t.TempDir()
		target = t.TempDir()

		require.NoError(t, os.WriteFile(filepath.Join(target, "existing.txt"), []byte("old"), 0644))
		
		require.NoError(t, os.MkdirAll(filepath.Join(staging, "subdir/nested"), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(staging, "new.txt"), []byte("new"), 0644))
		require.NoError(t, os.WriteFile(
			filepath.Join(staging, "subdir/nested/file.txt"), 
			[]byte("content"), 
			0644,
		))
		
		return staging, target
	}

	t.Run("successful sync with nested dirs", func(t *testing.T) {
		staging, target := setupTestDir(t)
		err := atomicSync(context.Background(), staging, target, createTestLogger())
		assert.NoError(t, err)
		
		assert.FileExists(t, filepath.Join(target, "new.txt"))
		assert.FileExists(t, filepath.Join(target, "subdir/nested/file.txt"))
	})

	t.Run("rollback on directory conflict", func(t *testing.T) {
		staging, target := setupTestDir(t)
		
		conflictPath := filepath.Join(target, "new.txt")
		require.NoError(t, os.Mkdir(conflictPath, 0755))
		
		err := atomicSync(context.Background(), staging, target, createTestLogger())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "can't replace directory with file")
		
		content, _ := os.ReadFile(filepath.Join(target, "existing.txt"))
		assert.Equal(t, "old", string(content))
	})
}

func TestCleanup(t *testing.T) {
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
}

func TestRunIntegration(t *testing.T) {
	files := map[string]string{
		"plugin":           "binary content",
		"nested/plugin":    "nested content",
	}
	
	var tarBuf bytes.Buffer
	gw := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		dir := filepath.Dir(name)
		if dir != "." {
			hdr := &tar.Header{
				Name:     dir + "/",
				Mode:     0755,
				Typeflag: tar.TypeDir,
			}
			tw.WriteHeader(hdr)
		}

		hdr := &tar.Header{
			Name: name,
			Mode: 0755,
			Size: int64(len(content)),
		}
		tw.WriteHeader(hdr)
		tw.Write([]byte(content))
	}

	tw.Close()
	gw.Close()
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
	
	assert.FileExists(t, filepath.Join(targetDir, "plugin"))
	assert.FileExists(t, filepath.Join(targetDir, "nested/plugin"))
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
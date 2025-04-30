package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
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

func createTempFile(t *testing.T, cl *cleanup) string {
	tmpFile, err := os.CreateTemp("", tempFilePrefix+"*")
	require.NoError(t, err)
	tmpName := tmpFile.Name()
	cl.addFile(tmpName)
	return tmpName
}

func TestRun_MissingVersion(t *testing.T) {
	os.Unsetenv("CNI_PLUGINS_VERSION")
	logger := zerolog.New(io.Discard)
	err := run(context.Background(), logger)
	assert.ErrorContains(t, err, "CNI_PLUGINS_VERSION")
}

func TestRun_Success(t *testing.T) {
	tarContent := new(bytes.Buffer)
	gzWriter := gzip.NewWriter(tarContent)
	tarWriter := tar.NewWriter(gzWriter)

	content := []byte("test content")
	header := &tar.Header{
		Name:     "testfile",
		Typeflag: tar.TypeReg,
		Mode:     0644,
		Size:     int64(len(content)),
		ModTime:  time.Now(),
	}
	tarWriter.WriteHeader(header)
	tarWriter.Write(content)
	tarWriter.Close()
	gzWriter.Close()

	sum := sha256.Sum256(tarContent.Bytes())

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".tgz"):
			w.Write(tarContent.Bytes())
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			fmt.Fprintf(w, "%x  cni-plugins-linux-amd64-v1.0.0.tgz", sum[:])
		}
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	originalCfg := cfg
	cfg = Config{
		BaseURL:         ts.URL,
		TargetDir:       tmpDir,
		DownloadTimeout: 5 * time.Second,
		MaxRetries:      3,
		BufferSize:      1024,
	}
	t.Cleanup(func() { cfg = originalCfg })
	t.Setenv("CNI_PLUGINS_VERSION", "v1.0.0")

	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf).Level(zerolog.DebugLevel)

	cl := &cleanup{}
	t.Cleanup(func() {
		cl.execute(logger)
	})

	err := run(context.Background(), logger)
	assert.NoError(t, err, "Installation failed. Logs:\n%s", logBuf.String())

	targetPath := filepath.Join(tmpDir, "testfile")
	_, err = os.Stat(targetPath)
	assert.NoError(t, err, "Main file should exist")

	entries, err := os.ReadDir(tmpDir)
	assert.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), ".cni-staging-"),
			"Staging directory not cleaned up: %s", entry.Name())
	}

	downloadFiles, _ := filepath.Glob(filepath.Join(os.TempDir(), tempFilePrefix+"*"))
	assert.Empty(t, downloadFiles, "Leftover download files: %v", downloadFiles)

	finalEntries, _ := os.ReadDir(tmpDir)
	assert.NotEmpty(t, finalEntries, "Target dir should contain installed files")
	assert.Len(t, finalEntries, 1, "Should only contain the testfile")
}

func TestRun_DownloadFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	tmpDir := t.TempDir()

	originalCfg := cfg
	cfg = Config{
		BaseURL:         ts.URL,
		TargetDir:       tmpDir,
		DownloadTimeout: 5 * time.Second,
		MaxRetries:      3,
		BufferSize:      1024,
	}
	t.Cleanup(func() { cfg = originalCfg })

	t.Setenv("CNI_PLUGINS_VERSION", "v1.0.0")

	logger := zerolog.New(io.Discard)
	err := run(context.Background(), logger)
	
	assert.ErrorContains(t, err, "download failed")
	
	assert.True(t, strings.Contains(err.Error(), "404 Not Found"), 
		"Expected HTTP 404 error, got: %v", err)
}

func TestDownloadFile_Retries(t *testing.T) {
	attempt := 0
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
	logger := zerolog.New(io.Discard)

	_, err := downloadFile(context.Background(), ts.URL, cl, logger)
	assert.NoError(t, err)
	assert.Equal(t, 3, attempt)
}

func TestVerifyChecksum_Mismatch(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "test.tgz")
	shaPath := filepath.Join(tmpDir, "test.sha256")

	require.NoError(t, os.WriteFile(tarPath, []byte("content"), 0644))
	require.NoError(t, os.WriteFile(shaPath, []byte(strings.Repeat("a", 64)+"  test.tgz"), 0644))

	logger := zerolog.New(io.Discard)
	err := verifyChecksum(tarPath, shaPath, logger)
	assert.ErrorContains(t, err, "checksum mismatch")
}

func TestAtomicSync_Rollback(t *testing.T) {
	tmpDir := t.TempDir()
	staging := filepath.Join(tmpDir, "staging")
	target := filepath.Join(tmpDir, "target")

	require.NoError(t, os.MkdirAll(staging, 0755))
	require.NoError(t, os.MkdirAll(target, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "test"), []byte("test"), 0644))
	require.NoError(t, os.Mkdir(filepath.Join(target, "test"), 0755))

	logger := zerolog.New(io.Discard)
	err := atomicSync(context.Background(), staging, target, logger)
	assert.Error(t, err)

	fi, err := os.Stat(filepath.Join(target, "test"))
	assert.NoError(t, err)
	assert.True(t, fi.IsDir())
}

func TestExtractTarGz_InvalidFile(t *testing.T) {
	tmpDir := t.TempDir()
	tarPath := filepath.Join(tmpDir, "invalid.tgz")
	require.NoError(t, os.WriteFile(tarPath, []byte("invalid content"), 0644))

	logger := zerolog.New(io.Discard)
	err := extractTarGz(context.Background(), tarPath, tmpDir, logger)
	assert.ErrorContains(t, err, "gzip")
}

func TestWriteFileAtomic_PermissionError(t *testing.T) {
	tmpDir := t.TempDir()
	readOnlyDir := filepath.Join(tmpDir, "readonly")
	require.NoError(t, os.Mkdir(readOnlyDir, 0555))

	logger := zerolog.New(io.Discard)
	err := writeFileAtomic(filepath.Join(readOnlyDir, "test"), strings.NewReader("test"), 0644, logger)
	assert.Error(t, err)
}

func TestCleanup_Execute(t *testing.T) {
	tmpDir := t.TempDir()

	testDir := filepath.Join(tmpDir, "testdir")
	require.NoError(t, os.Mkdir(testDir, 0755))

	protectedFile := filepath.Join(testDir, "protected")
	require.NoError(t, os.WriteFile(protectedFile, []byte("test"), 0644))

	cl := &cleanup{
		files:   []string{protectedFile},
		tempDir: testDir,
	}

	require.NoError(t, os.Chmod(testDir, 0555))
	t.Cleanup(func() {
		os.Chmod(testDir, 0755)
		os.RemoveAll(testDir)
	})

	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf).Level(zerolog.DebugLevel)

	cl.execute(logger)

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "Cleanup failed", "Should log cleanup failure")
	assert.Contains(t, logOutput, "permission denied", "Should show permission error")

	_, err := os.Stat(protectedFile)
	assert.NoError(t, err, "File should still exist")

	_, err = os.Stat(testDir)
	assert.NoError(t, err, "Test directory should still exist")
}

func TestFileChecksum_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tmpFile := filepath.Join(t.TempDir(), "test")
	require.NoError(t, os.WriteFile(tmpFile, []byte("test"), 0644))
	_, err := fileChecksum(ctx, tmpFile)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestSleepWithJitter(t *testing.T) {
	start := time.Now()
	sleepWithJitter(2)
	duration := time.Since(start)

	assert.True(t, duration >= 4*time.Second)
	assert.True(t, duration < 5*time.Second)
}

func TestCleanupOldBackups(t *testing.T) {
	tmpDir := t.TempDir()
	oldBackup := filepath.Join(tmpDir, backupPrefix+"old")
	newBackup := filepath.Join(tmpDir, backupPrefix+"new")

	require.NoError(t, os.WriteFile(oldBackup, []byte("old"), 0644))
	require.NoError(t, os.WriteFile(newBackup, []byte("new"), 0644))

	oldTime := time.Now().Add(-2 * backupMaxAge)
	require.NoError(t, os.Chtimes(oldBackup, oldTime, oldTime))

	logger := zerolog.New(io.Discard)
	cleanupOldBackups(tmpDir, backupMaxAge, logger)

	_, err := os.Stat(oldBackup)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(newBackup)
	assert.NoError(t, err)
}

func createTestTarball(w io.Writer) {
	gzw := gzip.NewWriter(w)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	content := []byte("test content")
	hdr := &tar.Header{
		Name: "testfile",
		Mode: 0644,
		Size: int64(len(content)),
	}
	tw.WriteHeader(hdr)
	tw.Write(content)
}

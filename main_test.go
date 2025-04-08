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
    
    originalTmpdir := os.Getenv("TMPDIR")
    
    code := m.Run()
    
    os.Setenv("TMPDIR", originalTmpdir)
    
    os.Exit(code)
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
	originalTmpdir := os.Getenv("TMPDIR")
	t.Cleanup(func() { os.Setenv("TMPDIR", originalTmpdir) })

	var (
		mu            sync.Mutex
		retryAttempts = make(map[string]int)
	)

	tests := []struct {
		name        string
		handler     http.HandlerFunc
		setup       func(*testing.T)
		expectError string
	}{
		{
			name: "successful_download",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("test content"))
			},
		},
		{
			name: "retries_on_temporary_errors",
			handler: func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempt := retryAttempts[r.URL.Path]
				retryAttempts[r.URL.Path]++
				mu.Unlock()

				if attempt < 2 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Write([]byte("success content"))
			},
		},
		{
			name: "fails_after_max_retries",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			expectError: "after 3 attempts",
		},
		{
			name: "handles_network_errors",
			setup: func(t *testing.T) {
				os.Setenv("TMPDIR", t.TempDir())
			},
			expectError: "connection", // Broader check
		},
		{
			name: "handles_timeout",
			handler: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(100 * time.Millisecond)
			},
			expectError: context.DeadlineExceeded.Error(),
		},
		{
			name: "temp_file_creation_failure",
			setup: func(t *testing.T) {
				dir := t.TempDir()
				require.NoError(t, os.Chmod(dir, 0444))
				os.Setenv("TMPDIR", dir)
			},
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("test"))
			},
			expectError: "temp file creation failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			retryAttempts = make(map[string]int)
			mu.Unlock()
			os.Setenv("TMPDIR", originalTmpdir)

			if tt.setup != nil {
				tt.setup(t)
			}

			var ts *httptest.Server
			if tt.handler != nil {
				ts = httptest.NewServer(tt.handler)
				defer ts.Close()
			}

			ctx := context.Background()
			if tt.name == "handles_timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
			}

			cl := &cleanup{}
			url := "http://127.0.0.1:9999" // Invalid endpoint
			if ts != nil {
				url = ts.URL
			}

			_, err := downloadFile(ctx, url, cl, createTestLogger())

			if tt.expectError != "" {
				require.Error(t, err)
				assert.True(t,
					strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.expectError)) ||
					strings.Contains(strings.ToLower(err.Error()), "refused"),
					"Expected error containing %q, got: %v", tt.expectError, err,
				)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestVerifyChecksum(t *testing.T) {
	t.Parallel()
	
	tarPath, shaValue := createTestTarGz(t, map[string]string{"test.txt": "content"})

	tests := []struct {
		name        string
		setup       func(t *testing.T) (string, string)
		expectError string
	}{
		{
			name: "valid checksum",
			setup: func(t *testing.T) (string, string) {
				shaPath := filepath.Join(filepath.Dir(tarPath), "test.sha256")
				require.NoError(t, os.WriteFile(shaPath, []byte(shaValue), 0644))
				return tarPath, shaPath
			},
		},
		{
			name: "invalid checksum",
			setup: func(t *testing.T) (string, string) {
				shaPath := filepath.Join(filepath.Dir(tarPath), "test.sha256")
				require.NoError(t, os.WriteFile(shaPath, []byte(strings.Repeat("a", 64)), 0644))
				return tarPath, shaPath
			},
			expectError: "checksum mismatch",
		},
		{
			name: "missing sha file",
			setup: func(t *testing.T) (string, string) {
				return "nonexistent.tar.gz", "missing.sha256"
			},
			expectError: "no such file",
		},
		{
			name: "invalid sha format",
			setup: func(t *testing.T) (string, string) {
				invalidPath := filepath.Join(t.TempDir(), "invalid.sha256")
				require.NoError(t, os.WriteFile(invalidPath, []byte("invalid"), 0644))
				return "test.tar.gz", invalidPath
			},
			expectError: "invalid SHA256",
		},
		{
			name: "hash read error",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				filePath := filepath.Join(dir, "test.txt")
				shaPath := filepath.Join(dir, "test.sha256")

				require.NoError(t, os.WriteFile(filePath, []byte("test"), 0000))
				require.NoError(t, os.WriteFile(shaPath, []byte(strings.Repeat("a", 64)), 0644))
				t.Cleanup(func() { os.Chmod(filePath, 0644) })
				return filePath, shaPath
			},
			expectError: "open file failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tarPath, shaPath := tt.setup(t)
			err := verifyChecksum(tarPath, shaPath, createTestLogger())

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestExtractTarGz(t *testing.T) {
	tarPath, _ := createTestTarGz(t, map[string]string{
		"file1.txt":        "content1",
		"dir/file2.txt":    "content2",
		"nested/file3.txt": "content3",
	})

	tests := []struct {
		name        string
		setup       func(t *testing.T) (string, string)
		ctx         func() context.Context
		expectError string
	}{
		{
			name: "successful extraction",
			setup: func(t *testing.T) (string, string) {
				return tarPath, t.TempDir()
			},
		},
		{
			name: "cancelled context",
			setup: func(t *testing.T) (string, string) {
				return tarPath, t.TempDir()
			},
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			expectError: context.Canceled.Error(),
		},
		{
			name: "invalid tar format",
			setup: func(t *testing.T) (string, string) {
				path := filepath.Join(t.TempDir(), "invalid.tar.gz")
				require.NoError(t, os.WriteFile(path, []byte("invalid content"), 0644))
				return path, t.TempDir()
			},
			expectError: "gzip reader failed",
		},
		{
			name: "read-only destination",
			setup: func(t *testing.T) (string, string) {
				dest := t.TempDir()
				require.NoError(t, os.Chmod(dest, 0444))
				t.Cleanup(func() { os.Chmod(dest, 0755) })
				return tarPath, dest
			},
			expectError: "permission denied",
		},
		{
			name: "skips non-regular files",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				tarPath := filepath.Join(dir, "test.tar.gz")

				f, err := os.Create(tarPath)
				require.NoError(t, err)

				gw := gzip.NewWriter(f)
				tw := tar.NewWriter(gw)

				// Regular file
				hdr := &tar.Header{
					Name:     "regular.txt",
					Mode:     0644,
					Size:     int64(len("test content")),
					Typeflag: tar.TypeReg,
				}
				require.NoError(t, tw.WriteHeader(hdr))
				_, err = tw.Write([]byte("test content"))
				require.NoError(t, err)

				// Symlink - FIXED: Set proper size (0) for symlinks
				hdr = &tar.Header{
					Name:     "symlink.txt",
					Linkname: "regular.txt",
					Mode:     0777,
					Typeflag: tar.TypeSymlink,
					Size:     0, // Explicitly set size for symlinks
				}
				require.NoError(t, tw.WriteHeader(hdr))

				require.NoError(t, tw.Close())
				require.NoError(t, gw.Close())
				require.NoError(t, f.Close())

				return tarPath, t.TempDir()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, dest := tt.setup(t)

			ctx := context.Background()
			if tt.ctx != nil {
				ctx = tt.ctx()
			}

			err := extractTarGz(ctx, src, dest, createTestLogger())

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
				// Verify symlink was skipped
				_, err := os.Lstat(filepath.Join(dest, "symlink.txt"))
				assert.True(t, os.IsNotExist(err), "Symlink should not exist")
			}
		})
	}
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

	tests := []struct {
		name        string
		setup       func(t *testing.T) (string, string)
		expectError string
	}{
		{
			name: "successful sync",
			setup: func(t *testing.T) (string, string) {
				return setupTestDir(t)
			},
		},
		{
			name: "rollback on directory conflict",
			setup: func(t *testing.T) (string, string) {
				staging, target := setupTestDir(t)
				conflictPath := filepath.Join(target, "new.txt")
				require.NoError(t, os.Mkdir(conflictPath, 0755))
				return staging, target
			},
			expectError: "can't replace directory with file",
		},
		{
			name: "partial sync failure",
			setup: func(t *testing.T) (string, string) {
				staging := t.TempDir()
				target := t.TempDir()

				require.NoError(t, os.WriteFile(filepath.Join(target, "existing.txt"), []byte("old"), 0644))
				require.NoError(t, os.WriteFile(filepath.Join(staging, "valid.txt"), []byte("good"), 0644))
				require.NoError(t, os.WriteFile(filepath.Join(staging, "invalid.txt"), []byte("bad"), 0644))

				invalidDst := filepath.Join(target, "invalid.txt")
				require.NoError(t, os.Mkdir(invalidDst, 0755))
				return staging, target
			},
			expectError: "can't replace directory with file",
		},
		{
			name: "empty staging directory",
			setup: func(t *testing.T) (string, string) {
				return t.TempDir(), t.TempDir()
			},
		},
		{
			name: "read-only target directory",
			setup: func(t *testing.T) (string, string) {
				staging := t.TempDir()
				target := t.TempDir()

				require.NoError(t, os.WriteFile(filepath.Join(staging, "test.txt"), []byte("content"), 0644))
				require.NoError(t, os.Chmod(target, 0444))
				t.Cleanup(func() { os.Chmod(target, 0755) })
				return staging, target
			},
			expectError: "permission denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staging, target := tt.setup(t)
			err := atomicSync(context.Background(), staging, target, createTestLogger())

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCleanup(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T) *cleanup
		expectError bool
	}{
		{
			name: "removes files and dirs",
			setup: func(t *testing.T) *cleanup {
				cl := &cleanup{}
				file1 := filepath.Join(t.TempDir(), "test1.txt")
				require.NoError(t, os.WriteFile(file1, []byte("test"), 0644))
				cl.addFile(file1)
				cl.tempDir = t.TempDir()
				return cl
			},
		},
		{
			name: "handles non-existent files",
			setup: func(t *testing.T) *cleanup {
				cl := &cleanup{}
				cl.addFile("/non/existent/file")
				return cl
			},
		},
		{
			name: "handles write-protected dir",
			setup: func(t *testing.T) *cleanup {
				tempDir := t.TempDir()
				require.NoError(t, os.Chmod(tempDir, 0444))
				t.Cleanup(func() { os.Chmod(tempDir, 0755) })
				return &cleanup{tempDir: tempDir}
			},
			expectError: true,
		},
		{
			name: "concurrent cleanup",
			setup: func(t *testing.T) *cleanup {
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
				return cl
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := tt.setup(t)
			cl.execute(createTestLogger())

			if !tt.expectError {
				for _, f := range cl.files {
					_, err := os.Stat(f)
					assert.True(t, os.IsNotExist(err))
				}
				if cl.tempDir != "" {
					_, err := os.Stat(cl.tempDir)
					assert.True(t, os.IsNotExist(err))
				}
			}
		})
	}
}

func TestRunIntegration(t *testing.T) {
	files := map[string]string{
		"nested/plugin": "binary content",
	}

	var tarBuf bytes.Buffer
	gw := gzip.NewWriter(&tarBuf)
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

	tests := []struct {
		name        string
		setup       func(t *testing.T)
		expectError string
	}{
		{
			name: "successful run",
			setup: func(t *testing.T) {
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, ".tgz"):
						w.Write(tarData)
					case strings.HasSuffix(r.URL.Path, ".sha256"):
						w.Write([]byte(shaValue))
					}
				}))
				t.Cleanup(func() { ts.Close() })

				t.Setenv("CNI_PLUGINS_VERSION", "v1.0.0")
				tempDir := t.TempDir()
				t.Setenv("TMPDIR", tempDir)

				oldTargetDir := targetDir
				targetDir = filepath.Join(tempDir, "host/opt/cni/bin")
				t.Cleanup(func() { targetDir = oldTargetDir })

				oldBaseURL := baseURL
				baseURL = ts.URL
				t.Cleanup(func() { baseURL = oldBaseURL })
			},
		},
		{
			name: "missing version",
			setup: func(t *testing.T) {
				t.Setenv("CNI_PLUGINS_VERSION", "")
			},
			expectError: "CNI_PLUGINS_VERSION",
		},
		{
			name: "download failure",
			setup: func(t *testing.T) {
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				}))
				t.Cleanup(func() { ts.Close() })

				t.Setenv("CNI_PLUGINS_VERSION", "v1.0.0")
				oldBaseURL := baseURL
				baseURL = ts.URL
				t.Cleanup(func() { baseURL = oldBaseURL })
			},
			expectError: "download failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}

			logger := createTestLogger()
			err := run(context.Background(), logger)

			if tt.expectError != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
				assert.FileExists(t, filepath.Join(targetDir, "plugin"))
				assert.NoDirExists(t, filepath.Join(targetDir, "nested"))
			}
		})
	}
}

func TestSameChecksum(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T) (string, string)
		expected bool
		wantErr  bool
	}{
		{
			name: "identical files",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				file1 := filepath.Join(dir, "file1")
				file2 := filepath.Join(dir, "file2")
				require.NoError(t, os.WriteFile(file1, []byte("same"), 0644))
				require.NoError(t, os.WriteFile(file2, []byte("same"), 0644))
				return file1, file2
			},
			expected: true,
		},
		{
			name: "different files",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				file1 := filepath.Join(dir, "file1")
				file2 := filepath.Join(dir, "file2")
				require.NoError(t, os.WriteFile(file1, []byte("same"), 0644))
				require.NoError(t, os.WriteFile(file2, []byte("different"), 0644))
				return file1, file2
			},
			expected: false,
		},
		{
			name: "non-existent files",
			setup: func(t *testing.T) (string, string) {
				return "/nonexistent1", "/nonexistent2"
			},
			wantErr: true,
		},
		{
			name: "directory instead of file",
			setup: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				file := filepath.Join(dir, "file")
				require.NoError(t, os.WriteFile(file, []byte("test"), 0644))
				return dir, file
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file1, file2 := tt.setup(t)
			same, err := sameChecksum(file1, file2)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, same)
			}
		})
	}
}

func TestFileChecksum(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		wantErr bool
	}{
		{
			name: "successful checksum",
			setup: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "test.txt")
				require.NoError(t, os.WriteFile(path, []byte("content"), 0644))
				return path
			},
		},
		{
			name: "non-existent file",
			setup: func(t *testing.T) string {
				return "/nonexistent"
			},
			wantErr: true,
		},
		{
			name: "directory instead of file",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup(t)
			_, err := fileChecksum(path)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestWriteFile(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T) (string, io.Reader)
		expectError string
	}{
		{
			name: "successful write",
			setup: func(t *testing.T) (string, io.Reader) {
				return filepath.Join(t.TempDir(), "test.txt"), strings.NewReader("content")
			},
		},
		{
			name: "parent dir creation failure",
			setup: func(t *testing.T) (string, io.Reader) {
				dir := t.TempDir()
				require.NoError(t, os.Chmod(dir, 0444))
				t.Cleanup(func() { os.Chmod(dir, 0755) })
				return filepath.Join(dir, "subdir", "file.txt"), strings.NewReader("test")
			},
			expectError: "failed to create parent directory",
		},
		{
			name: "temp file write failure",
			setup: func(t *testing.T) (string, io.Reader) {
				return filepath.Join(t.TempDir(), "test.txt"), &failingReader{}
			},
			expectError: "copy failed: simulated read failure",
		},
		{
			name: "rename failure",
			setup: func(t *testing.T) (string, io.Reader) {
				dir := t.TempDir()
				path := filepath.Join(dir, "test.txt")
				// Create a directory at the target path
				require.NoError(t, os.Mkdir(path, 0755))
				return path, strings.NewReader("test")
			},
			expectError: "atomic rename failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, reader := tt.setup(t)
			err := writeFile(path, reader, 0644, createTestLogger())

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError, 
					"Error message should contain %q, got: %v", tt.expectError, err)
			} else {
				assert.NoError(t, err)
				assert.FileExists(t, path)
			}
		})
	}
}

type failingReader struct{}

func (r *failingReader) Read(p []byte) (n int, err error) {
    return 0, errors.New("simulated read failure") 
    // ^ Always fails with this error
}

func TestSignalHandling(t *testing.T) {
	t.Run("handles SIGTERM signal", func(t *testing.T) {
		// Setup test environment
		t.Setenv("CNI_PLUGINS_VERSION", "v1.0.0")
		tempDir := t.TempDir()
		t.Setenv("TMPDIR", tempDir)

		// Create a test server that will hang
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
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

		// Verify we get an error
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

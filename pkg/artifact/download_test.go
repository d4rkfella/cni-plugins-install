package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/constants"
	"github.com/darkfella/cni-plugins-install/internal/logging"
	httpClient "github.com/darkfella/cni-plugins-install/pkg/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test Setup ---

const testVersion = "v1.5.0"
const testArchiveContent = "dummy archive content"
const testShaContentFormat = "%s  cni-plugins-linux-amd64-%s.tgz\n" // Format from release files

func calculateSHA256(content string) string {
	h := sha256.New()
	h.Write([]byte(content))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func createTestServer(t *testing.T, version, archiveContent string) *httptest.Server {
	archiveHash := calculateSHA256(archiveContent)
	shaContent := fmt.Sprintf(testShaContentFormat, archiveHash, version)

	mux := http.NewServeMux()

	// Archive handler
	archivePath := fmt.Sprintf("/%s/cni-plugins-linux-amd64-%s.tgz", version, version)
	mux.HandleFunc(archivePath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(archiveContent))
	})

	// SHA handler
	shaPath := archivePath + ".sha256"
	mux.HandleFunc(shaPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(shaContent))
	})

	// Default handler for unexpected paths
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Unexpected request path: %s", r.URL.Path)
		http.NotFound(w, r)
	})

	return httptest.NewServer(mux)
}

func defaultDownloaderConfig(baseURL string) *Config {
	return &Config{
		BaseURL:         baseURL,
		DownloadTimeout: 30 * time.Second,
		MaxRetries:      3,
		BufferSize:      constants.DefaultBufferSize,
	}
}

// --- Tests ---

func TestNewDownloader(t *testing.T) {
	logger := logging.NewLogger()
	server := createTestServer(t, testVersion, testArchiveContent)
	defer server.Close()
	config := defaultDownloaderConfig(server.URL)

	t.Run("With provided HTTP client", func(t *testing.T) {
		client := httpClient.NewClient(logger, server.Client(), &httpClient.Config{
			DownloadTimeout: 10 * time.Second, // Different timeout
			MaxRetries:      5,
			BufferSize:      1024,
		})
		d := NewDownloader(logger, client, config)
		require.NotNil(t, d)
		assert.Equal(t, logger, d.logger)
		assert.Equal(t, client, d.httpClient, "Should use provided client")
		assert.NotNil(t, d.fileSystem)
		assert.NotNil(t, d.cleanup)
		assert.Equal(t, config, d.config)
		assert.NotNil(t, d.extractor)
		assert.NotNil(t, d.validator)
	})

	t.Run("With nil HTTP client", func(t *testing.T) {
		d := NewDownloader(logger, nil, config) // Pass nil client
		require.NotNil(t, d)
		assert.Equal(t, logger, d.logger)
		assert.NotNil(t, d.httpClient, "Should create default client")
		// Check if default client uses config values (cannot directly check internal http.Client timeout)
		// We can infer by checking config propagation if possible, or just check non-nil
		assert.NotNil(t, d.fileSystem)
		assert.NotNil(t, d.cleanup)
		assert.Equal(t, config, d.config)
		assert.NotNil(t, d.extractor)
		assert.NotNil(t, d.validator)
	})
}

func TestDownloadAndExtract(t *testing.T) {
	// Define dummy files to be included in the test archive
	dummyFiles := map[string]string{
		"bridge":     "bridge-content",
		"loopback":   "loopback-content",
		"host-local": "host-local-content",
	}
	// Create a realistic in-memory tar.gz archive and its sha256
	archiveBytes, shaBytes, err := createDummyArchiveAndSha(t, dummyFiles)
	require.NoError(t, err)

	tests := []struct {
		name        string
		version     string
		mockHandler http.HandlerFunc // To customize server behavior per test
		setupFunc   func(d *Downloader, targetDir string) error
		wantErr     bool
		wantErrMsg  string
	}{
		{
			name:    "Successful download and extract",
			version: testVersion,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				archivePath := fmt.Sprintf("/%s/cni-plugins-linux-amd64-%s.tgz", testVersion, testVersion)
				shaPath := archivePath + ".sha256"
				switch r.URL.Path {
				case archivePath:
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(archiveBytes)
				case shaPath:
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(shaBytes)
				default:
					t.Errorf("Unexpected request path: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			},
			wantErr: false,
		},
		// --- Add Error Cases Here ---
		{
			name:    "Invalid version format",
			version: "invalid-version", // Does not start with 'v'
			mockHandler: func(w http.ResponseWriter, r *http.Request) { // Handler not called
				t.Error("Server should not be called for invalid version")
				http.Error(w, "Should not be called", http.StatusInternalServerError)
			},
			wantErr:    true,
			wantErrMsg: "validate version failed",
		},
		{
			name:    "Target directory is a file",
			version: testVersion,
			setupFunc: func(d *Downloader, targetDir string) error {
				// Remove the auto-created targetDir and replace with a file
				if err := os.RemoveAll(targetDir); err != nil {
					return err
				}
				return os.WriteFile(targetDir, []byte("i am a file"), 0644)
			},
			mockHandler: func(w http.ResponseWriter, r *http.Request) { // Handler not called
				t.Error("Server should not be called for invalid target dir")
				http.Error(w, "Should not be called", http.StatusInternalServerError)
			},
			wantErr:    true,
			wantErrMsg: "validate directory failed: not a directory",
		},
		{
			name:    "Target directory not writable (staging creation fails)",
			version: testVersion,
			setupFunc: func(d *Downloader, targetDir string) error {
				// Make target directory read-only
				return os.Chmod(targetDir, 0555)
			},
			mockHandler: func(w http.ResponseWriter, r *http.Request) { // Handler not called
				t.Error("Server should not be called when staging creation fails")
				http.Error(w, "Should not be called", http.StatusInternalServerError)
			},
			wantErr:    true,
			wantErrMsg: "validate directory failed: directory is not writable",
		},
		{
			name:    "Archive download fails (404)",
			version: testVersion,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				archivePath := fmt.Sprintf("/%s/cni-plugins-linux-amd64-%s.tgz", testVersion, testVersion)
				shaPath := archivePath + ".sha256"
				switch r.URL.Path {
				case archivePath:
					http.NotFound(w, r) // Archive not found
				case shaPath:
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(shaBytes)
				default:
					http.NotFound(w, r)
				}
			},
			wantErr:    true,
			wantErrMsg: "download failed",
		},
		{
			name:    "SHA download fails (500)",
			version: testVersion,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				archivePath := fmt.Sprintf("/%s/cni-plugins-linux-amd64-%s.tgz", testVersion, testVersion)
				shaPath := archivePath + ".sha256"
				switch r.URL.Path {
				case archivePath:
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(archiveBytes)
				case shaPath:
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				default:
					http.NotFound(w, r)
				}
			},
			wantErr:    true,
			wantErrMsg: "download failed",
		},
		{
			name:    "Checksum mismatch",
			version: testVersion,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				archivePath := fmt.Sprintf("/%s/cni-plugins-linux-amd64-%s.tgz", testVersion, testVersion)
				shaPath := archivePath + ".sha256"
				switch r.URL.Path {
				case archivePath:
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(archiveBytes) // Correct archive
				case shaPath:
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("incorrectsha  somefile.tgz\n")) // Incorrect SHA content
				default:
					http.NotFound(w, r)
				}
			},
			wantErr:    true,
			wantErrMsg: "checksum verification failed",
		},
		{
			name:    "Extraction fails (corrupt archive)",
			version: testVersion,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				archivePath := fmt.Sprintf("/%s/cni-plugins-linux-amd64-%s.tgz", testVersion, testVersion)
				shaPath := archivePath + ".sha256"
				switch r.URL.Path {
				case archivePath:
					// Calculate correct hash for the *corrupt* data we send
					corruptData := []byte("this is not a valid tgz")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write(corruptData)
				case shaPath:
					// Send the hash matching the corrupt data so checksum passes
					corruptData := []byte("this is not a valid tgz")
					correctHashForCorrupt := calculateSHA256(string(corruptData))
					shaContent := fmt.Sprintf(testShaContentFormat, correctHashForCorrupt, testVersion)
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(shaContent))
				default:
					http.NotFound(w, r)
				}
			},
			wantErr:    true,
			wantErrMsg: "archive extraction failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _, targetDir, cleanup := setupDownloaderTest(t)
			defer cleanup()

			// Set up the specific mock handler for this test
			mockServer := httptest.NewServer(tt.mockHandler)
			d.config.BaseURL = mockServer.URL // Update downloader config with mock server URL
			// Re-create or update the http client inside downloader to use the new mock server
			d.httpClient = httpClient.NewClient(d.logger, mockServer.Client(), &httpClient.Config{
				DownloadTimeout: d.config.DownloadTimeout,
				MaxRetries:      d.config.MaxRetries,
				BufferSize:      d.config.BufferSize,
			})

			// Run test-specific setup
			if tt.setupFunc != nil {
				err := tt.setupFunc(d, targetDir)
				require.NoError(t, err, "test setup failed")
			}

			// --- Execute ---
			err := d.DownloadAndExtract(context.Background(), tt.version, targetDir)

			// --- Verify Error ---
			if tt.wantErr {
				assert.Error(t, err)
				if tt.wantErrMsg != "" {
					assert.Contains(t, err.Error(), tt.wantErrMsg)
				}
				// Ensure staging directory is cleaned up even on failure
				assert.NoError(t, d.Cleanup(), "Cleanup should not fail even if main operation did")
				stgDir := d.StagingDir()
				if stgDir != "" { // Only check if staging dir was attempted
					_, errStat := os.Stat(stgDir)
					assert.True(t, os.IsNotExist(errStat), "Staging directory should be cleaned up on error")
				}
				return // Don't check success conditions
			}

			// --- Verify Success ---
			require.NoError(t, err)

			// Verify staging directory and downloads dir were created
			stagingDir := d.StagingDir()
			require.NotEmpty(t, stagingDir)
			downloadsDir := filepath.Join(stagingDir, "downloads")
			_, err = os.Stat(stagingDir)
			require.NoError(t, err)
			_, err = os.Stat(downloadsDir)
			require.NoError(t, err)

			// Verify archive and sha file downloaded
			archiveName := fmt.Sprintf("cni-plugins-linux-amd64-%s.tgz", tt.version)
			shaName := archiveName + ".sha256"
			_, err = os.Stat(filepath.Join(downloadsDir, archiveName))
			require.NoError(t, err)
			_, err = os.Stat(filepath.Join(downloadsDir, shaName))
			require.NoError(t, err)

			// Verify files were extracted to staging dir (check one file)
			extractedFilePath := filepath.Join(stagingDir, "bridge")
			_, err = os.Stat(extractedFilePath)
			assert.NoError(t, err, "Expected extracted file 'bridge' not found in staging dir")
			content, err := os.ReadFile(extractedFilePath)
			assert.NoError(t, err)
			assert.Equal(t, dummyFiles["bridge"], string(content))

			// Verify cleanup removes staging directory
			err = d.Cleanup()
			require.NoError(t, err)
			_, err = os.Stat(stagingDir)
			assert.True(t, os.IsNotExist(err), "Staging directory should be removed after cleanup")
		})
	}
}

// setupDownloaderTest creates temp directories and a Downloader with a mock server
func setupDownloaderTest(t *testing.T) (d *Downloader, server *httptest.Server, targetDir string, cleanupFunc func()) {
	t.Helper()
	logger := logging.NewLogger()

	// Mock Server setup will be done in specific tests
	server = nil // Placeholder

	// Target Directory for downloads/staging
	targetDir, err := os.MkdirTemp("", "artifact-test-tgt-*")
	require.NoError(t, err)

	// Config (mock server URL will be set later)
	cfg := &Config{
		BaseURL:    "placeholder",
		MaxRetries: 2,    // Explicitly set MaxRetries for testing
		BufferSize: 1024, // Explicitly set BufferSize for testing
	}

	// Use real dependencies for now
	d = NewDownloader(logger, nil, cfg)

	cleanupFunc = func() {
		if server != nil {
			server.Close()
		}
		// Execute downloader cleanup first to remove staging dirs etc.
		if d != nil {
			_ = d.Cleanup()
		}
		if targetDir != "" {
			_ = os.RemoveAll(targetDir)
		}
	}

	return d, server, targetDir, cleanupFunc
}

// Helper to create dummy archive and sha files for mock server
func createDummyArchiveAndSha(t *testing.T, files map[string]string) (archiveBytes []byte, shaBytes []byte, err error) {
	t.Helper()
	// Create tar.gz in memory
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for name, content := range files {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0644,
			Size:    int64(len(content)),
			ModTime: time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, nil, fmt.Errorf("write tar header: %w", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			return nil, nil, fmt.Errorf("write tar content: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, nil, fmt.Errorf("close tar writer: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, nil, fmt.Errorf("close gzip writer: %w", err)
	}
	archiveBytes = buf.Bytes()

	// Calculate SHA256
	hash := sha256.Sum256(archiveBytes)
	shaContent := fmt.Sprintf("%x  dummy_filename.tar.gz\n", hash) // Mimic sha256sum output format
	shaBytes = []byte(shaContent)

	return archiveBytes, shaBytes, nil
}

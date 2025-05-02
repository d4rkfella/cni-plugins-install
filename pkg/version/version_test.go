package version

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/stretchr/testify/require"
)

// Tests the constructor for the version Manager.
func TestNewManager(t *testing.T) {
	logger := logging.NewLogger()
	plugins := map[string]bool{"plugin1": true}

	m := NewManager(logger, plugins)
	if m == nil {
		t.Error("NewManager() returned nil")
		return
	}
	if m.logger != logger {
		t.Error("NewManager() did not set logger correctly")
	}
	if len(m.plugins) != len(plugins) {
		t.Error("NewManager() did not set plugins correctly")
	}
}

// Tests the SaveVersion function, covering success and error cases
// like unreadable directories or files.
func TestSaveVersion(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "version-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Logf("Error cleaning up temp dir %s: %v", tempDir, err)
		}
	}()

	// Create test plugin files
	plugin1 := filepath.Join(tempDir, "plugin1")
	if err := os.WriteFile(plugin1, []byte("test plugin 1"), 0755); err != nil {
		t.Fatalf("Failed to create test plugin: %v", err)
	}

	plugin2 := filepath.Join(tempDir, "plugin2")
	if err := os.WriteFile(plugin2, []byte("test plugin 2"), 0755); err != nil {
		t.Fatalf("Failed to create test plugin: %v", err)
	}

	// Create a directory to test directory skipping
	if err := os.Mkdir(filepath.Join(tempDir, "plugin3"), 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	// Create a directory that SaveVersion cannot list (for error testing)
	unreadableDir := filepath.Join(tempDir, "unreadable")
	if err := os.Mkdir(unreadableDir, 0000); err != nil {
		t.Fatalf("Failed to create unreadable test directory: %v", err)
	}
	// Create a managed plugin file that is unreadable for checksum error testing
	unreadablePlugin := filepath.Join(tempDir, "unreadable_plugin")
	if err := os.WriteFile(unreadablePlugin, []byte("unreadable"), 0755); err != nil { // Write first
		t.Fatalf("Failed to create unreadable plugin file: %v", err)
	}
	if err := os.Chmod(unreadablePlugin, 0000); err != nil { // Then make unreadable
		t.Fatalf("Failed to make plugin file unreadable: %v", err)
	}

	tests := []struct {
		name      string
		plugins   map[string]bool
		version   string
		targetDir string // Add targetDir to test cases
		wantErr   bool
	}{
		{
			name:      "save single plugin version",
			plugins:   map[string]bool{"plugin1": true},
			version:   "v1.0.0",
			targetDir: tempDir,
			wantErr:   false,
		},
		{
			name:      "save multiple plugin versions",
			plugins:   map[string]bool{"plugin1": true, "plugin2": true},
			version:   "v1.1.0",
			targetDir: tempDir,
			wantErr:   false,
		},
		{
			name:      "save with non-existent plugin",
			plugins:   map[string]bool{"nonexistent": true},
			version:   "v1.0.0",
			targetDir: tempDir,
			wantErr:   false,
		},
		{
			name:      "save in unreadable directory",
			plugins:   map[string]bool{"plugin1": true},
			version:   "v1.2.0",
			targetDir: unreadableDir,
			wantErr:   true, // Expect error when listing dir
		},
		{
			name:      "save with unreadable plugin file",
			plugins:   map[string]bool{"plugin1": true, "unreadable_plugin": true},
			version:   "v1.3.0",
			targetDir: tempDir,
			wantErr:   true, // Expect error when calculating checksum
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManager(logging.NewLogger(), tt.plugins)
			err := m.SaveVersion(context.Background(), tt.targetDir, tt.version)
			if (err != nil) != tt.wantErr {
				t.Errorf("SaveVersion() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// Verify version file was created
				versionFile := filepath.Join(tt.targetDir, ".version")
				data, err := os.ReadFile(versionFile)
				if err != nil {
					t.Errorf("Failed to read version file: %v", err)
					return
				}

				var info Info
				if err := json.Unmarshal(data, &info); err != nil {
					t.Errorf("Failed to parse version file: %v", err)
					return
				}

				if info.Version != tt.version {
					t.Errorf("SaveVersion() saved version = %v, want %v", info.Version, tt.version)
				}

				// Verify only managed plugins are included
				for plugin := range info.Plugins {
					if !tt.plugins[plugin] {
						t.Errorf("SaveVersion() included unmanaged plugin: %v", plugin)
					}
				}
			}
		})
	}
	// Restore permissions so cleanup works
	require.NoError(t, os.Chmod(unreadableDir, 0755))
	require.NoError(t, os.Chmod(unreadablePlugin, 0755))
}

// Tests the CheckVersion function for checking if a given version string
// matches the one stored in the .version file.
func TestCheckVersion(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "version-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Logf("Error cleaning up temp dir %s: %v", tempDir, err)
		}
	}()

	// Create a test version file
	info := Info{
		Version:     "v1.0.0",
		InstalledAt: time.Now(),
		Plugins:     map[string]string{"plugin1": "hash1"},
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Failed to marshal version info: %v", err)
	}
	versionFile := filepath.Join(tempDir, ".version")
	if err := os.WriteFile(versionFile, data, 0644); err != nil {
		t.Fatalf("Failed to write version file: %v", err)
	}

	tests := []struct {
		name      string
		version   string
		wantMatch bool
		wantErr   bool
		setupFunc func() error
	}{
		{
			name:      "matching version",
			version:   "v1.0.0",
			wantMatch: true,
			wantErr:   false,
		},
		{
			name:      "different version",
			version:   "v1.1.0",
			wantMatch: false,
			wantErr:   false,
		},
		{
			name:      "no version file",
			version:   "v1.0.0",
			wantMatch: false,
			wantErr:   false,
			setupFunc: func() error {
				return os.Remove(versionFile)
			},
		},
		{
			name:      "invalid version file",
			version:   "v1.0.0",
			wantMatch: false,
			wantErr:   true,
			setupFunc: func() error {
				return os.WriteFile(versionFile, []byte("invalid json"), 0644)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setupFunc != nil {
				if err := tt.setupFunc(); err != nil {
					t.Fatalf("Setup failed: %v", err)
				}
			}

			m := NewManager(logging.NewLogger(), map[string]bool{"plugin1": true})
			match, err := m.CheckVersion(context.Background(), tempDir, tt.version)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckVersion() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if match != tt.wantMatch {
				t.Errorf("CheckVersion() = %v, want %v", match, tt.wantMatch)
			}
		})
	}
}

// calculateSHA256 computes the SHA256 hash of byte data.
func calculateSHA256(data []byte) string {
	hash := sha256.New()
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

// Helper function to create a version file for tests
func createVersionFile(t *testing.T, targetDir string, version string, plugins map[string]string) string {
	t.Helper()
	info := Info{
		Version:     version,
		InstalledAt: time.Now(),
		Plugins:     plugins,
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Failed to marshal version info: %v", err)
	}
	versionFile := filepath.Join(targetDir, ".version")
	if err := os.WriteFile(versionFile, data, 0644); err != nil {
		t.Fatalf("Failed to write version file: %v", err)
	}
	return versionFile
}

// Tests the VerifyPlugins function, checking for existence, permissions,
// checksums (if .version exists), and handling of directories vs files.
func TestVerifyPlugins(t *testing.T) {
	// Create a temporary directory for testing
	tempDir, err := os.MkdirTemp("", "version-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Logf("Error cleaning up temp dir %s: %v", tempDir, err)
		}
	}()

	// --- Default Setup (run before each subtest unless overridden) ---
	setupDefault := func(dir string) {
		// Create test plugin files
		plugin1Content := []byte("test plugin 1")
		plugin1 := filepath.Join(dir, "plugin1")
		if err := os.WriteFile(plugin1, plugin1Content, 0755); err != nil {
			t.Fatalf("Failed to create test plugin1: %v", err)
		}

		plugin2 := filepath.Join(dir, "plugin2")
		if err := os.WriteFile(plugin2, []byte("test plugin 2"), 0644); err != nil { // Non-executable
			t.Fatalf("Failed to create test plugin2: %v", err)
		}
	}

	// Calculate actual plugin1 checksum
	plugin1Hash := calculateSHA256([]byte("test plugin 1"))

	// --- Test Cases ---
	tests := []struct {
		name        string
		plugins     map[string]bool // Plugins managed by the Manager for this test
		wantValid   bool
		wantErr     bool
		setupFunc   func(string) // Function to setup specific state (runs AFTER default setup)
		cleanupFunc func(string) // Function to cleanup specific state
	}{
		{
			name:    "all plugins valid and executable",
			plugins: map[string]bool{"plugin1": true}, // Only manage plugin1 here
			setupFunc: func(dir string) {
				// Ensure plugin1 is executable (already is by default setup)
				// Create a version file with correct hash
				createVersionFile(t, dir, "v1.0.0", map[string]string{"plugin1": plugin1Hash})
			},
			wantValid: true,
			wantErr:   false,
		},
		{
			name:      "plugin not executable",
			plugins:   map[string]bool{"plugin2": true}, // Only manage plugin2
			wantValid: false,
			wantErr:   false,
			setupFunc: func(dir string) {
				// plugin2 created non-executable in default setup
				// No version file needed, should fail on executable check
			},
		},
		{
			name:      "managed plugin file missing",
			plugins:   map[string]bool{"nonexistent": true},
			wantValid: false,
			wantErr:   false,
			setupFunc: func(dir string) {
				// No setup needed, plugin does not exist
			},
		},
		{
			name:    "plugin checksum mismatch",
			plugins: map[string]bool{"plugin1": true},
			setupFunc: func(dir string) {
				createVersionFile(t, dir, "v1.0.0", map[string]string{"plugin1": "incorrecthash"})
			},
			wantValid: false,
			wantErr:   false,
		},
		{
			name:    "plugin missing from version file",
			plugins: map[string]bool{"plugin1": true, "plugin_not_in_version": true},
			setupFunc: func(dir string) {
				// Create the extra plugin file
				pluginNotInVersion := filepath.Join(dir, "plugin_not_in_version")
				if err := os.WriteFile(pluginNotInVersion, []byte("test extra"), 0755); err != nil {
					t.Fatalf("Failed to create extra plugin: %v", err)
				}
				// Create version file *without* the extra plugin
				createVersionFile(t, dir, "v1.0.0", map[string]string{"plugin1": plugin1Hash})
			},
			wantValid: false, // Fails because plugin_not_in_version is managed but missing from .version
			wantErr:   false,
		},
		{
			name:    "plugin file exists but .version file does not",
			plugins: map[string]bool{"plugin1": true},
			setupFunc: func(dir string) {
				// Ensure plugin1 exists and is executable (done in default setup)
				// No version file created here
			},
			wantValid: true, // Should be valid as long as executable
			wantErr:   false,
		},
		{
			name:    "managed plugin path is a directory",
			plugins: map[string]bool{"plugin_as_dir": true}, // ONLY manage the directory plugin
			setupFunc: func(dir string) {
				// Create a directory where a managed plugin is expected
				pluginAsDir := filepath.Join(dir, "plugin_as_dir")
				if err := os.Mkdir(pluginAsDir, 0755); err != nil {
					t.Fatalf("Failed to create plugin as directory: %v", err)
				}
				// Create version file including the directory name (checksum irrelevant)
				// Even though it's skipped, if checksums ARE verified, it must be present.
				createVersionFile(t, dir, "v1.0.0", map[string]string{
					"plugin_as_dir": "dir_hash",
				})
			},
			// wantValid depends on whether IsDirectory check happens before or after missing file check
			// Currently IsDirectory is checked *after* FileExists. Since the directory exists,
			// IsDirectory is true, loop continues, returns true.
			wantValid: true, // Directory should be skipped
			wantErr:   false,
		},
		{
			name:    "IsExecutable check fails (permissions)",
			plugins: map[string]bool{"unstatable_plugin": true}, // ONLY manage this plugin
			setupFunc: func(dir string) {
				// Create a plugin file
				unstatablePlugin := filepath.Join(dir, "unstatable_plugin")
				if err := os.WriteFile(unstatablePlugin, []byte("test unstatable"), 0755); err != nil {
					t.Fatalf("Failed to create unstatable plugin: %v", err)
				}
				// Remove read/execute permission from PARENT directory to cause Stat to fail
				if err := os.Chmod(dir, 0200); err != nil { // write-only for owner
					t.Fatalf("Failed to chmod parent dir: %v", err)
				}
				// IMPORTANT: Ensure NO .version file exists for this test
				_ = os.Remove(filepath.Join(dir, ".version"))
			},
			cleanupFunc: func(dir string) {
				_ = os.Chmod(dir, 0755) // Restore parent dir permissions
			},
			wantValid: false,
			wantErr:   true, // Expect error from IsExecutable -> Stat
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset temp dir state for each test
			require.NoError(t, os.RemoveAll(tempDir), "Failed to remove temp dir before test")
			if err := os.Mkdir(tempDir, 0755); err != nil {
				t.Fatalf("Failed to recreate temp dir: %v", err)
			}
			setupDefault(tempDir)

			// Run test-specific setup
			if tt.setupFunc != nil {
				tt.setupFunc(tempDir)
			}
			// Ensure cleanup runs if setup was performed
			if tt.cleanupFunc != nil {
				t.Cleanup(func() { tt.cleanupFunc(tempDir) })
			}

			m := NewManager(logging.NewLogger(), tt.plugins) // Use plugins specific to test case
			valid, err := m.VerifyPlugins(context.Background(), tempDir)

			if (err != nil) != tt.wantErr {
				t.Errorf("VerifyPlugins() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if valid != tt.wantValid {
				t.Errorf("VerifyPlugins() = %v, want %v", valid, tt.wantValid)
			}
		})
	}
}

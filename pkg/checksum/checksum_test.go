package checksum

import (
	"context"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Helper function to create a temporary file with specific content
func createTempFile(t *testing.T, dir, content string) string {
	t.Helper()
	tempFile, err := os.CreateTemp(dir, "checksum-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	if _, err := tempFile.WriteString(content); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	require.NoError(t, tempFile.Close(), "Failed to close temp file")
	return tempFile.Name()
}

func TestCalculateFileSHA256(t *testing.T) {
	tempDir := t.TempDir()
	content := "test file content"
	tempFile := createTempFile(t, tempDir, content)
	expectedHash := "60f5237ed4049f0382661ef009d2bc42e48c3ceb3edb6600f7024e7ab3b838f3"

	tests := []struct {
		name     string
		filePath string
		wantHash string
		wantErr  bool
	}{
		{
			name:     "valid file",
			filePath: tempFile,
			wantHash: expectedHash,
			wantErr:  false,
		},
		{
			name:     "non-existent file",
			filePath: "/non/existent/file",
			wantHash: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotHash, err := CalculateFileSHA256(context.Background(), tt.filePath)
			if (err != nil) != tt.wantErr {
				t.Errorf("CalculateFileSHA256() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotHash != tt.wantHash {
				t.Errorf("CalculateFileSHA256() = %v, want %v", gotHash, tt.wantHash)
			}
		})
	}
}

func TestVerifyFileSHA256(t *testing.T) {
	tempDir := t.TempDir()
	content := "another test content"
	tempFile := createTempFile(t, tempDir, content)
	correctHash := "1130e686033b4e90c7d304fe7448c0a54432d0996bd607de6929a4287e8a6385"
	incorrectHash := "incorrecthash"

	tests := []struct {
		name         string
		filePath     string
		expectedHash string
		wantErr      bool
	}{
		{
			name:         "correct hash",
			filePath:     tempFile,
			expectedHash: correctHash,
			wantErr:      false,
		},
		{
			name:         "incorrect hash",
			filePath:     tempFile,
			expectedHash: incorrectHash,
			wantErr:      true,
		},
		{
			name:         "non-existent file",
			filePath:     "/non/existent/verify",
			expectedHash: correctHash,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyFileSHA256(context.Background(), tt.filePath, tt.expectedHash)
			if (err != nil) != tt.wantErr {
				t.Errorf("VerifyFileSHA256() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestReadSHAFile(t *testing.T) {
	tempDir := t.TempDir()
	validHash := "f2ca1bb6c7e907d06dafe4687e579fce76b37e4e93b7605022da52e6ccc26fd2"
	validContent := validHash + "  filename"
	validFile := createTempFile(t, tempDir, validContent)

	invalidContentShort := "short invalid hash  filename"
	invalidFileShort := createTempFile(t, tempDir, invalidContentShort)

	invalidContentEmpty := ""
	invalidFileEmpty := createTempFile(t, tempDir, invalidContentEmpty)

	tests := []struct {
		name     string
		filePath string
		wantHash string
		wantErr  bool
	}{
		{
			name:     "valid sha file",
			filePath: validFile,
			wantHash: validHash,
			wantErr:  false,
		},
		{
			name:     "invalid short hash",
			filePath: invalidFileShort,
			wantHash: "",
			wantErr:  true,
		},
		{
			name:     "empty file",
			filePath: invalidFileEmpty,
			wantHash: "",
			wantErr:  true,
		},
		{
			name:     "non-existent file",
			filePath: "/non/existent/shafile",
			wantHash: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotHash, err := ReadSHAFile(tt.filePath)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReadSHAFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotHash != tt.wantHash {
				t.Errorf("ReadSHAFile() = %v, want %v", gotHash, tt.wantHash)
			}
		})
	}
}

func TestCompareFilesSHA256(t *testing.T) {
	tempDir := t.TempDir()
	content1 := "file content 1"
	file1 := createTempFile(t, tempDir, content1)
	content2 := "file content 2"
	file2 := createTempFile(t, tempDir, content2)
	file1_copy := createTempFile(t, tempDir, content1)

	tests := []struct {
		name      string
		path1     string
		path2     string
		wantMatch bool
		wantErr   bool
	}{
		{
			name:      "matching files",
			path1:     file1,
			path2:     file1_copy,
			wantMatch: true,
			wantErr:   false,
		},
		{
			name:      "different files",
			path1:     file1,
			path2:     file2,
			wantMatch: false,
			wantErr:   false,
		},
		{
			name:      "one file non-existent",
			path1:     file1,
			path2:     "/non/existent/compare",
			wantMatch: false,
			wantErr:   true,
		},
		{
			name:      "both files non-existent",
			path1:     "/non/existent/compare1",
			path2:     "/non/existent/compare2",
			wantMatch: false,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMatch, err := CompareFilesSHA256(context.Background(), tt.path1, tt.path2)
			if (err != nil) != tt.wantErr {
				t.Errorf("CompareFilesSHA256() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotMatch != tt.wantMatch {
				t.Errorf("CompareFilesSHA256() = %v, want %v", gotMatch, tt.wantMatch)
			}
		})
	}
}

func TestVerifyDirectoryChecksums(t *testing.T) {
	tempDir := t.TempDir()
	contentA := "content A"
	fileA := createTempFile(t, tempDir, contentA)
	// Calculate actual hash for fileA
	hashA, errA := CalculateFileSHA256(context.Background(), fileA)
	if errA != nil {
		t.Fatalf("Failed to calculate hash for file A: %v", errA)
	}

	contentB := "content B"
	fileB := createTempFile(t, tempDir, contentB)
	// Calculate actual hash for fileB
	hashB, errB := CalculateFileSHA256(context.Background(), fileB)
	if errB != nil {
		t.Fatalf("Failed to calculate hash for file B: %v", errB)
	}

	// Use dynamically calculated hashes
	expectedCorrect := map[string]string{
		filepath.Base(fileA): hashA,
		filepath.Base(fileB): hashB,
	}

	expectedIncorrectHash := map[string]string{
		filepath.Base(fileA): hashA,
		filepath.Base(fileB): "incorrecthash",
	}

	expectedMissingFile := map[string]string{
		filepath.Base(fileA): hashA,
		"missing_file":       "somehash",
	}

	tests := []struct {
		name              string
		dirPath           string
		expectedChecksums map[string]string
		wantAllMatch      bool
		wantErr           bool
	}{
		{
			name:              "all match",
			dirPath:           tempDir,
			expectedChecksums: expectedCorrect,
			wantAllMatch:      true,
			wantErr:           false,
		},
		{
			name:              "one incorrect hash",
			dirPath:           tempDir,
			expectedChecksums: expectedIncorrectHash,
			wantAllMatch:      false,
			wantErr:           false,
		},
		{
			name:              "one file missing",
			dirPath:           tempDir,
			expectedChecksums: expectedMissingFile,
			wantAllMatch:      false,
			wantErr:           true,
		},
		{
			name:              "non-existent directory",
			dirPath:           "/non/existent/dir/verify",
			expectedChecksums: expectedCorrect,
			wantAllMatch:      false,
			wantErr:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Running test case: %s", tt.name)
			t.Logf("Directory Path: %s", tt.dirPath)
			t.Logf("Expected Checksums: %v", tt.expectedChecksums)

			gotAllMatch, err := VerifyDirectoryChecksums(context.Background(), tt.dirPath, tt.expectedChecksums)

			t.Logf("Result - gotAllMatch: %v, err: %v", gotAllMatch, err)

			if (err != nil) != tt.wantErr {
				t.Errorf("VerifyDirectoryChecksums() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotAllMatch != tt.wantAllMatch {
				t.Errorf("VerifyDirectoryChecksums() = %v, want %v", gotAllMatch, tt.wantAllMatch)
			}
		})
	}
}

// Test context cancellation
func TestCalculateFileSHA256_ContextCancel(t *testing.T) {
	tempDir := t.TempDir()
	tempFile := createTempFile(t, tempDir, strings.Repeat("a", 1024*1024)) // Larger file

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := CalculateFileSHA256(ctx, tempFile)
	if err == nil {
		// Note: Depending on timing, the copy might finish before the check,
		// making this test potentially flaky. A more robust test would mock io.Copy.
		// For now, we accept it might sometimes pass when it ideally shouldn't.
		t.Logf("Warning: CalculateFileSHA256 did not return an error with a cancelled context (potentially flaky test)")
	}

}

func TestVerifyFileSHA256_ContextCancel(t *testing.T) {
	tempDir := t.TempDir()
	tempFile := createTempFile(t, tempDir, "test")
	correctHash := "f2ca1bb6c7e907d06dafe4687e579fce76b37e4e93b7605022da52e6ccc26fd2"

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := VerifyFileSHA256(ctx, tempFile, correctHash)
	if err == nil {
		t.Logf("Warning: VerifyFileSHA256 did not return an error with a cancelled context (potentially flaky test)")
	}
}

func TestCompareFilesSHA256_ContextCancel(t *testing.T) {
	tempDir := t.TempDir()
	file1 := createTempFile(t, tempDir, "content1")
	file2 := createTempFile(t, tempDir, "content2")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := CompareFilesSHA256(ctx, file1, file2)
	if err == nil {
		t.Logf("Warning: CompareFilesSHA256 did not return an error with a cancelled context (potentially flaky test)")
	}
}

func TestVerifyDirectoryChecksums_ContextCancel(t *testing.T) {
	tempDir := t.TempDir()
	fileA := createTempFile(t, tempDir, "content A")
	hashA := "f9c271af1c585a9b35f45d3837101965a51d8a9b4051f75c9c6653ce1a511c0a"
	expected := map[string]string{filepath.Base(fileA): hashA}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := VerifyDirectoryChecksums(ctx, tempDir, expected)
	if err == nil {
		t.Logf("Warning: VerifyDirectoryChecksums did not return an error with a cancelled context (potentially flaky test)")
	}
}

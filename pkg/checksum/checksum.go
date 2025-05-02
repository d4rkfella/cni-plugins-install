package checksum

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	goerrors "errors"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// CalculateFileSHA256 calculates the SHA256 checksum of a file
func CalculateFileSHA256(ctx context.Context, filePath string) (hashStr string, err error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", errors.Wrap(err, "open file")
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(closeErr, "close file")
		}
	}()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", errors.Wrap(err, "calculate hash")
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// VerifyFileSHA256 verifies if a file matches the expected SHA256 checksum
func VerifyFileSHA256(ctx context.Context, filePath, expectedHash string) error {
	actualHash, err := CalculateFileSHA256(ctx, filePath)
	if err != nil {
		return errors.Wrap(err, "calculate file hash")
	}

	if actualHash != expectedHash {
		return errors.NewOperationError("verify checksum", fmt.Errorf("hash mismatch: expected %s, got %s", expectedHash, actualHash))
	}

	return nil
}

// ReadSHAFile reads a SHA256 checksum from a file
func ReadSHAFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.Wrap(err, "read file")
	}

	fields := strings.Fields(string(data))
	if len(fields) < 1 || len(fields[0]) != 64 {
		return "", errors.NewOperationError("read sha file", fmt.Errorf("invalid sha256 format"))
	}

	return fields[0], nil
}

// CompareFilesSHA256 compares the SHA256 checksums of two files
func CompareFilesSHA256(ctx context.Context, path1, path2 string) (bool, error) {
	g, gctx := errgroup.WithContext(ctx)
	var hash1, hash2 string

	g.Go(func() error {
		var err error
		hash1, err = CalculateFileSHA256(gctx, path1)
		return err
	})

	g.Go(func() error {
		var err error
		hash2, err = CalculateFileSHA256(gctx, path2)
		return err
	})

	if err := g.Wait(); err != nil {
		return false, errors.Wrap(err, "calculate file hashes")
	}

	return hash1 == hash2, nil
}

// VerifyDirectoryChecksums verifies the checksums of all files in a directory against a map of expected checksums.
// It returns true if all files match, false otherwise. If an error occurs reading a file or calculating
// a checksum during verification (other than a mismatch), that error is returned.
func VerifyDirectoryChecksums(ctx context.Context, dirPath string, expectedChecksums map[string]string) (bool, error) {
	for file, expectedHash := range expectedChecksums {
		path := filepath.Join(dirPath, file)
		if err := VerifyFileSHA256(ctx, path, expectedHash); err != nil {
			// Check if the error is a simple hash mismatch or a file system error
			var opErr *errors.OperationError
			if goerrors.As(err, &opErr) && opErr.Operation == "verify checksum" {
				// Hash mismatch, return false, no error (as per original logic)
				return false, nil
			}
			// For other errors (e.g., file not found, permission denied), return the error
			return false, errors.Wrap(err, fmt.Sprintf("failed to verify %s", file))
		}
	}
	// If loop completes without errors, all files matched
	return true, nil
}

package checksum

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sync/errgroup"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
)

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

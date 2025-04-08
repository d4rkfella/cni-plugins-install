package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

var (
	httpClient      = &http.Client{Transport: &http.Transport{MaxIdleConns: 10, IdleConnTimeout: 30 * time.Second, ForceAttemptHTTP2: true}}
	baseURL         = "https://github.com/containernetworking/plugins/releases/download"
	targetDir       = "/host/opt/cni/bin"
	tarFormat       = "cni-plugins-linux-amd64-%s.tgz"
	shaFormat       = "cni-plugins-linux-amd64-%s.tgz.sha256"
	downloadTimeout = 15 * time.Minute
	bufferSize      = 1 * 1024 * 1024
	maxRetries      = 3
)

type cleanup struct {
	mu      sync.Mutex
	files   []string
	tempDir string
}

func (c *cleanup) addFile(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files = append(c.files, path)
}

func (c *cleanup) execute(logger zerolog.Logger) {
	c.mu.Lock()
	files := append([]string(nil), c.files...)
	c.mu.Unlock()

	for i := len(files) - 1; i >= 0; i-- {
		if err := os.Remove(files[i]); err != nil && !os.IsNotExist(err) {
			logger.Error().Err(err).Str("file", files[i]).Msg("Failed to remove file")
		}
	}

	if c.tempDir != "" {
		if err := os.RemoveAll(c.tempDir); err != nil && !os.IsNotExist(err) {
			logger.Error().Err(err).Str("dir", c.tempDir).Msg("Failed to remove temp directory")
		}
	}
}

func main() {
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()

	rand.Seed(time.Now().UnixNano())

	if err := run(context.Background(), logger); err != nil {
		logger.Fatal().Err(err).Msg("Application terminated with error")
	}
}

func run(ctx context.Context, logger zerolog.Logger) error {
	mainCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cl := &cleanup{}
	defer cl.execute(logger)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigChan:
			logger.Warn().Str("signal", sig.String()).Msg("Received termination signal, cleaning up...")
			cancel()
		case <-mainCtx.Done():
		}
	}()

	version := os.Getenv("CNI_PLUGINS_VERSION")
	if version == "" {
		return errors.New("CNI_PLUGINS_VERSION environment variable not set")
	}

	logger.Info().Str("version", version).Msg("Starting CNI plugins installation...")

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create target directory: %w", err)
	}

	stagingDir, err := os.MkdirTemp(targetDir, ".cni-staging-")
	if err != nil {
		return fmt.Errorf("failed to create staging directory: %w", err)
	}
	cl.tempDir = stagingDir

	tarURL := fmt.Sprintf("%s/%s/%s", baseURL, version, fmt.Sprintf(tarFormat, version))
	shaURL := fmt.Sprintf("%s/%s/%s", baseURL, version, fmt.Sprintf(shaFormat, version))

	downloadCtx, cancelDownloads := context.WithTimeout(mainCtx, downloadTimeout)
	defer cancelDownloads()

	var tarPath, shaPath string
	g, groupCtx := errgroup.WithContext(downloadCtx)

	g.Go(func() error {
		path, err := downloadFile(groupCtx, tarURL, cl, logger)
		if err != nil {
			return fmt.Errorf("tar download failed: %w", err)
		}
		tarPath = path
		return nil
	})

	g.Go(func() error {
		path, err := downloadFile(groupCtx, shaURL, cl, logger)
		if err != nil {
			return fmt.Errorf("sha download failed: %w", err)
		}
		shaPath = path
		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}

	if err := verifyChecksum(tarPath, shaPath, logger); err != nil {
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	if err := extractTarGz(mainCtx, tarPath, stagingDir, logger); err != nil {
		return fmt.Errorf("extraction failed: %w", err)
	}

	if err := atomicSync(mainCtx, stagingDir, targetDir, logger); err != nil {
		return fmt.Errorf("atomic sync failed: %w", err)
	}

	logger.Info().Msg("CNI plugins installed successfully.")
	return nil
}

func downloadFile(ctx context.Context, url string, cl *cleanup, logger zerolog.Logger) (string, error) {
	var lastErr error

	for retry := 0; retry < maxRetries; retry++ {
		select {
		case <-ctx.Done():
			logger.Error().Err(ctx.Err()).Msg("Context cancelled during download")
			return "", ctx.Err()
		default:
		}

		tmpFile, err := os.CreateTemp("", "cni-download-*")
		if err != nil {
			lastErr = fmt.Errorf("temp file creation failed: %w", err)
			logger.Error().Err(err).Int("retry", retry+1).Int("max_retries", maxRetries).Msg("Error creating temp file")
			continue
		}
		tmpName := tmpFile.Name()
		cl.addFile(tmpName)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = fmt.Errorf("create request failed: %w", err)
			logger.Error().Err(err).Int("retry", retry+1).Int("max_retries", maxRetries).Msg("Error creating request")
			tmpFile.Close()
			os.Remove(tmpName)
			continue
		}
		req.Header.Set("User-Agent", "CNI-Installer/1.0")

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP request failed (attempt %d): %w", retry+1, err)
			logger.Error().Err(err).Int("retry", retry+1).Int("max_retries", maxRetries).Msg("HTTP request failed")
			tmpFile.Close()
			os.Remove(tmpName)
			sleepWithJitter(retry)
			continue
		}

		func() {
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				lastErr = fmt.Errorf("bad status %s (attempt %d)", resp.Status, retry+1)
				logger.Error().Str("status", resp.Status).Int("retry", retry+1).Int("max_retries", maxRetries).Msg("HTTP request returned non-OK status")
				tmpFile.Close()
				os.Remove(tmpName)
				sleepWithJitter(retry)
				return
			}

			buf := make([]byte, bufferSize)
			if _, err := io.CopyBuffer(tmpFile, resp.Body, buf); err != nil {
				lastErr = fmt.Errorf("download copy failed (attempt %d): %w", retry+1, err)
				tmpFile.Close()
				os.Remove(tmpName)
				sleepWithJitter(retry)
				return
			}

			tmpFile.Close()
			logger.Info().Str("file", tmpName).Int64("size", fileSize(tmpName)).Msg("Downloaded file")
			lastErr = nil
		}()

		if lastErr == nil {
			return tmpName, nil
		}
	}

	return "", fmt.Errorf("download failed after %d attempts: %w", maxRetries, errors.Join(lastErr))
}

func sleepWithJitter(retry int) {
	backoff := time.Second * time.Duration(1<<retry)
	jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
	time.Sleep(backoff + jitter)
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func verifyChecksum(filePath, shaPath string, logger zerolog.Logger) error {
	logger.Info().Msg("Verifying checksum...")

	shaData, err := os.ReadFile(shaPath)
	if err != nil {
		return fmt.Errorf("read SHA file failed: %w", err)
	}
	fields := strings.Fields(string(shaData))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return fmt.Errorf("invalid SHA256 file format")
	}
	expectedHash := fields[0]

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file failed: %w", err)
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("hash computation failed: %w", err)
	}

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if actualHash != expectedHash {
		return fmt.Errorf("checksum mismatch\nExpected: %s\nActual:   %s", expectedHash, actualHash)
	}

	return nil
}

func extractTarGz(ctx context.Context, src, dst string, logger zerolog.Logger) error {
	logger.Info().Msg("Extracting files...")

	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open archive failed: %w", err)
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader failed: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		select {
		case <-ctx.Done():
			logger.Error().Err(ctx.Err()).Msg("Context cancelled during extraction")
			return ctx.Err()
		default:
		}

		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read failed: %w", err)
		}

		if header.Typeflag == tar.TypeReg {
			fileName := filepath.Base(header.Name)
			target := filepath.Join(dst, fileName)

			if err := writeFile(target, tr, header.FileInfo().Mode(), logger); err != nil {
				return err
			}
			logger.Info().Str("file", target).Msg("Extracted file")
		}
	}
	return nil
}

func writeFile(path string, r io.Reader, mode os.FileMode, logger zerolog.Logger) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	tmpPath := path + ".tmp"
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, r); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("copy failed: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("atomic rename failed: %w", err)
	}

	return nil
}

func atomicSync(ctx context.Context, staging, target string, logger zerolog.Logger) error {
	type fileOperation struct {
		original string
		backup   string
	}

	var operations []fileOperation
	var rollbackErr error

	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("failed to read staging directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		srcPath := filepath.Join(staging, entry.Name())
		dstPath := filepath.Join(target, entry.Name())

		select {
		case <-ctx.Done():
			rollbackErr = fmt.Errorf("operation cancelled: %w", ctx.Err())
			break
		default:
		}

		if rollbackErr != nil {
			break
		}

		if targetInfo, err := os.Stat(dstPath); err == nil && targetInfo.IsDir() {
			rollbackErr = fmt.Errorf("can't replace directory with file: %s", dstPath)
			break
		}

		var backupPath string
		if _, err := os.Stat(dstPath); err == nil {
			backupPath = filepath.Join(target, entry.Name()+".backup-"+time.Now().Format("20060102T150405"))
			if err := os.Rename(dstPath, backupPath); err != nil {
				rollbackErr = fmt.Errorf("backup failed: %w", err)
				break
			}
			logger.Debug().Str("path", backupPath).Msg("Created backup file")
		}

		if err := os.Rename(srcPath, dstPath); err != nil {
			rollbackErr = fmt.Errorf("failed to sync file: %w", err)
			if backupPath != "" {
				if rerr := os.Rename(backupPath, dstPath); rerr != nil {
					logger.Error().Err(rerr).Msg("Failed to restore backup during rollback")
				}
			}
			break
		}

		operations = append(operations, fileOperation{
			original: dstPath,
			backup:   backupPath,
		})
	}

	if rollbackErr != nil {
		logger.Error().Err(rollbackErr).Msg("Initiating rollback")
		for i := len(operations) - 1; i >= 0; i-- {
			op := operations[i]
			if err := os.Remove(op.original); err != nil && !os.IsNotExist(err) {
				logger.Warn().Err(err).Str("file", op.original).Msg("Failed to remove during rollback")
			}
			if op.backup != "" {
				if err := os.Rename(op.backup, op.original); err != nil {
					logger.Warn().Err(err).Str("backup", op.backup).Msg("Failed to restore backup")
				}
			}
		}
		return rollbackErr
	}

	for _, op := range operations {
		if op.backup != "" {
			if err := os.Remove(op.backup); err != nil && !os.IsNotExist(err) {
				logger.Warn().Err(err).Str("backup", op.backup).Msg("Failed to clean up backup")
			} else {
				logger.Debug().Str("backup", op.backup).Msg("Cleaned up backup file")
			}
		}
	}

	logger.Info().Int("files", len(operations)).Msg("Sync completed successfully")
	return nil
}

func sameChecksum(path1, path2 string) (bool, error) {
	var wg sync.WaitGroup
	wg.Add(2)

	var hash1, hash2 string
	var err1, err2 error

	go func() {
		defer wg.Done()
		hash1, err1 = fileChecksum(path1)
	}()
	go func() {
		defer wg.Done()
		hash2, err2 = fileChecksum(path2)
	}()

	wg.Wait()
	if err1 != nil {
		return false, err1
	}
	if err2 != nil {
		return false, err2
	}
	return hash1 == hash2, nil
}

func fileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

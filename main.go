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

const (
	dirPerm        = 0o755
	filePerm       = 0o644
	backupPrefix   = ".backup-"
	backupMaxAge   = 24 * time.Hour
	tempFilePrefix = "cni-download-"
)

type Config struct {
	BaseURL         string
	TargetDir       string
	DownloadTimeout time.Duration
	MaxRetries      int
	BufferSize      int
}

var cfg = Config{
	BaseURL:         "https://github.com/containernetworking/plugins/releases/download",
	TargetDir:       "/host/opt/cni/bin",
	DownloadTimeout: 2 * time.Minute,
	MaxRetries:      3,
	BufferSize:      1 * 1024 * 1024,
}

var (
	httpClient = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:      10,
			IdleConnTimeout:   30 * time.Second,
			ForceAttemptHTTP2: true,
		},
	}
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
		logger.Debug().Str("file", files[i]).Msg("Cleaning file")

		if err := secureRemove(files[i]); err != nil {
			logger.Warn().Err(err).Str("file", files[i]).Msg("Cleanup failed")
		}
	}

	if c.tempDir != "" {
		logger.Debug().Str("dir", c.tempDir).Msg("Cleaning temp directory")
		if err := secureRemoveAll(c.tempDir); err != nil {
			logger.Warn().Err(err).Str("dir", c.tempDir).Msg("Temp directory cleanup failed")
		}
	}
}

func createLogger(logLevel string) zerolog.Logger {
	level := zerolog.InfoLevel
	switch logLevel {
	case "debug":
		level = zerolog.DebugLevel
	case "warn":
		level = zerolog.WarnLevel
	case "error":
		level = zerolog.ErrorLevel
	case "fatal":
		level = zerolog.FatalLevel
	}

	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		Level(level).
		With().
		Timestamp().
		Logger()
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	logger := createLogger(logLevel)

	rand.Seed(time.Now().UnixNano())

	if err := run(ctx, logger); err != nil {
		logger.Fatal().Err(err).Msg("Installation failed")
	}
}

func run(ctx context.Context, logger zerolog.Logger) error {
	cl := &cleanup{}
	defer cl.execute(logger)

	version := os.Getenv("CNI_PLUGINS_VERSION")
	if version == "" {
		return errors.New("CNI_PLUGINS_VERSION environment variable not set")
	}

	logger.Info().Str("version", version).Msg("Starting installation")

	if err := os.MkdirAll(cfg.TargetDir, dirPerm); err != nil {
		return fmt.Errorf("create target directory: %w", err)
	}

	stagingDir, err := os.MkdirTemp(cfg.TargetDir, ".cni-staging-*")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	cl.tempDir = stagingDir

	logger.Debug().Str("staging_dir", stagingDir).Msg("Staging directory created")

	tarName := fmt.Sprintf("cni-plugins-linux-amd64-%s.tgz", version)
	shaName := fmt.Sprintf("%s.sha256", tarName)
	tarURL := fmt.Sprintf("%s/%s/%s", cfg.BaseURL, version, tarName)
	shaURL := fmt.Sprintf("%s/%s/%s", cfg.BaseURL, version, shaName)

	downloadCtx, cancel := context.WithTimeout(ctx, cfg.DownloadTimeout)
	defer cancel()

	var tarPath, shaPath string
	g, gCtx := errgroup.WithContext(downloadCtx)

	g.Go(func() error {
		var err error
		tarPath, err = downloadFile(gCtx, tarURL, cl, logger)
		return err
	})

	g.Go(func() error {
		var err error
		shaPath, err = downloadFile(gCtx, shaURL, cl, logger)
		return err
	})

	if err := g.Wait(); err != nil {
		return fmt.Errorf("download failed %w", err)
	}

	if err := verifyChecksum(tarPath, shaPath, logger); err != nil {
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	if err := extractTarGz(ctx, tarPath, stagingDir, logger); err != nil {
		return fmt.Errorf("archive extraction failed: %w", err)
	}

	if err := atomicSync(ctx, stagingDir, cfg.TargetDir, logger); err != nil {
		return fmt.Errorf("atomic sync failed: %w", err)
	}

	logger.Info().Msg("Installation completed successfully")
	return nil
}

func downloadFile(ctx context.Context, url string, cl *cleanup, logger zerolog.Logger) (string, error) {
	var resultPath string

	err := withRetry(ctx, logger, cfg.MaxRetries, func(attempt int) error {
		tmpFile, err := os.CreateTemp("", tempFilePrefix+"*")
		if err != nil {
			return fmt.Errorf("create temp file: %w", err)
		}
		defer tmpFile.Close()

		tmpName := tmpFile.Name()
		cl.addFile(tmpName)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("User-Agent", "CNI-Installer/1.0")

		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("http request: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status: %s", resp.Status)
		}

		buf := make([]byte, cfg.BufferSize)
		if _, err := io.CopyBuffer(tmpFile, resp.Body, buf); err != nil {
			return fmt.Errorf("download content: %w", err)
		}

		resultPath = tmpName
		logger.Info().
			Str("url", url).
			Str("path", tmpName).
			Int64("size", fileSize(tmpName)).
			Msg("Download completed")
		return nil
	})

	return resultPath, err
}

func withRetry(ctx context.Context, logger zerolog.Logger, maxAttempts int, fn func(int) error) error {
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := fn(attempt)
		if err == nil {
			return nil
		}

		logger.Warn().
			Err(err).
			Int("attempt", attempt+1).
			Int("max_attempts", maxAttempts).
			Msg("Operation failed, retrying")

		lastErr = err
		sleepWithJitter(attempt)
	}

	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

func sleepWithJitter(attempt int) {
	base := time.Second * time.Duration(1<<attempt)
	jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
	time.Sleep(base + jitter)
}

func verifyChecksum(filePath, shaPath string, logger zerolog.Logger) error {
	expectedHash, err := readSHAFile(shaPath)
	if err != nil {
		return fmt.Errorf("read sha file: %w", err)
	}

	actualHash, err := fileChecksum(context.Background(), filePath)
	if err != nil {
		return fmt.Errorf("compute checksum: %w", err)
	}

	if actualHash != expectedHash {
		return fmt.Errorf("checksum mismatch (expected: %s, actual: %s)", expectedHash, actualHash)
	}

	logger.Info().Msg("Checksum verification successful")
	return nil
}

func readSHAFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}

	fields := strings.Fields(string(data))
	if len(fields) < 1 || len(fields[0]) != 64 {
		return "", errors.New("invalid sha256 format")
	}

	return fields[0], nil
}

func extractTarGz(ctx context.Context, src, dst string, logger zerolog.Logger) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		if header.Typeflag == tar.TypeReg {
			target := filepath.Join(dst, filepath.Base(header.Name))
			if err := writeFileAtomic(target, tr, header.FileInfo().Mode(), logger); err != nil {
				return fmt.Errorf("write file %s: %w", header.Name, err)
			}
		}
	}

	return nil
}

func writeFileAtomic(path string, r io.Reader, mode os.FileMode, logger zerolog.Logger) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := writeFile(tmpPath, r, mode); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic rename: %w", err)
	}

	logger.Debug().Str("path", path).Msg("File written successfully")
	return nil
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, r); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write content: %w", err)
	}

	return nil
}

func atomicSync(ctx context.Context, staging, target string, logger zerolog.Logger) error {
	defer cleanupOldBackups(target, backupMaxAge, logger)

	var operations []struct {
		original string
		backup   string
	}
	var rollbackErr error

	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("read staging directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		srcPath := filepath.Join(staging, entry.Name())
		dstPath := filepath.Join(target, entry.Name())

		if rollbackErr != nil {
			break
		}

		if err := processFile(ctx, srcPath, dstPath, &operations, &rollbackErr, logger); err != nil {
			rollbackErr = err
		}
	}

	if rollbackErr != nil {
		performRollback(operations, logger)
		return rollbackErr
	}

	cleanupBackups(operations, logger)
	return nil
}

func processFile(ctx context.Context, src, dst string, operations *[]struct{ original, backup string }, rollbackErr *error, logger zerolog.Logger) error {
	select {
	case <-ctx.Done():
		*rollbackErr = fmt.Errorf("operation cancelled: %w", ctx.Err())
		return nil
	default:
	}

	if targetInfo, err := os.Stat(dst); err == nil && targetInfo.IsDir() {
		return fmt.Errorf("cannot replace directory with file: %s", dst)
	}

	same, err := sameChecksum(ctx, src, dst)
	if err == nil && same {
		logger.Debug().Str("file", filepath.Base(src)).Msg("Skipping unchanged file")
		return nil
	}

	backupPath, err := createBackup(dst, logger)
	if err != nil {
		return err
	}

	if err := os.Rename(src, dst); err != nil {
		return handleSyncError(dst, backupPath, err, logger)
	}

	*operations = append(*operations, struct{ original, backup string }{dst, backupPath})
	return nil
}

func createBackup(dst string, logger zerolog.Logger) (string, error) {
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return "", nil
	}

	backupPath := fmt.Sprintf("%s%s%s", dst, backupPrefix, time.Now().Format("20060102T150405"))
	if err := os.Rename(dst, backupPath); err != nil {
		return "", fmt.Errorf("create backup: %w", err)
	}

	logger.Debug().Str("path", backupPath).Msg("Created backup")
	return backupPath, nil
}

func handleSyncError(dst, backup string, err error, logger zerolog.Logger) error {
	logger.Error().Err(err).Str("path", dst).Msg("Sync failed")

	if backup != "" {
		if rerr := os.Rename(backup, dst); rerr != nil {
			logger.Error().Err(rerr).Msg("Backup restoration failed")
		}
	}

	return fmt.Errorf("sync file %s: %w", dst, err)
}

func performRollback(ops []struct{ original, backup string }, logger zerolog.Logger) {
	for i := len(ops) - 1; i >= 0; i-- {
		op := ops[i]
		if err := secureRemove(op.original); err != nil {
			logger.Warn().Err(err).Str("path", op.original).Msg("Rollback cleanup failed")
		}

		if op.backup != "" {
			if err := os.Rename(op.backup, op.original); err != nil {
				logger.Warn().Err(err).Str("backup", op.backup).Msg("Backup restoration failed")
			}
		}
	}
}

func cleanupBackups(ops []struct{ original, backup string }, logger zerolog.Logger) {
	for _, op := range ops {
		if op.backup != "" {
			if err := secureRemove(op.backup); err != nil {
				logger.Warn().Err(err).Str("backup", op.backup).Msg("Backup cleanup failed")
			}
		}
	}
}

func cleanupOldBackups(dir string, maxAge time.Duration, logger zerolog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Warn().Err(err).Msg("Failed to list directory for backup cleanup")
		return
	}

	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), backupPrefix) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			path := filepath.Join(dir, entry.Name())
			if err := secureRemove(path); err == nil {
				logger.Debug().Str("path", path).Msg("Cleaned up old backup")
			}
		}
	}
}

func sameChecksum(ctx context.Context, path1, path2 string) (bool, error) {
	g, gctx := errgroup.WithContext(ctx)
	var hash1, hash2 string

	g.Go(func() error {
		var err error
		hash1, err = fileChecksum(gctx, path1)
		return err
	})

	g.Go(func() error {
		var err error
		hash2, err = fileChecksum(gctx, path2)
		return err
	})

	if err := g.Wait(); err != nil {
		return false, err
	}

	return hash1 == hash2, nil
}

func fileChecksum(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	hasher := sha256.New()
	tee := io.TeeReader(f, hasher)

	buf := make([]byte, cfg.BufferSize)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		_, err := tee.Read(buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read file: %w", err)
		}
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func secureRemove(path string) error {
	err := os.Remove(path)
	if err != nil {
		return err
	}
	return nil
}

func secureRemoveAll(path string) error {
	err := os.RemoveAll(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

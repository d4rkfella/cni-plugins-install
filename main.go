package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

var (
	httpClient    = &http.Client{Transport: &http.Transport{MaxIdleConns: 10, IdleConnTimeout: 30 * time.Second, ForceAttemptHTTP2: true}}
	baseURL       = "https://github.com/containernetworking/plugins/releases/download"
	targetDir     = "/tmp/host/opt/cni/bin"
	tarFormat     = "cni-plugins-linux-amd64-%s.tgz"
	shaFormat     = "cni-plugins-linux-amd64-%s.tgz.sha256"
	downloadTimeout = 15 * time.Minute
	bufferSize    = 1 * 1024 * 1024
	maxRetries    = 3
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

	if err := run(logger); err != nil {
		logger.Fatal().Err(err).Msg("Application terminated with error")
	}
}

func run(logger zerolog.Logger) error {
	mainCtx, cancel := context.WithCancel(context.Background())
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
	defer os.RemoveAll(stagingDir)

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

		target := filepath.Join(dst, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, header.FileInfo().Mode()); err != nil {
				return fmt.Errorf("failed to create directory: %w", err)
			}
		case tar.TypeReg:
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
	type syncedFile struct {
		original string
		backup   string
	}

	var (
		modified      []syncedFile
		installedFiles int
	)

	err := filepath.Walk(staging, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		select {
		case <-ctx.Done():
			logger.Error().Err(ctx.Err()).Msg("Context cancelled during sync")
			return ctx.Err()
		default:
		}

		relPath, err := filepath.Rel(staging, srcPath)
		if err != nil {
			return fmt.Errorf("relative path error: %w", err)
		}

		dstPath := filepath.Join(target, relPath)
		dstDir := filepath.Dir(dstPath)
		
		if err := os.MkdirAll(dstDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dstDir, err)
		}

		if targetInfo, err := os.Stat(dstPath); err == nil {
			if targetInfo.IsDir() {
				return fmt.Errorf("can't replace directory with file: %s", dstPath)
			}

			same, err := sameChecksum(srcPath, dstPath)
			if err != nil {
				return fmt.Errorf("checksum comparison failed: %w", err)
			}
			if same {
				logger.Info().Str("file", dstPath).Msg("Skipping identical file")
				return nil
			}

			backupPath := fmt.Sprintf("%s.backup-%s", dstPath, time.Now().Format("20060102T150405"))
			if err := os.Rename(dstPath, backupPath); err != nil {
				return fmt.Errorf("backup failed for %s: %w", dstPath, err)
			}
			modified = append(modified, syncedFile{original: dstPath, backup: backupPath})
			logger.Info().Str("src", dstPath).Str("backup", backupPath).Msg("Backed up existing file")
		}

		if err := moveOrCopyFile(srcPath, dstPath, info.Mode()); err != nil {
			return fmt.Errorf("failed to move file: %w", err)
		}
		logger.Info().Str("file", dstPath).Msg("Installed file")

		installedFiles++
		return nil
	})

	if err != nil {
		logger.Error().Err(err).Msg("Error during sync. Rolling back...")
		for i := len(modified) - 1; i >= 0; i-- {
			m := modified[i]
			if err := os.Remove(m.original); err != nil && !os.IsNotExist(err) {
				logger.Warn().Err(err).Str("file", m.original).Msg("Error removing new file during rollback")
			}
			if err := os.Rename(m.backup, m.original); err != nil {
				logger.Warn().Err(err).Str("file", m.backup).Msg("Error restoring backup")
			}
			logger.Info().Str("file", m.original).Msg("Restored backup")
		}
		return err
	}

	for _, m := range modified {
		if err := os.Remove(m.backup); err != nil {
			logger.Warn().Err(err).Str("backup", m.backup).Msg("Failed to remove backup")
		}
	}

	logger.Info().Int("files_installed", installedFiles).Str("directory", target).Msg("Installed plugins")
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

func moveOrCopyFile(src, dst string, mode os.FileMode) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if linkErr, ok := err.(*os.LinkError); ok && errors.Is(linkErr.Err, syscall.EXDEV) {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()

		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		defer out.Close()

		if _, err := io.Copy(out, in); err != nil {
			return err
		}

		if err := os.Remove(src); err != nil {
			return err
		}
		return nil
	}
	return err
}
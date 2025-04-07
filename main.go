package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	baseURL         = "https://github.com/containernetworking/plugins/releases/download"
	tarFormat       = "cni-plugins-linux-amd64-%s.tgz"
	shaFormat       = "cni-plugins-linux-amd64-%s.tgz.sha256"
	targetDir       = "/host/opt/cni/bin"
	downloadTimeout = 15 * time.Minute
	bufferSize      = 1 * 1024 * 1024
)

var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:      10,
		IdleConnTimeout:   30 * time.Second,
		ForceAttemptHTTP2: true,
	},
}

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

func (c *cleanup) execute() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := len(c.files) - 1; i >= 0; i-- {
		if _, err := os.Stat(c.files[i]); os.IsNotExist(err) {
			continue
		}
		if err := os.Remove(c.files[i]); err != nil && !os.IsNotExist(err) {
			log.Printf("Cleanup error removing %s: %v", c.files[i], err)
		}
	}

	if c.tempDir != "" {
		if _, err := os.Stat(c.tempDir); !os.IsNotExist(err) {
			if err := os.RemoveAll(c.tempDir); err != nil && !os.IsNotExist(err) {
				log.Printf("Cleanup error removing temp dir %s: %v", c.tempDir, err)
			}
		}
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	mainCtx, cancelMain := context.WithCancel(context.Background())
	defer cancelMain()

	cl := &cleanup{}
	defer cl.execute()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received signal: %s. Cleaning up...", sig)
		cancelMain()
	}()

	version := os.Getenv("CNI_PLUGINS_VERSION")
	if version == "" {
		return fmt.Errorf("CNI_PLUGINS_VERSION environment variable not set")
	}

	log.Println("Starting CNI plugins installation...")

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create target directory: %w", err)
	}

	stagingDir, err := os.MkdirTemp(targetDir, ".cni-staging-")
	if err != nil {
		return fmt.Errorf("failed to create staging directory: %w", err)
	}
	cl.tempDir = stagingDir
	log.Printf("Using staging directory: %s", stagingDir)

	tarURL := fmt.Sprintf("%s/%s/%s", baseURL, version, fmt.Sprintf(tarFormat, version))
	shaURL := fmt.Sprintf("%s/%s/%s", baseURL, version, fmt.Sprintf(shaFormat, version))

	downloadCtx, cancelDownloads := context.WithTimeout(mainCtx, downloadTimeout)
	defer cancelDownloads()

	g, groupCtx := errgroup.WithContext(downloadCtx)
	var tarPath, shaPath string

	g.Go(func() error {
		path, err := downloadFile(groupCtx, tarURL, cl)
		if err != nil {
			return fmt.Errorf("tar download failed: %w", err)
		}
		tarPath = path
		return nil
	})

	g.Go(func() error {
		path, err := downloadFile(groupCtx, shaURL, cl)
		if err != nil {
			return fmt.Errorf("sha download failed: %w", err)
		}
		shaPath = path
		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}

	if err := verifyChecksum(tarPath, shaPath); err != nil {
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	if err := extractTarGz(mainCtx, tarPath, stagingDir); err != nil {
		return fmt.Errorf("extraction failed: %w", err)
	}

	if err := atomicSync(stagingDir, targetDir); err != nil {
		return fmt.Errorf("atomic sync failed: %w", err)
	}

	log.Println("Successfully installed CNI plugins")
	return nil
}

func downloadFile(ctx context.Context, url string, cl *cleanup) (string, error) {
	const maxRetries = 3
	var lastErr error

	for retry := 0; retry < maxRetries; retry++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		tmpFile, err := os.CreateTemp("", "cni-download-*")
		if err != nil {
			lastErr = fmt.Errorf("temp file creation failed: %w", err)
			continue
		}
		tmpName := tmpFile.Name()
		cl.addFile(tmpName)

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpName)
			lastErr = fmt.Errorf("create request failed: %w", err)
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")

		resp, err := httpClient.Do(req)
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpName)
			lastErr = fmt.Errorf("HTTP request failed (attempt %d): %w", retry+1, err)
			time.Sleep(time.Second * time.Duration(1<<uint(retry)))
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			tmpFile.Close()
			os.Remove(tmpName)
			lastErr = fmt.Errorf("bad status %s (attempt %d)", resp.Status, retry+1)
			time.Sleep(time.Second * time.Duration(1<<uint(retry)))
			continue
		}

		if _, err := io.Copy(tmpFile, resp.Body); err != nil {
			resp.Body.Close()
			tmpFile.Close()
			os.Remove(tmpName)
			lastErr = fmt.Errorf("download copy failed (attempt %d): %w", retry+1, err)
			time.Sleep(time.Second * time.Duration(1<<uint(retry)))
			continue
		}

		resp.Body.Close()
		tmpFile.Close()
		log.Printf("Downloaded %s (%d bytes)", tmpName, fileSize(tmpName))
		return tmpName, nil
	}

	return "", fmt.Errorf("download failed after %d attempts: %w", maxRetries, lastErr)
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func verifyChecksum(filePath, shaPath string) error {
	log.Println("Verifying checksum...")

	shaData, err := os.ReadFile(shaPath)
	if err != nil {
		return fmt.Errorf("read SHA file failed: %w", err)
	}
	if len(shaData) < 64 {
		return fmt.Errorf("invalid SHA256 file format")
	}
	expectedHash := string(shaData[:64])

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

func extractTarGz(ctx context.Context, src, dst string) error {
	log.Println("Extracting files...")

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
			if err := writeFile(target, tr, header.FileInfo().Mode()); err != nil {
				return err
			}
			log.Printf("Extracted: %s", target)
		}
	}
	return nil
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
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

func atomicSync(staging, target string) error {
	return filepath.Walk(staging, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		relPath, err := filepath.Rel(staging, srcPath)
		if err != nil {
			return fmt.Errorf("relative path error: %w", err)
		}

		dstPath := filepath.Join(target, relPath)
		if err := os.Rename(srcPath, dstPath); err != nil {
			return fmt.Errorf("failed to move file: %w", err)
		}

		return nil
	})
}

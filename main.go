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
	downloadTimeout = 5 * time.Minute
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
	dirs    []string
	tempDir string
}

type result struct {
	path     string
	fileType string
}

func (c *cleanup) addFile(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files = append(c.files, path)
}

func (c *cleanup) addDir(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirs = append(c.dirs, path)
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

	for i := len(c.dirs) - 1; i >= 0; i-- {
		if _, err := os.Stat(c.dirs[i]); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(c.dirs[i]); err != nil && !os.IsNotExist(err) {
			log.Printf("Cleanup error removing %s: %v", c.dirs[i], err)
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

    parentDir := filepath.Dir(targetDir)
    if err := os.MkdirAll(parentDir, 0755); err != nil {
        return fmt.Errorf("failed to create parent directory %s: %w", parentDir, err)
    }

    stagingDir, err := os.MkdirTemp(parentDir, "cni-staging-")
    if err != nil {
        return fmt.Errorf("failed to create staging directory: %w", err)
    }
    cl.tempDir = stagingDir
    log.Printf("Using staging directory: %s", stagingDir)

    tarURL := fmt.Sprintf("%s/%s/%s", baseURL, version, fmt.Sprintf(tarFormat, version))
    shaURL := fmt.Sprintf("%s/%s/%s", baseURL, version, fmt.Sprintf(shaFormat, version))

    downloadCtx, cancelDownloads := context.WithTimeout(mainCtx, 15*time.Minute)
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

    if err := extractTarGz(mainCtx, tarPath, stagingDir, cl); err != nil {
        return fmt.Errorf("extraction failed: %w", err)
    }

    if err := os.Rename(stagingDir, targetDir); err != nil {
        return fmt.Errorf("atomic install failed: %w", err)
    }
    cl.tempDir = ""

    log.Println("Successfully installed CNI plugins")
    return nil
}

func downloadFile(ctx context.Context, url string, cl *cleanup) (string, error) {
	const (
		maxRetries     = 3
		initialBackoff = 2 * time.Second
		maxBackoff     = 30 * time.Second
		perTryTimeout  = 5 * time.Minute
	)

	log.Printf("Downloading %s", url)

	var tmpName string
	var lastErr error

	client := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: perTryTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}

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
		tmpName = tmpFile.Name()
		cl.addFile(tmpName)

		attemptCtx, cancelAttempt := context.WithTimeout(ctx, perTryTimeout)
		defer cancelAttempt()

		req, err := http.NewRequestWithContext(attemptCtx, "GET", url, nil)
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpName)
			lastErr = fmt.Errorf("create request failed: %w", err)
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")

		resp, err := client.Do(req)
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpName)
			lastErr = fmt.Errorf("HTTP request failed (attempt %d): %w", retry+1, err)
			sleepWithBackoff(retry, initialBackoff, maxBackoff)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			tmpFile.Close()
			os.Remove(tmpName)
			lastErr = fmt.Errorf("bad status %s (attempt %d)", resp.Status, retry+1)
			if shouldRetryStatus(resp.StatusCode) {
				sleepWithBackoff(retry, initialBackoff, maxBackoff)
				continue
			}
			break
		}

		buf := make([]byte, bufferSize)
		written, err := io.CopyBuffer(tmpFile, resp.Body, buf)
		resp.Body.Close()
		tmpFile.Close()

		if err != nil {
			os.Remove(tmpName)
			lastErr = fmt.Errorf("download copy failed (attempt %d): %w", retry+1, err)
			sleepWithBackoff(retry, initialBackoff, maxBackoff)
			continue
		}

		log.Printf("Downloaded %s (%.2f MB)", tmpName, float64(written)/1024/1024)
		return tmpName, nil
	}

	return "", fmt.Errorf("download failed after %d attempts: %w", maxRetries, lastErr)
}

func sleepWithBackoff(retry int, initial, max time.Duration) {
	backoff := initial * time.Duration(1<<uint(retry))
	if backoff > max {
		backoff = max
	}
	time.Sleep(backoff)
}

func shouldRetryStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		statusCode >= http.StatusInternalServerError
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
        return fmt.Errorf("checksum mismatch\nExpected: %s\nActual:   %s",
            expectedHash, actualHash)
    }

    return nil
}

func extractTarGz(ctx context.Context, src, dst string, cl *cleanup) error {
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
	buf := make([]byte, bufferSize)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			header, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("tar read failed: %w", err)
			}

			target := filepath.Join(dst, header.Name)

			switch header.Typeflag {
			case tar.TypeDir:
				if err := os.MkdirAll(target, 0755); err != nil {
					return fmt.Errorf("mkdir failed: %w", err)
				}
				cl.addDir(target)
			case tar.TypeReg:
				if err := writeFile(ctx, target, tr, header.FileInfo().Mode(), buf, cl); err != nil {
					return err
				}
				log.Printf("Extracted: %s", target)
			}
		}
	}
}

func writeFile(ctx context.Context, path string, r io.Reader, mode os.FileMode, buf []byte, cl *cleanup) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()

	cl.addFile(path)

	_, err = io.CopyBuffer(f, r, buf)
	return err
}

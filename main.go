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
)

const (
	baseURL         = "https://github.com/containernetworking/plugins/releases/download"
	tarFormat       = "cni-plugins-linux-amd64-%s.tgz"
	shaFormat       = "cni-plugins-linux-amd64-%s.tgz.sha256"
	targetDir       = "/tmp/host/opt/cni/bin"
	downloadTimeout = 5 * time.Minute
	bufferSize      = 1 * 1024 * 1024
)

var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     30 * time.Second,
		ForceAttemptHTTP2:   true,
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl := &cleanup{}
	defer cl.execute()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received signal: %s. Cleaning up...", sig)
		cancel()
	}()

	version := os.Getenv("CNI_PLUGINS_VERSION")
	if version == "" {
		return fmt.Errorf("CNI_PLUGINS_VERSION environment variable not set")
	}

	log.Println("Starting CNI plugins installation...")

	stagingDir, err := os.MkdirTemp(filepath.Dir(targetDir), "cni-staging-")
	if err != nil {
		return fmt.Errorf("failed to create staging directory: %w", err)
	}
	cl.tempDir = stagingDir
	log.Printf("Using staging directory: %s", stagingDir)

	tarURL := fmt.Sprintf("%s/%s/%s", baseURL, version, fmt.Sprintf(tarFormat, version))
	shaURL := fmt.Sprintf("%s/%s/%s", baseURL, version, fmt.Sprintf(shaFormat, version))

	var wg sync.WaitGroup
	results := make(chan result, 2)
	errors := make(chan error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		path, err := downloadFile(ctx, tarURL, cl)
		if err != nil {
			errors <- fmt.Errorf("tar download failed: %w", err)
			return
		}
		results <- result{path: path, fileType: "tar"}
	}()

	go func() {
		defer wg.Done()
		path, err := downloadFile(ctx, shaURL, cl)
		if err != nil {
			errors <- fmt.Errorf("sha download failed: %w", err)
			return
		}
		results <- result{path: path, fileType: "sha"}
	}()

	go func() {
		wg.Wait()
		close(results)
		close(errors)
	}()

	var tarPath, shaPath string
	for {
		select {
		case res, ok := <-results:
			if !ok {
				results = nil
				continue
			}
			switch res.fileType {
			case "tar":
				tarPath = res.path
			case "sha":
				shaPath = res.path
			}
		case err, ok := <-errors:
			if ok {
				return err
			}
			errors = nil
		case <-ctx.Done():
			return fmt.Errorf("operation cancelled: %w", ctx.Err())
		}

		if results == nil && errors == nil {
			break
		}
	}

	if err := verifyChecksum(ctx, tarPath, shaPath); err != nil {
		return fmt.Errorf("checksum verification failed: %w", err)
	}

	if err := extractTarGz(ctx, tarPath, stagingDir, cl); err != nil {
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
	log.Printf("Downloading %s", url)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")

	tmpFile, err := os.CreateTemp("", "cni-download-*")
	if err != nil {
		return "", fmt.Errorf("temp file creation failed: %w", err)
	}
	tmpName := tmpFile.Name()
	cl.addFile(tmpName)

	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status: %s", resp.Status)
	}

	buf := make([]byte, bufferSize)
	written, err := io.CopyBuffer(tmpFile, resp.Body, buf)
	if closeErr := tmpFile.Close(); closeErr != nil {
		return "", fmt.Errorf("file close failed: %w", closeErr)
	}

	if err != nil {
		return "", fmt.Errorf("download copy failed: %w", err)
	}

	log.Printf("Downloaded %s (%.2f MB)", tmpName, float64(written)/1024/1024)
	return tmpName, nil
}

func verifyChecksum(ctx context.Context, filePath, shaPath string) error {
	log.Println("Verifying checksum...")

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
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
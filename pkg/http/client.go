package http

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/constants"
	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
	"github.com/darkfella/cni-plugins-install/pkg/retry"
	"golang.org/x/sync/errgroup"
)

// Config represents the HTTP client configuration
type Config struct {
	BaseURL         string
	DownloadTimeout time.Duration
	MaxRetries      int
	BufferSize      int
	UserAgent       string
}

// DefaultConfig returns the default HTTP client configuration
func DefaultConfig() *Config {
	return &Config{
		DownloadTimeout: 30 * time.Second,
		MaxRetries:      3,
		BufferSize:      32 * 1024, // 32KB buffer
		UserAgent:       constants.DefaultUserAgent,
	}
}

// Client represents an HTTP client with retry capabilities
type Client struct {
	logger     *logging.Logger
	httpClient *http.Client
	config     *Config
}

// NewClient creates a new HTTP client
func NewClient(logger *logging.Logger, httpClient *http.Client, config *Config) *Client {
	if config == nil {
		config = DefaultConfig()
	}
	return &Client{
		logger:     logger,
		httpClient: httpClient,
		config:     config,
	}
}

// DownloadFile downloads a file from a URL to a file with retry capabilities
func (c *Client) DownloadFile(ctx context.Context, url string, file io.Writer) error {
	return retry.WithRetry(ctx, &retry.Config{
		MaxRetries: c.config.MaxRetries,
		Logger:     c.logger,
	}, func(attempt int) (err error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return errors.Wrap(err, "create request")
		}
		req.Header.Set("User-Agent", c.config.UserAgent)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return errors.Wrap(err, "http request")
		}
		defer func() {
			if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
				err = errors.Wrap(closeErr, "close response body")
			}
		}()

		if resp.StatusCode != http.StatusOK {
			return &errors.HTTPError{
				StatusCode: resp.StatusCode,
				Status:     resp.Status,
				URL:        url,
				Method:     http.MethodGet,
				Message:    "unexpected status code",
			}
		}

		buf := make([]byte, c.config.BufferSize)
		if _, err := io.CopyBuffer(file, resp.Body, buf); err != nil {
			return errors.Wrap(err, "download content")
		}

		c.logger.Info().
			Str("url", url).
			Msg("Download completed")
		return nil
	})
}

// DownloadFiles downloads multiple files concurrently
func (c *Client) DownloadFiles(ctx context.Context, urls []string, files []io.Writer) error {
	if len(urls) != len(files) {
		return errors.NewOperationError("download files", fmt.Errorf("mismatched number of URLs and files"))
	}

	g, gCtx := errgroup.WithContext(ctx)
	for i := range urls {
		i := i // Create new variable for goroutine
		g.Go(func() error {
			return c.DownloadFile(gCtx, urls[i], files[i])
		})
	}

	if err := g.Wait(); err != nil {
		return errors.Wrap(err, "download files")
	}

	return nil
}

// GetFileSize gets the size of a file from a URL
func (c *Client) GetFileSize(ctx context.Context, url string) (size int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, errors.Wrap(err, "create request")
	}
	req.Header.Set("User-Agent", c.config.UserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, errors.Wrap(err, "http request")
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(closeErr, "close response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return 0, &errors.HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			URL:        url,
			Method:     http.MethodHead,
			Message:    "unexpected status code",
		}
	}

	return resp.ContentLength, nil
}

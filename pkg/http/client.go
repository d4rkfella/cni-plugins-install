package http

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/constants"
	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
	"github.com/darkfella/cni-plugins-install/pkg/retry"
)

type Config struct {
	BaseURL         string
	DownloadTimeout time.Duration
	MaxRetries      int
	BufferSize      int
	UserAgent       string
}

func DefaultConfig() *Config {
	return &Config{
		DownloadTimeout: constants.DefaultDownloadTimeout
		MaxRetries:      constants.DefaultMaxRetries
		BufferSize:      constants.DefaultBufferSize
		UserAgent:       constants.DefaultUserAgent,
	}
}

type Client struct {
	logger     *logging.Logger
	httpClient *http.Client
	config     *Config
}

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

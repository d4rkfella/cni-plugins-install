package http

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/constants"
	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"errors" // Added standard errors

	pkgErrors "github.com/darkfella/cni-plugins-install/pkg/errors" // Alias local errors package
)

func TestNewClient(t *testing.T) {
	logger := logging.NewLogger()
	baseClient := &http.Client{Timeout: 10 * time.Second}

	t.Run("DefaultConfig", func(t *testing.T) {
		c := NewClient(logger, baseClient, nil)
		require.NotNil(t, c)
		assert.Equal(t, logger, c.logger)
		assert.Equal(t, baseClient, c.httpClient)
		require.NotNil(t, c.config)
		assert.Equal(t, DefaultConfig().DownloadTimeout, c.config.DownloadTimeout)
		assert.Equal(t, DefaultConfig().MaxRetries, c.config.MaxRetries)
		assert.Equal(t, DefaultConfig().BufferSize, c.config.BufferSize)
		assert.Equal(t, constants.DefaultUserAgent, c.config.UserAgent)
		assert.Empty(t, c.config.BaseURL)
	})

	t.Run("CustomConfig", func(t *testing.T) {
		customConfig := &Config{
			BaseURL:         "http://custom.example.com",
			DownloadTimeout: 60 * time.Second,
			MaxRetries:      5,
			BufferSize:      64 * 1024,
			UserAgent:       "custom-agent/1.0",
		}
		c := NewClient(logger, baseClient, customConfig)
		require.NotNil(t, c)
		assert.Equal(t, logger, c.logger)
		assert.Equal(t, baseClient, c.httpClient)
		require.NotNil(t, c.config)
		assert.Equal(t, customConfig.DownloadTimeout, c.config.DownloadTimeout)
		assert.Equal(t, customConfig.MaxRetries, c.config.MaxRetries)
		assert.Equal(t, customConfig.BufferSize, c.config.BufferSize)
		assert.Equal(t, customConfig.UserAgent, c.config.UserAgent)
		assert.Equal(t, customConfig.BaseURL, c.config.BaseURL)
	})
}

func TestDownloadFile(t *testing.T) {
	logger := logging.NewLogger()

	t.Run("Success", func(t *testing.T) {
		// Setup mock server
		expectedContent := "Test file content"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, constants.DefaultUserAgent, r.UserAgent())
			w.WriteHeader(http.StatusOK)
			_, err := w.Write([]byte(expectedContent))
			assert.NoError(t, err)
		}))
		defer server.Close()

		// Create client
		// Use the server's client to handle potential redirects etc.
		// Set a reasonable timeout for the test client
		client := NewClient(logger, server.Client(), nil)

		// Download
		var buffer bytes.Buffer
		err := client.DownloadFile(context.Background(), server.URL, &buffer)

		// Verify
		require.NoError(t, err)
		assert.Equal(t, expectedContent, buffer.String())
	})

	t.Run("ErrorNotFound", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound) // Return 404
		}))
		defer server.Close()

		client := NewClient(logger, server.Client(), nil)
		var buffer bytes.Buffer
		// Reinstate short timeout
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		err := client.DownloadFile(ctx, server.URL, &buffer)

		require.Error(t, err)

		// Check for specific HTTPError type
		var httpErr *pkgErrors.HTTPError
		require.True(t, errors.As(err, &httpErr), "Error should be an *errors.HTTPError")
		if httpErr != nil { // Check if assertion succeeded before accessing fields
			assert.Equal(t, http.StatusNotFound, httpErr.StatusCode)
			assert.Contains(t, httpErr.Status, "404 Not Found")
			assert.Equal(t, server.URL, httpErr.URL)
		}
		// Non-retryable errors should not wrap retry errors
		assert.NotContains(t, err.Error(), "after attempts")

		assert.Empty(t, buffer.Bytes(), "Buffer should be empty on error")
	})

	t.Run("RetrySuccess", func(t *testing.T) {
		var requestCount int32 = 0
		expectedContent := "Finally worked"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count := atomic.AddInt32(&requestCount, 1)
			if count <= 2 { // Fail first 2 attempts
				w.WriteHeader(http.StatusServiceUnavailable) // 503
			} else { // Succeed on the 3rd attempt
				w.WriteHeader(http.StatusOK)
				_, err := w.Write([]byte(expectedContent))
				assert.NoError(t, err)
			}
		}))
		defer server.Close()

		// Use config with specific retry count
		config := DefaultConfig()
		config.MaxRetries = 3 // Allow up to 3 retries (so 4 attempts total)
		client := NewClient(logger, server.Client(), config)

		var buffer bytes.Buffer
		err := client.DownloadFile(context.Background(), server.URL, &buffer)

		require.NoError(t, err)
		assert.Equal(t, expectedContent, buffer.String())
		assert.Equal(t, int32(3), requestCount, "Expected 3 requests (1 initial + 2 retries)")
	})

	t.Run("RetryFailure", func(t *testing.T) {
		var requestCount int32 = 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&requestCount, 1)
			w.WriteHeader(http.StatusServiceUnavailable) // Always fail
		}))
		defer server.Close()

		// Use config with specific retry count
		config := DefaultConfig()
		config.MaxRetries = 2 // Allow up to 2 retries (3 attempts total)
		client := NewClient(logger, server.Client(), config)

		var buffer bytes.Buffer
		// Reinstate timeout (make it longer to allow for retries)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // Longer timeout
		defer cancel()
		err := client.DownloadFile(ctx, server.URL, &buffer) // Use background context

		require.Error(t, err)

		// The final error should be the retry error wrapping the HTTPError
		assert.Contains(t, err.Error(), fmt.Sprintf("after %d attempts", config.MaxRetries))

		var httpErr *pkgErrors.HTTPError
		require.True(t, errors.As(err, &httpErr), "Error should wrap an *errors.HTTPError")
		if httpErr != nil {
			assert.Equal(t, http.StatusServiceUnavailable, httpErr.StatusCode)
		}

		assert.Equal(t, int32(config.MaxRetries), requestCount, "Expected requests = MaxRetries")
		assert.Empty(t, buffer.Bytes())
	})

	t.Run("ContextCancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate a delay
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			_, err := w.Write([]byte("should not be written"))
			assert.NoError(t, err)
		}))
		defer server.Close()

		client := NewClient(logger, server.Client(), nil)
		var buffer bytes.Buffer

		// Create a context that cancels quickly
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond) // Cancel before server responds
		defer cancel()

		err := client.DownloadFile(ctx, server.URL, &buffer)

		require.Error(t, err)
		// Check if the error is context deadline exceeded or cancelled
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded, "Context error should be DeadlineExceeded")
		assert.ErrorIs(t, err, context.DeadlineExceeded, "Expected error to be context.DeadlineExceeded")

		// // The underlying error might be wrapped, check contains as well
		// assert.Contains(t, err.Error(), context.DeadlineExceeded.Error())
		assert.Empty(t, buffer.Bytes())
	})

	// More sub-tests here...
}

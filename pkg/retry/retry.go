package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/logging"
	pkgErrors "github.com/darkfella/cni-plugins-install/pkg/errors"
)

// Config represents the retry configuration
type Config struct {
	MaxRetries int
	Logger     *logging.Logger
}

// WithRetry retries an operation with exponential backoff
func WithRetry(ctx context.Context, cfg *Config, fn func(int) error) error {
	if ctx == nil {
		return fmt.Errorf("context cannot be nil")
	}
	if cfg == nil {
		return fmt.Errorf("config cannot be nil")
	}
	if fn == nil {
		return fmt.Errorf("retry function cannot be nil")
	}

	var lastErr error

	for attempt := 0; attempt < cfg.MaxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := fn(attempt)
		if err == nil {
			return nil
		}

		// Check if the error is retryable
		var httpErr *pkgErrors.HTTPError
		retryable := true // Default to retryable for non-HTTP errors (e.g., network)
		if errors.As(err, &httpErr) {
			// If it's an HTTP error, check the status code
			retryable = httpErr.IsRetryable()
		}

		// Check for context errors explicitly as well
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			retryable = false
		}

		lastErr = err // Store the last error regardless

		if !retryable {
			cfg.Logger.Warn().
				Err(err).
				Msg("Operation failed with non-retryable error")
			return lastErr // Return the non-retryable error immediately
		}

		// If retryable, log and sleep
		cfg.Logger.Warn().
			Err(err).
			Int("attempt", attempt+1).
			Int("max_attempts", cfg.MaxRetries).
			Msg("Operation failed, retrying")

		sleepWithJitter(attempt)
	}

	return fmt.Errorf("after %d attempts: %w", cfg.MaxRetries, lastErr)
}

// sleepWithJitter adds jitter to the sleep duration
func sleepWithJitter(attempt int) {
	base := time.Second * time.Duration(1<<attempt)
	jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
	time.Sleep(base + jitter)
}

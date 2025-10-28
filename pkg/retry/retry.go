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

type Config struct {
	MaxRetries int
	Logger     *logging.Logger
}

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

		var httpErr *pkgErrors.HTTPError
		retryable := true
		if errors.As(err, &httpErr) {
			retryable = httpErr.IsRetryable()
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			retryable = false
		}

		lastErr = err

		if !retryable {
			cfg.Logger.Warn().
				Err(err).
				Msg("Operation failed with non-retryable error")
			return lastErr
		}

		cfg.Logger.Warn().
			Err(err).
			Int("attempt", attempt+1).
			Int("max_attempts", cfg.MaxRetries).
			Msg("Operation failed, retrying")

		sleepWithJitter(attempt)
	}

	return fmt.Errorf("after %d attempts: %w", cfg.MaxRetries, lastErr)
}

func sleepWithJitter(attempt int) {
	base := time.Second * time.Duration(1<<attempt)
	jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
	time.Sleep(base + jitter)
}

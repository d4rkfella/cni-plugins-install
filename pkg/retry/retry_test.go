package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/logging"
)

func TestWithRetry(t *testing.T) {
	logger := logging.NewLogger()

	tests := []struct {
		name       string
		maxRetries int
		fn         func(int) error
		wantErr    bool
		setup      func() (context.Context, context.CancelFunc)
	}{
		{
			name:       "successful operation",
			maxRetries: 3,
			fn: func(attempt int) error {
				return nil
			},
			wantErr: false,
			setup: func() (context.Context, context.CancelFunc) {
				return context.Background(), func() {}
			},
		},
		{
			name:       "successful after retry",
			maxRetries: 3,
			fn: func(attempt int) error {
				if attempt < 1 {
					return errors.New("temporary error")
				}
				return nil
			},
			wantErr: false,
			setup: func() (context.Context, context.CancelFunc) {
				return context.Background(), func() {}
			},
		},
		{
			name:       "max retries exceeded",
			maxRetries: 2,
			fn: func(attempt int) error {
				return errors.New("persistent error")
			},
			wantErr: true,
			setup: func() (context.Context, context.CancelFunc) {
				return context.Background(), func() {}
			},
		},
		{
			name:       "context cancelled",
			maxRetries: 3,
			fn: func(attempt int) error {
				return errors.New("should not reach max retries")
			},
			wantErr: true,
			setup: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					time.Sleep(100 * time.Millisecond)
					cancel()
				}()
				return ctx, cancel
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := tt.setup()
			defer cancel()

			cfg := &Config{
				MaxRetries: tt.maxRetries,
				Logger:     logger,
			}

			err := WithRetry(ctx, cfg, tt.fn)
			if (err != nil) != tt.wantErr {
				t.Errorf("WithRetry() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSleepWithJitter(t *testing.T) {
	tests := []struct {
		name        string
		attempt     int
		minDuration time.Duration
		maxDuration time.Duration
	}{
		{
			name:        "first attempt",
			attempt:     0,
			minDuration: time.Second,
			maxDuration: 2 * time.Second,
		},
		{
			name:        "second attempt",
			attempt:     1,
			minDuration: 2 * time.Second,
			maxDuration: 3 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			sleepWithJitter(tt.attempt)
			duration := time.Since(start)

			if duration < tt.minDuration {
				t.Errorf("sleepWithJitter() duration %v is less than minimum %v", duration, tt.minDuration)
			}
			if duration > tt.maxDuration {
				t.Errorf("sleepWithJitter() duration %v is more than maximum %v", duration, tt.maxDuration)
			}
		})
	}
}

func TestWithRetryNilConfig(t *testing.T) {
	err := WithRetry(context.Background(), nil, func(attempt int) error {
		return nil
	})
	if err == nil {
		t.Error("WithRetry() with nil config should return error")
	}
}

func TestWithRetryNilFunction(t *testing.T) {
	cfg := &Config{
		MaxRetries: 3,
		Logger:     logging.NewLogger(),
	}
	err := WithRetry(context.Background(), cfg, nil)
	if err == nil {
		t.Error("WithRetry() with nil function should return error")
	}
}

func TestWithRetryNilContext(t *testing.T) {
	cfg := &Config{
		MaxRetries: 3,
		Logger:     logging.NewLogger(),
	}
	//nolint:staticcheck // Intentional nil context for testing error handling
	err := WithRetry(nil, cfg, func(attempt int) error {
		return nil
	})
	if err == nil {
		t.Error("WithRetry() with nil context should return error")
	}
}

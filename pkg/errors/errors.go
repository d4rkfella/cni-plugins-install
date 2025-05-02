package errors

import (
	"fmt"
)

// OperationError represents an error that occurred during an operation
type OperationError struct {
	Operation string
	File      string
	Cause     error
}

// Error returns the error message
func (e *OperationError) Error() string {
	if e.File != "" {
		return fmt.Sprintf("%s failed for file %s: %v", e.Operation, e.File, e.Cause)
	}
	return fmt.Sprintf("%s failed: %v", e.Operation, e.Cause)
}

// Unwrap returns the underlying error
func (e *OperationError) Unwrap() error {
	return e.Cause
}

// NewOperationError creates a new operation error
func NewOperationError(operation string, cause error) *OperationError {
	return &OperationError{
		Operation: operation,
		Cause:     cause,
	}
}

// Wrap wraps an error with additional context
func Wrap(err error, format string, args ...interface{}) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(format+": %w", append(args, err)...)
}

// HTTPError represents an error returned from an HTTP request
type HTTPError struct {
	StatusCode int
	Status     string
	URL        string
	Method     string
	Message    string
	Cause      error // Optional underlying error
}

// Error returns the formatted error message
func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("HTTP error for %s %s: status %s (%d)", e.Method, e.URL, e.Status, e.StatusCode)
	if e.Message != "" {
		msg += " - " + e.Message
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap returns the underlying cause, if any
func (e *HTTPError) Unwrap() error {
	return e.Cause
}

// IsRetryable checks if the HTTP status code suggests a retry might succeed.
// Typically, 5xx server errors are retryable.
func (e *HTTPError) IsRetryable() bool {
	return e.StatusCode >= 500 && e.StatusCode < 600
}

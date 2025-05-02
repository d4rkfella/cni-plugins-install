package errors

import (
	"errors"
	"fmt"
	"testing"
)

// Tests the OperationError type, focusing on its Error() and Unwrap() methods.
func TestNewOperationError(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		cause     error
		want      string
	}{
		{
			name:      "basic operation error",
			operation: "download",
			cause:     fmt.Errorf("network timeout"),
			want:      "download failed: network timeout",
		},
		{
			name:      "nil cause",
			operation: "validate",
			cause:     nil,
			want:      "validate failed: <nil>",
		},
		{
			name:      "empty operation",
			operation: "",
			cause:     fmt.Errorf("something went wrong"),
			want:      " failed: something went wrong",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewOperationError(tt.operation, tt.cause)
			if err.Error() != tt.want {
				t.Errorf("NewOperationError() error = %v, want %v", err.Error(), tt.want)
			}
			if err.Unwrap() != tt.cause {
				t.Errorf("NewOperationError() unwrapped = %v, want %v", err.Unwrap(), tt.cause)
			}
		})
	}
}

// Tests the FileOperationError type (which embeds OperationError),
// ensuring file path context is included correctly in the error message.
func TestNewFileOperationError(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		file      string
		cause     error
		want      string
	}{
		{
			name:      "basic file operation error",
			operation: "read",
			file:      "/path/to/file.txt",
			cause:     fmt.Errorf("file not found"),
			want:      "read failed for file /path/to/file.txt: file not found",
		},
		{
			name:      "empty file path",
			operation: "write",
			file:      "",
			cause:     fmt.Errorf("permission denied"),
			want:      "write failed: permission denied",
		},
		{
			name:      "nil cause",
			operation: "delete",
			file:      "/tmp/test.txt",
			cause:     nil,
			want:      "delete failed for file /tmp/test.txt: <nil>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewFileOperationError(tt.operation, tt.file, tt.cause)
			if err.Error() != tt.want {
				t.Errorf("NewFileOperationError() error = %v, want %v", err.Error(), tt.want)
			}
			if err.Unwrap() != tt.cause {
				t.Errorf("NewFileOperationError() unwrapped = %v, want %v", err.Unwrap(), tt.cause)
			}
		})
	}
}

// Tests the Wrap function for correctly adding context to errors.
func TestWrap(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		format string
		args   []interface{}
		want   string
		isNil  bool
	}{
		{
			name:   "wrap with format only",
			err:    fmt.Errorf("original error"),
			format: "context info",
			args:   nil,
			want:   "context info: original error",
		},
		{
			name:   "wrap with format and args",
			err:    fmt.Errorf("base error"),
			format: "failed to process %s at %d",
			args:   []interface{}{"file.txt", 42},
			want:   "failed to process file.txt at 42: base error",
		},
		{
			name:   "wrap nil error",
			err:    nil,
			format: "some context",
			args:   nil,
			isNil:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Wrap(tt.err, tt.format, tt.args...)
			if tt.isNil {
				if got != nil {
					t.Errorf("Wrap() = %v, want nil", got)
				}
				return
			}
			if got.Error() != tt.want {
				t.Errorf("Wrap() = %v, want %v", got, tt.want)
			}
			if tt.err != nil {
				unwrapped := errors.Unwrap(got)
				if unwrapped.Error() != tt.err.Error() {
					t.Errorf("Unwrap() = %v, want %v", unwrapped, tt.err)
				}
			}
		})
	}
}

// Tests the HTTPError type, covering its Error(), Unwrap(), and IsRetryable() methods.
func TestHTTPError(t *testing.T) {
	baseErr := fmt.Errorf("underlying network issue")

	tests := []struct {
		name       string
		error      *HTTPError
		wantMsg    string
		wantUnwrap error
		wantRetry  bool
	}{
		{
			name: "404 Not Found",
			error: &HTTPError{
				StatusCode: 404,
				Status:     "Not Found",
				URL:        "http://example.com/resource",
				Method:     "GET",
				Message:    "Resource does not exist",
			},
			wantMsg:    "HTTP error for GET http://example.com/resource: status Not Found (404) - Resource does not exist",
			wantUnwrap: nil,
			wantRetry:  false,
		},
		{
			name: "500 Internal Server Error with Cause",
			error: &HTTPError{
				StatusCode: 500,
				Status:     "Internal Server Error",
				URL:        "http://api.example.com/process",
				Method:     "POST",
				Cause:      baseErr,
			},
			wantMsg:    "HTTP error for POST http://api.example.com/process: status Internal Server Error (500): underlying network issue",
			wantUnwrap: baseErr,
			wantRetry:  true,
		},
		{
			name: "503 Service Unavailable without Message/Cause",
			error: &HTTPError{
				StatusCode: 503,
				Status:     "Service Unavailable",
				URL:        "http://service.example.com/health",
				Method:     "HEAD",
			},
			wantMsg:    "HTTP error for HEAD http://service.example.com/health: status Service Unavailable (503)",
			wantUnwrap: nil,
			wantRetry:  true,
		},
		{
			name: "400 Bad Request (non-retryable)",
			error: &HTTPError{
				StatusCode: 400,
				Status:     "Bad Request",
				URL:        "http://example.com/submit",
				Method:     "PUT",
				Message:    "Invalid input data",
			},
			wantMsg:    "HTTP error for PUT http://example.com/submit: status Bad Request (400) - Invalid input data",
			wantUnwrap: nil,
			wantRetry:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test Error() method
			if msg := tt.error.Error(); msg != tt.wantMsg {
				t.Errorf("HTTPError.Error()\n got: %q\nwant: %q", msg, tt.wantMsg)
			}

			// Test Unwrap() method
			if unwrapped := tt.error.Unwrap(); unwrapped != tt.wantUnwrap {
				t.Errorf("HTTPError.Unwrap() = %v, want %v", unwrapped, tt.wantUnwrap)
			}

			// Test IsRetryable() method
			if retryable := tt.error.IsRetryable(); retryable != tt.wantRetry {
				t.Errorf("HTTPError.IsRetryable() = %v, want %v", retryable, tt.wantRetry)
			}

			// Also test errors.Unwrap
			if unwrappedStd := errors.Unwrap(tt.error); unwrappedStd != tt.wantUnwrap {
				t.Errorf("errors.Unwrap(HTTPError) = %v, want %v", unwrappedStd, tt.wantUnwrap)
			}
		})
	}
}

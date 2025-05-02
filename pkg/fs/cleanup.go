package fs

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/darkfella/cni-plugins-install/internal/logging"
	"github.com/darkfella/cni-plugins-install/pkg/errors"
	"github.com/darkfella/cni-plugins-install/pkg/validator"
)

// CleanupItem represents a single item to be cleaned up
type CleanupItem struct {
	Path      string
	Type      string // "file", "directory", "temp"
	CreatedAt time.Time
	Priority  int // Higher priority items are cleaned up first
}

// CleanupResult represents the result of a cleanup operation
type CleanupResult struct {
	Path    string
	Success bool
	Error   error
}

// Cleanup represents a cleanup handler
type Cleanup struct {
	mu           sync.Mutex
	items        []CleanupItem
	tempDir      string
	logger       *logging.Logger
	validator    *validator.Validator
	fileSystem   FileSystem
	results      []CleanupResult
	cleanupTime  time.Duration
	cleanupCount int
}

// NewCleanup creates a new cleanup handler
func NewCleanup(logger *logging.Logger) *Cleanup {
	return &Cleanup{
		logger:    logger,
		validator: validator.NewValidator(logger),
		items:     make([]CleanupItem, 0),
		results:   make([]CleanupResult, 0),
	}
}

// AddDirectory adds a directory to be cleaned up
func (c *Cleanup) AddDirectory(path string) {
	c.AddItem(path, "directory", 0)
}

// AddItem adds an item to be cleaned up with specified type and priority
func (c *Cleanup) AddItem(path, itemType string, priority int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	item := CleanupItem{
		Path:      path,
		Type:      itemType,
		CreatedAt: time.Now(),
		Priority:  priority,
	}

	c.items = append(c.items, item)
	c.logger.Debug().
		Str("path", path).
		Str("type", itemType).
		Int("priority", priority).
		Msg("Added item to cleanup queue")
}

// Execute performs the cleanup
func (c *Cleanup) Execute(fs FileSystem) error {
	return c.ExecuteWithContext(context.Background(), fs)
}

// ExecuteWithContext performs the cleanup with context support
func (c *Cleanup) ExecuteWithContext(ctx context.Context, fs FileSystem) error {
	if fs == nil {
		return errors.NewOperationError("cleanup", fmt.Errorf("nil filesystem provided"))
	}

	c.mu.Lock()
	if c.fileSystem == nil {
		c.fileSystem = fs
	}
	items := append([]CleanupItem(nil), c.items...)
	tempDir := c.tempDir
	// Clear previous results
	c.results = make([]CleanupResult, 0)
	c.mu.Unlock()

	startTime := time.Now()
	successCount := 0
	errorCount := 0

	// Sort items by priority (higher priority first)
	sortCleanupItems(items)

	for _, item := range items {
		select {
		case <-ctx.Done():
			c.logger.Warn().Msg("Cleanup operation cancelled")
			return ctx.Err()
		default:
		}

		result := c.cleanupItem(ctx, item)
		c.mu.Lock()
		c.results = append(c.results, result)
		c.mu.Unlock()

		if result.Success {
			successCount++
		} else {
			errorCount++
			c.logger.Error().
				Err(result.Error).
				Str("path", result.Path).
				Msg("Failed to clean up item")
		}
	}

	if tempDir != "" {
		c.logger.Debug().Str("dir", tempDir).Msg("Cleaning temp directory")
		result := c.cleanupItem(ctx, CleanupItem{
			Path: tempDir,
			Type: "directory",
		})
		c.mu.Lock()
		c.results = append(c.results, result)
		c.mu.Unlock()
		if result.Success {
			successCount++
		} else {
			errorCount++
		}
	}

	c.mu.Lock()
	c.cleanupTime = time.Since(startTime)
	c.cleanupCount = len(c.results)
	c.mu.Unlock()

	c.logger.Info().
		Int("total", len(c.results)).
		Int("success", successCount).
		Int("errors", errorCount).
		Dur("duration", c.cleanupTime).
		Msg("Cleanup completed")

	// Return an error only if one or more cleanup operations failed.
	if errorCount > 0 {
		return errors.NewOperationError("cleanup", fmt.Errorf("failed to clean up %d items", errorCount))
	}

	return nil
}

// cleanupItem cleans up a single item
func (c *Cleanup) cleanupItem(ctx context.Context, item CleanupItem) CleanupResult {
	result := CleanupResult{
		Path: item.Path,
	}

	// Check context cancellation
	select {
	case <-ctx.Done():
		result.Error = ctx.Err()
		return result
	default:
	}

	// Validate path
	if err := c.validator.ValidatePath(item.Path); err != nil {
		result.Error = err
		return result
	}

	// Check if item exists
	if !c.fileSystem.FileExists(item.Path) {
		result.Success = true
		return result
	}

	// Check file permissions
	info, err := os.Stat(item.Path)
	if err != nil {
		result.Error = errors.Wrap(err, "failed to get file info")
		return result
	}

	// Check if file is writable
	if info.Mode().Perm()&0200 == 0 {
		result.Error = errors.NewOperationError("cleanup", fmt.Errorf("file is not writable: %s", item.Path))
		return result
	}

	// Perform cleanup based on item type
	switch item.Type {
	case "directory":
		err = c.fileSystem.RemoveDirectory(item.Path)
	case "file", "temp":
		err = c.fileSystem.SecureRemove(item.Path)
	default:
		err = fmt.Errorf("unknown item type: %s", item.Type)
	}

	if err != nil {
		result.Error = errors.Wrap(err, fmt.Sprintf("failed to clean up %s", item.Type))
		return result
	}

	result.Success = true
	return result
}

// sortCleanupItems sorts cleanup items by priority (higher priority first)
func sortCleanupItems(items []CleanupItem) {
	for i := 0; i < len(items)-1; i++ {
		for j := i + 1; j < len(items); j++ {
			if items[i].Priority < items[j].Priority {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
}

// Package logging provides structured logging using Go's slog package.
// It offers consistent, context-aware logging across the P2P sync engine.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Log levels
const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// Context keys for request tracing
type contextKey string

const (
	PeerIDKey    contextKey = "peer_id"
	SessionIDKey contextKey = "session_id"
	FilePathKey  contextKey = "file_path"
	ChunkHashKey contextKey = "chunk_hash"
)

var (
	defaultLogger *slog.Logger
	loggerMu      sync.RWMutex
)

func init() {
	// Initialize with default text handler
	defaultLogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: LevelInfo,
	}))
}

// Config holds logging configuration.
type Config struct {
	Level      slog.Level
	Format     string // "json" or "text"
	Output     io.Writer
	AddSource  bool
	TimeFormat string
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Level:      LevelInfo,
		Format:     "text",
		Output:     os.Stdout,
		AddSource:  false,
		TimeFormat: time.RFC3339,
	}
}

// Init initializes the global logger with the given configuration.
func Init(cfg *Config) {
	if cfg == nil {
		cfg = DefaultConfig()
	}

	opts := &slog.HandlerOptions{
		Level:     cfg.Level,
		AddSource: cfg.AddSource,
	}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(cfg.Output, opts)
	} else {
		handler = slog.NewTextHandler(cfg.Output, opts)
	}

	loggerMu.Lock()
	defaultLogger = slog.New(handler)
	loggerMu.Unlock()
}

// Logger returns the default logger.
func Logger() *slog.Logger {
	loggerMu.RLock()
	defer loggerMu.RUnlock()
	return defaultLogger
}

// WithContext creates a logger with values from context.
func WithContext(ctx context.Context) *slog.Logger {
	logger := Logger()

	if peerID, ok := ctx.Value(PeerIDKey).(string); ok {
		logger = logger.With("peer_id", peerID)
	}
	if sessionID, ok := ctx.Value(SessionIDKey).(string); ok {
		logger = logger.With("session_id", sessionID)
	}
	if filePath, ok := ctx.Value(FilePathKey).(string); ok {
		logger = logger.With("file_path", filePath)
	}
	if chunkHash, ok := ctx.Value(ChunkHashKey).(string); ok {
		logger = logger.With("chunk_hash", chunkHash)
	}

	return logger
}

// WithPeerID adds peer ID to context.
func WithPeerID(ctx context.Context, peerID string) context.Context {
	return context.WithValue(ctx, PeerIDKey, peerID)
}

// WithSessionID adds session ID to context.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, SessionIDKey, sessionID)
}

// WithFilePath adds file path to context.
func WithFilePath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, FilePathKey, path)
}

// WithChunkHash adds chunk hash to context.
func WithChunkHash(ctx context.Context, hash string) context.Context {
	return context.WithValue(ctx, ChunkHashKey, hash)
}

// Convenience functions that use the default logger

// Debug logs at debug level.
func Debug(msg string, args ...any) {
	Logger().Debug(msg, args...)
}

// Info logs at info level.
func Info(msg string, args ...any) {
	Logger().Info(msg, args...)
}

// Warn logs at warning level.
func Warn(msg string, args ...any) {
	Logger().Warn(msg, args...)
}

// Error logs at error level.
func Error(msg string, args ...any) {
	Logger().Error(msg, args...)
}

// DebugContext logs at debug level with context.
func DebugContext(ctx context.Context, msg string, args ...any) {
	WithContext(ctx).Debug(msg, args...)
}

// InfoContext logs at info level with context.
func InfoContext(ctx context.Context, msg string, args ...any) {
	WithContext(ctx).Info(msg, args...)
}

// WarnContext logs at warning level with context.
func WarnContext(ctx context.Context, msg string, args ...any) {
	WithContext(ctx).Warn(msg, args...)
}

// ErrorContext logs at error level with context.
func ErrorContext(ctx context.Context, msg string, args ...any) {
	WithContext(ctx).Error(msg, args...)
}

// Component returns a logger for a specific component.
func Component(name string) *slog.Logger {
	return Logger().With("component", name)
}

// Common component loggers
var (
	CASLogger     = func() *slog.Logger { return Component("cas") }
	MerkleLogger  = func() *slog.Logger { return Component("merkle") }
	NetLogger     = func() *slog.Logger { return Component("net") }
	SyncLogger    = func() *slog.Logger { return Component("sync") }
	WatcherLogger = func() *slog.Logger { return Component("watcher") }
	DeltaLogger   = func() *slog.Logger { return Component("delta") }
	NATLogger     = func() *slog.Logger { return Component("nat") }
)

// Operation creates a logger for tracking a specific operation with timing.
type Operation struct {
	logger    *slog.Logger
	name      string
	startTime time.Time
	attrs     []any
}

// StartOperation begins tracking an operation.
func StartOperation(name string, attrs ...any) *Operation {
	op := &Operation{
		logger:    Logger(),
		name:      name,
		startTime: time.Now(),
		attrs:     attrs,
	}
	op.logger.Info("operation started",
		append([]any{"operation", name}, attrs...)...)
	return op
}

// Success marks the operation as successful.
func (o *Operation) Success(attrs ...any) {
	duration := time.Since(o.startTime)
	allAttrs := append([]any{
		"operation", o.name,
		"status", "success",
		"duration_ms", duration.Milliseconds(),
	}, o.attrs...)
	allAttrs = append(allAttrs, attrs...)
	o.logger.Info("operation completed", allAttrs...)
}

// Failure marks the operation as failed.
func (o *Operation) Failure(err error, attrs ...any) {
	duration := time.Since(o.startTime)
	allAttrs := append([]any{
		"operation", o.name,
		"status", "failure",
		"duration_ms", duration.Milliseconds(),
		"error", err.Error(),
	}, o.attrs...)
	allAttrs = append(allAttrs, attrs...)
	o.logger.Error("operation failed", allAttrs...)
}

// SyncEvent represents a sync-related event for structured logging.
type SyncEvent struct {
	Type         string    `json:"type"`
	PeerID       string    `json:"peer_id,omitempty"`
	FilePath     string    `json:"file_path,omitempty"`
	ChunksTotal  int       `json:"chunks_total,omitempty"`
	ChunksSynced int       `json:"chunks_synced,omitempty"`
	BytesTotal   int64     `json:"bytes_total,omitempty"`
	BytesSynced  int64     `json:"bytes_synced,omitempty"`
	Duration     int64     `json:"duration_ms,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
}

// LogSyncEvent logs a sync event with structured data.
func LogSyncEvent(event *SyncEvent) {
	event.Timestamp = time.Now()
	SyncLogger().Info("sync event",
		"event_type", event.Type,
		"peer_id", event.PeerID,
		"file_path", event.FilePath,
		"chunks_total", event.ChunksTotal,
		"chunks_synced", event.ChunksSynced,
		"bytes_total", event.BytesTotal,
		"bytes_synced", event.BytesSynced,
		"duration_ms", event.Duration,
	)
}

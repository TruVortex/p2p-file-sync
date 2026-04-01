// Package watcher monitors filesystem changes and triggers re-chunking.
// Uses fsnotify for cross-platform file watching.
package watcher

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"p2p-file-sync/internal/cas"
	"p2p-file-sync/internal/db"
)

// Watcher monitors a directory for changes and triggers re-chunking.
type Watcher struct {
	rootPath  string
	store     *db.Store
	fsWatcher *fsnotify.Watcher

	// Debouncing: accumulate changes before processing
	pending   map[string]struct{}
	pendingMu sync.Mutex
	debounce  time.Duration

	// Callbacks
	onUpdate func(path string, record *db.FileRecord)
	onDelete func(path string)
	onError  func(err error)

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Config holds watcher configuration.
type Config struct {
	RootPath string
	Store    *db.Store
	Debounce time.Duration // Time to wait for more changes before processing

	// Optional callbacks
	OnUpdate func(path string, record *db.FileRecord)
	OnDelete func(path string)
	OnError  func(err error)
}

// New creates a new file watcher.
func New(cfg Config) (*Watcher, error) {
	if cfg.RootPath == "" {
		return nil, fmt.Errorf("root path is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("store is required")
	}

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating fsnotify watcher: %w", err)
	}

	debounce := cfg.Debounce
	if debounce == 0 {
		debounce = 100 * time.Millisecond
	}

	ctx, cancel := context.WithCancel(context.Background())

	w := &Watcher{
		rootPath:  cfg.RootPath,
		store:     cfg.Store,
		fsWatcher: fsWatcher,
		pending:   make(map[string]struct{}),
		debounce:  debounce,
		onUpdate:  cfg.OnUpdate,
		onDelete:  cfg.OnDelete,
		onError:   cfg.OnError,
		ctx:       ctx,
		cancel:    cancel,
	}

	return w, nil
}

// Start begins watching the directory tree.
func (w *Watcher) Start() error {
	// Add root and all subdirectories
	err := filepath.WalkDir(w.rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip hidden directories
			if len(d.Name()) > 0 && d.Name()[0] == '.' && path != w.rootPath {
				return filepath.SkipDir
			}
			if err := w.fsWatcher.Add(path); err != nil {
				return fmt.Errorf("watching %s: %w", path, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking directory tree: %w", err)
	}

	// Start event processing goroutine
	w.wg.Add(1)
	go w.processEvents()

	// Start debounce processing goroutine
	w.wg.Add(1)
	go w.processPending()

	return nil
}

// Stop stops the watcher and waits for goroutines to finish.
func (w *Watcher) Stop() error {
	w.cancel()
	w.wg.Wait()
	return w.fsWatcher.Close()
}

// processEvents reads fsnotify events and queues them for processing.
func (w *Watcher) processEvents() {
	defer w.wg.Done()

	for {
		select {
		case <-w.ctx.Done():
			return

		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}
			w.handleEvent(event)

		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			if w.onError != nil {
				w.onError(err)
			}
		}
	}
}

// handleEvent processes a single fsnotify event.
func (w *Watcher) handleEvent(event fsnotify.Event) {
	// Skip hidden files
	base := filepath.Base(event.Name)
	if len(base) > 0 && base[0] == '.' {
		return
	}

	// Get relative path for storage
	relPath, err := filepath.Rel(w.rootPath, event.Name)
	if err != nil {
		if w.onError != nil {
			w.onError(fmt.Errorf("getting relative path: %w", err))
		}
		return
	}

	// Handle directory creation - need to watch new directories
	if event.Has(fsnotify.Create) {
		info, err := os.Stat(event.Name)
		if err == nil && info.IsDir() {
			w.fsWatcher.Add(event.Name)
			return
		}
	}

	// Queue file for processing
	w.pendingMu.Lock()
	w.pending[relPath] = struct{}{}
	w.pendingMu.Unlock()
}

// processPending processes queued file changes after debounce period.
func (w *Watcher) processPending() {
	defer w.wg.Done()

	ticker := time.NewTicker(w.debounce)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return

		case <-ticker.C:
			w.flushPending()
		}
	}
}

// flushPending processes all pending file changes.
func (w *Watcher) flushPending() {
	w.pendingMu.Lock()
	if len(w.pending) == 0 {
		w.pendingMu.Unlock()
		return
	}

	// Swap pending map
	pending := w.pending
	w.pending = make(map[string]struct{})
	w.pendingMu.Unlock()

	// Process each file
	for relPath := range pending {
		if err := w.processFile(relPath); err != nil {
			if w.onError != nil {
				w.onError(fmt.Errorf("processing %s: %w", relPath, err))
			}
		}
	}
}

// processFile checks if a file needs updating and re-chunks if necessary.
func (w *Watcher) processFile(relPath string) error {
	fullPath := filepath.Join(w.rootPath, relPath)

	// Check if file exists
	info, err := os.Stat(fullPath)
	if os.IsNotExist(err) {
		// File was deleted
		return w.handleDelete(relPath)
	}
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}

	// Skip directories
	if info.IsDir() {
		return nil
	}

	// Check if update is needed (based on ModTime or Size)
	needsUpdate, err := w.store.NeedsUpdate(w.ctx, relPath, info.ModTime(), info.Size())
	if err != nil {
		return fmt.Errorf("checking needs update: %w", err)
	}

	if !needsUpdate {
		return nil
	}

	return w.handleUpdate(relPath, fullPath, info)
}

// handleUpdate re-chunks a file and updates the database.
func (w *Watcher) handleUpdate(relPath, fullPath string, info fs.FileInfo) error {
	// Open and chunk the file
	f, err := os.Open(fullPath)
	if err != nil {
		return fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	chunker := cas.NewChunker(f)
	chunks, err := chunker.ChunkAll()
	if err != nil {
		return fmt.Errorf("chunking file: %w", err)
	}

	// Extract chunk hashes and compute Merkle root
	chunkHashes := make([][]byte, len(chunks))
	chunkSizes := make([]int, len(chunks))
	for i, chunk := range chunks {
		chunkHashes[i] = chunk.Hash
		chunkSizes[i] = int(chunk.Size)
	}

	// Compute file merkle root (hash of all chunk hashes)
	merkleRoot := computeMerkleRoot(chunkHashes)

	// Update database in a transaction
	record := &db.FileRecord{
		Path:       relPath,
		MerkleRoot: merkleRoot,
		Size:       info.Size(),
		ModTime:    info.ModTime(),
		ChunkCount: len(chunks),
	}

	err = w.store.Transaction(w.ctx, func(tx *sql.Tx) error {
		if err := w.store.UpsertFile(w.ctx, tx, record); err != nil {
			return err
		}
		return w.store.SetFileChunks(w.ctx, tx, record.ID, chunkHashes, chunkSizes)
	})
	if err != nil {
		return fmt.Errorf("updating database: %w", err)
	}

	// Notify callback
	if w.onUpdate != nil {
		w.onUpdate(relPath, record)
	}

	return nil
}

// handleDelete removes a file from the database.
func (w *Watcher) handleDelete(relPath string) error {
	err := w.store.Transaction(w.ctx, func(tx *sql.Tx) error {
		return w.store.DeleteFile(w.ctx, tx, relPath)
	})
	if err != nil {
		return fmt.Errorf("deleting from database: %w", err)
	}

	// Notify callback
	if w.onDelete != nil {
		w.onDelete(relPath)
	}

	return nil
}

// computeMerkleRoot computes a simple merkle root from chunk hashes.
func computeMerkleRoot(hashes [][]byte) []byte {
	if len(hashes) == 0 {
		return make([]byte, 32) // Empty hash
	}

	// Simple approach: hash all chunk hashes together
	// A full merkle tree would build bottom-up in pairs
	hasher := cas.NewHasher()
	for _, h := range hashes {
		hasher.Write(h)
	}
	return hasher.Sum(nil)
}

// ScanAll performs an initial scan of all files in the watched directory.
func (w *Watcher) ScanAll() error {
	return filepath.WalkDir(w.rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip hidden files/directories
		if len(d.Name()) > 0 && d.Name()[0] == '.' {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip directories
		if d.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(w.rootPath, path)
		if err != nil {
			return fmt.Errorf("getting relative path: %w", err)
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("getting file info: %w", err)
		}

		// Check if update needed
		needsUpdate, err := w.store.NeedsUpdate(w.ctx, relPath, info.ModTime(), info.Size())
		if err != nil {
			return fmt.Errorf("checking needs update: %w", err)
		}

		if needsUpdate {
			if err := w.handleUpdate(relPath, path, info); err != nil {
				if w.onError != nil {
					w.onError(fmt.Errorf("scanning %s: %w", relPath, err))
				}
			}
		}

		return nil
	})
}

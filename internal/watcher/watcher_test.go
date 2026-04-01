package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"p2p-file-sync/internal/db"
)

func TestWatcher_DetectsNewFile(t *testing.T) {
	tmpDir := t.TempDir()
	watchDir := filepath.Join(tmpDir, "watch")
	if err := os.MkdirAll(watchDir, 0755); err != nil {
		t.Fatalf("creating watch dir: %v", err)
	}

	store, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	var updatedFiles []string

	w, err := New(Config{
		RootPath: watchDir,
		Store:    store,
		Debounce: 50 * time.Millisecond,
		OnUpdate: func(path string, record *db.FileRecord) {
			mu.Lock()
			updatedFiles = append(updatedFiles, path)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("creating watcher: %v", err)
	}

	if err := w.Start(); err != nil {
		t.Fatalf("starting watcher: %v", err)
	}
	defer w.Stop()

	// Create a new file
	testFile := filepath.Join(watchDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello world"), 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	// Wait for debounce + processing
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(updatedFiles) != 1 {
		t.Errorf("expected 1 updated file, got %d", len(updatedFiles))
	}
	if len(updatedFiles) > 0 && updatedFiles[0] != "test.txt" {
		t.Errorf("expected 'test.txt', got '%s'", updatedFiles[0])
	}

	// Verify in database
	record, err := store.GetFile(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("getting file: %v", err)
	}
	if record == nil {
		t.Fatal("expected file record in database")
	}
	if record.Size != 11 {
		t.Errorf("expected size 11, got %d", record.Size)
	}
}

func TestWatcher_IgnoresHiddenFiles(t *testing.T) {
	tmpDir := t.TempDir()
	watchDir := filepath.Join(tmpDir, "watch")
	if err := os.MkdirAll(watchDir, 0755); err != nil {
		t.Fatalf("creating watch dir: %v", err)
	}

	store, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	var updatedFiles []string

	w, err := New(Config{
		RootPath: watchDir,
		Store:    store,
		Debounce: 50 * time.Millisecond,
		OnUpdate: func(path string, record *db.FileRecord) {
			mu.Lock()
			updatedFiles = append(updatedFiles, path)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("creating watcher: %v", err)
	}

	if err := w.Start(); err != nil {
		t.Fatalf("starting watcher: %v", err)
	}
	defer w.Stop()

	// Create a hidden file
	hiddenFile := filepath.Join(watchDir, ".hidden")
	if err := os.WriteFile(hiddenFile, []byte("secret"), 0644); err != nil {
		t.Fatalf("writing hidden file: %v", err)
	}

	// Wait for debounce
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(updatedFiles) != 0 {
		t.Errorf("expected 0 updated files for hidden file, got %d", len(updatedFiles))
	}
}

func TestWatcher_ScanAll(t *testing.T) {
	tmpDir := t.TempDir()
	watchDir := filepath.Join(tmpDir, "watch")
	subDir := filepath.Join(watchDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("creating subdirectory: %v", err)
	}

	// Create files before starting watcher
	files := map[string]string{
		"a.txt":        "content a",
		"b.txt":        "content b",
		"subdir/c.txt": "content c",
	}
	for name, content := range files {
		path := filepath.Join(watchDir, name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	store, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	w, err := New(Config{
		RootPath: watchDir,
		Store:    store,
	})
	if err != nil {
		t.Fatalf("creating watcher: %v", err)
	}

	// Scan all existing files
	if err := w.ScanAll(); err != nil {
		t.Fatalf("scanning all: %v", err)
	}

	// Verify all files in database
	ctx := context.Background()
	for name := range files {
		// Use OS-specific path separator
		osPath := filepath.FromSlash(name)
		record, err := store.GetFile(ctx, osPath)
		if err != nil {
			t.Fatalf("getting %s: %v", osPath, err)
		}
		if record == nil {
			t.Errorf("expected record for %s", osPath)
		}
	}
}

func TestWatcher_OnlyUpdatesWhenChanged(t *testing.T) {
	tmpDir := t.TempDir()
	watchDir := filepath.Join(tmpDir, "watch")
	if err := os.MkdirAll(watchDir, 0755); err != nil {
		t.Fatalf("creating watch dir: %v", err)
	}

	store, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	// Create initial file
	testFile := filepath.Join(watchDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("initial"), 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	w, err := New(Config{
		RootPath: watchDir,
		Store:    store,
		Debounce: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("creating watcher: %v", err)
	}

	// Initial scan
	if err := w.ScanAll(); err != nil {
		t.Fatalf("initial scan: %v", err)
	}

	// Check file exists in DB
	ctx := context.Background()
	record1, err := store.GetFile(ctx, "test.txt")
	if err != nil {
		t.Fatalf("getting file: %v", err)
	}
	if record1 == nil {
		t.Fatal("expected file in DB after first scan")
	}

	// Get the file info for unchanged check
	info, _ := os.Stat(testFile)

	// Verify NeedsUpdate returns false for unchanged file
	needs, err := store.NeedsUpdate(ctx, "test.txt", info.ModTime(), info.Size())
	if err != nil {
		t.Fatalf("checking needs update: %v", err)
	}
	if needs {
		t.Error("unchanged file should not need update")
	}
}

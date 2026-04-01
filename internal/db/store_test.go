package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_OpenClose(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("closing store: %v", err)
	}

	// Verify file was created
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("database file not created: %v", err)
	}
}

func TestStore_UpsertAndGetFile(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	hash := sha256.Sum256([]byte("test content"))
	modTime := time.Now().Truncate(time.Second)

	record := &FileRecord{
		Path:       "/path/to/file.txt",
		MerkleRoot: hash[:],
		Size:       1024,
		ModTime:    modTime,
		ChunkCount: 1,
	}

	// Insert
	err = store.Transaction(ctx, func(tx *sql.Tx) error {
		return store.UpsertFile(ctx, tx, record)
	})
	if err != nil {
		t.Fatalf("upserting file: %v", err)
	}

	if record.ID == 0 {
		t.Error("expected ID to be set after insert")
	}

	// Get
	retrieved, err := store.GetFile(ctx, "/path/to/file.txt")
	if err != nil {
		t.Fatalf("getting file: %v", err)
	}

	if retrieved == nil {
		t.Fatal("expected file record, got nil")
	}
	if retrieved.Path != record.Path {
		t.Errorf("path mismatch: got %s", retrieved.Path)
	}
	if retrieved.Size != record.Size {
		t.Errorf("size mismatch: got %d", retrieved.Size)
	}
}

func TestStore_NeedsUpdate(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	hash := sha256.Sum256([]byte("test"))
	modTime := time.Now().Truncate(time.Second)

	// New file should need update
	needs, err := store.NeedsUpdate(ctx, "/new/file.txt", modTime, 100)
	if err != nil {
		t.Fatalf("checking needs update: %v", err)
	}
	if !needs {
		t.Error("new file should need update")
	}

	// Insert file
	record := &FileRecord{
		Path:       "/new/file.txt",
		MerkleRoot: hash[:],
		Size:       100,
		ModTime:    modTime,
		ChunkCount: 1,
	}
	err = store.Transaction(ctx, func(tx *sql.Tx) error {
		return store.UpsertFile(ctx, tx, record)
	})
	if err != nil {
		t.Fatalf("upserting: %v", err)
	}

	// Same metadata should not need update
	needs, err = store.NeedsUpdate(ctx, "/new/file.txt", modTime, 100)
	if err != nil {
		t.Fatalf("checking needs update: %v", err)
	}
	if needs {
		t.Error("unchanged file should not need update")
	}

	// Changed size should need update
	needs, err = store.NeedsUpdate(ctx, "/new/file.txt", modTime, 200)
	if err != nil {
		t.Fatalf("checking needs update: %v", err)
	}
	if !needs {
		t.Error("file with changed size should need update")
	}

	// Changed modtime should need update
	needs, err = store.NeedsUpdate(ctx, "/new/file.txt", modTime.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("checking needs update: %v", err)
	}
	if !needs {
		t.Error("file with changed modtime should need update")
	}
}

func TestStore_CreateSnapshot(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Add some files first
	for i := 0; i < 3; i++ {
		hash := sha256.Sum256([]byte{byte(i)})
		record := &FileRecord{
			Path:       filepath.Join("/test", string(rune('a'+i))+".txt"),
			MerkleRoot: hash[:],
			Size:       int64(100 * (i + 1)),
			ModTime:    time.Now(),
			ChunkCount: 1,
		}
		err = store.Transaction(ctx, func(tx *sql.Tx) error {
			return store.UpsertFile(ctx, tx, record)
		})
		if err != nil {
			t.Fatalf("upserting file: %v", err)
		}
	}

	// Create snapshot
	rootHash := sha256.Sum256([]byte("root"))
	snapshot, err := store.CreateSnapshot(ctx, rootHash[:], "test snapshot")
	if err != nil {
		t.Fatalf("creating snapshot: %v", err)
	}

	if snapshot.ID == 0 {
		t.Error("expected snapshot ID to be set")
	}
	if snapshot.FileCount != 3 {
		t.Errorf("expected 3 files, got %d", snapshot.FileCount)
	}
	if snapshot.TotalSize != 600 { // 100 + 200 + 300
		t.Errorf("expected total size 600, got %d", snapshot.TotalSize)
	}
	if snapshot.Comment != "test snapshot" {
		t.Errorf("expected comment 'test snapshot', got '%s'", snapshot.Comment)
	}

	// Verify snapshot files
	files, err := store.GetSnapshotFiles(ctx, snapshot.ID)
	if err != nil {
		t.Fatalf("getting snapshot files: %v", err)
	}
	if len(files) != 3 {
		t.Errorf("expected 3 snapshot files, got %d", len(files))
	}
}

func TestStore_ListSnapshots(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create multiple snapshots
	for i := 0; i < 3; i++ {
		hash := sha256.Sum256([]byte{byte(i)})
		_, err := store.CreateSnapshot(ctx, hash[:], "")
		if err != nil {
			t.Fatalf("creating snapshot: %v", err)
		}
	}

	snapshots, err := store.ListSnapshots(ctx)
	if err != nil {
		t.Fatalf("listing snapshots: %v", err)
	}

	if len(snapshots) != 3 {
		t.Errorf("expected 3 snapshots, got %d", len(snapshots))
	}

	// Should be ordered newest first
	for i := 0; i < len(snapshots)-1; i++ {
		if snapshots[i].CreatedAt.Before(snapshots[i+1].CreatedAt) {
			t.Error("snapshots should be ordered newest first")
		}
	}
}

func TestStore_Transaction_Rollback(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	hash := sha256.Sum256([]byte("test"))

	// Transaction that fails
	err = store.Transaction(ctx, func(tx *sql.Tx) error {
		record := &FileRecord{
			Path:       "/rollback/test.txt",
			MerkleRoot: hash[:],
			Size:       100,
			ModTime:    time.Now(),
		}
		if err := store.UpsertFile(ctx, tx, record); err != nil {
			return err
		}
		// Return error to trigger rollback
		return fmt.Errorf("intentional error")
	})

	if err == nil {
		t.Fatal("expected transaction to fail")
	}

	// File should not exist due to rollback
	record, err := store.GetFile(ctx, "/rollback/test.txt")
	if err != nil {
		t.Fatalf("getting file: %v", err)
	}
	if record != nil {
		t.Error("file should not exist after rollback")
	}
}

func TestStore_DeleteFile(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	hash := sha256.Sum256([]byte("test"))

	// Insert file
	record := &FileRecord{
		Path:       "/delete/test.txt",
		MerkleRoot: hash[:],
		Size:       100,
		ModTime:    time.Now(),
	}
	err = store.Transaction(ctx, func(tx *sql.Tx) error {
		return store.UpsertFile(ctx, tx, record)
	})
	if err != nil {
		t.Fatalf("upserting: %v", err)
	}

	// Delete file
	err = store.Transaction(ctx, func(tx *sql.Tx) error {
		return store.DeleteFile(ctx, tx, "/delete/test.txt")
	})
	if err != nil {
		t.Fatalf("deleting: %v", err)
	}

	// Verify deleted
	record, err = store.GetFile(ctx, "/delete/test.txt")
	if err != nil {
		t.Fatalf("getting file: %v", err)
	}
	if record != nil {
		t.Error("file should be deleted")
	}
}

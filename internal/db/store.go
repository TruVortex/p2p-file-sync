// Package db provides SQLite-based metadata storage for file tracking and versioning.
// Uses modernc.org/sqlite for CGO-free operation.
package db

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store manages SQLite database operations for file metadata.
type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

// FileRecord represents a tracked file's metadata.
type FileRecord struct {
	ID         int64
	Path       string
	MerkleRoot []byte
	Size       int64
	ModTime    time.Time
	ChunkCount int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Snapshot represents a point-in-time state of the tracked folder.
type Snapshot struct {
	ID        int64
	RootHash  []byte // Merkle root of entire directory tree
	FileCount int
	TotalSize int64
	CreatedAt time.Time
	Comment   string
}

// SnapshotFile links files to snapshots for version history.
type SnapshotFile struct {
	SnapshotID int64
	FilePath   string
	MerkleRoot []byte
	Size       int64
}

const schema = `
-- Files table: maps file paths to their current Merkle Root hash
CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    merkle_root BLOB NOT NULL,
    size INTEGER NOT NULL,
    mod_time INTEGER NOT NULL,  -- Unix timestamp
    chunk_count INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    updated_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_files_path ON files(path);
CREATE INDEX IF NOT EXISTS idx_files_merkle_root ON files(merkle_root);

-- Snapshots table: stores history of folder state (versioning)
CREATE TABLE IF NOT EXISTS snapshots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    root_hash BLOB NOT NULL,     -- Merkle root of entire directory tree
    file_count INTEGER NOT NULL,
    total_size INTEGER NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now')),
    comment TEXT
);

CREATE INDEX IF NOT EXISTS idx_snapshots_created_at ON snapshots(created_at);
CREATE INDEX IF NOT EXISTS idx_snapshots_root_hash ON snapshots(root_hash);

-- Snapshot files: links files to snapshots for version history
CREATE TABLE IF NOT EXISTS snapshot_files (
    snapshot_id INTEGER NOT NULL,
    file_path TEXT NOT NULL,
    merkle_root BLOB NOT NULL,
    size INTEGER NOT NULL,
    PRIMARY KEY (snapshot_id, file_path),
    FOREIGN KEY (snapshot_id) REFERENCES snapshots(id) ON DELETE CASCADE
);

-- Chunks table: tracks which chunks belong to which files
CREATE TABLE IF NOT EXISTS file_chunks (
    file_id INTEGER NOT NULL,
    chunk_index INTEGER NOT NULL,
    chunk_hash BLOB NOT NULL,
    size INTEGER NOT NULL,
    PRIMARY KEY (file_id, chunk_index),
    FOREIGN KEY (file_id) REFERENCES files(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_file_chunks_hash ON file_chunks(chunk_hash);
`

// Open creates or opens a SQLite database at the given path.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Enable WAL mode for better concurrent performance
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting WAL mode: %w", err)
	}

	// Enable foreign keys
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}

	// Initialize schema
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Transaction executes a function within a database transaction.
// If the function returns an error, the transaction is rolled back.
func (s *Store) Transaction(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("rollback failed: %v (original error: %w)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// UpsertFile inserts or updates a file record.
func (s *Store) UpsertFile(ctx context.Context, tx *sql.Tx, record *FileRecord) error {
	query := `
		INSERT INTO files (path, merkle_root, size, mod_time, chunk_count, updated_at)
		VALUES (?, ?, ?, ?, ?, strftime('%s', 'now'))
		ON CONFLICT(path) DO UPDATE SET
			merkle_root = excluded.merkle_root,
			size = excluded.size,
			mod_time = excluded.mod_time,
			chunk_count = excluded.chunk_count,
			updated_at = strftime('%s', 'now')
		RETURNING id
	`

	var executor interface {
		QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
	}
	if tx != nil {
		executor = tx
	} else {
		executor = s.db
	}

	err := executor.QueryRowContext(ctx, query,
		record.Path,
		record.MerkleRoot,
		record.Size,
		record.ModTime.Unix(),
		record.ChunkCount,
	).Scan(&record.ID)

	if err != nil {
		return fmt.Errorf("upserting file %s: %w", record.Path, err)
	}

	return nil
}

// GetFile retrieves a file record by path.
func (s *Store) GetFile(ctx context.Context, path string) (*FileRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, path, merkle_root, size, mod_time, chunk_count, created_at, updated_at
		FROM files WHERE path = ?
	`

	var record FileRecord
	var modTime, createdAt, updatedAt int64

	err := s.db.QueryRowContext(ctx, query, path).Scan(
		&record.ID,
		&record.Path,
		&record.MerkleRoot,
		&record.Size,
		&modTime,
		&record.ChunkCount,
		&createdAt,
		&updatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting file %s: %w", path, err)
	}

	record.ModTime = time.Unix(modTime, 0)
	record.CreatedAt = time.Unix(createdAt, 0)
	record.UpdatedAt = time.Unix(updatedAt, 0)

	return &record, nil
}

// DeleteFile removes a file record by path.
func (s *Store) DeleteFile(ctx context.Context, tx *sql.Tx, path string) error {
	query := `DELETE FROM files WHERE path = ?`

	var executor interface {
		ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	}
	if tx != nil {
		executor = tx
	} else {
		executor = s.db
	}

	_, err := executor.ExecContext(ctx, query, path)
	if err != nil {
		return fmt.Errorf("deleting file %s: %w", path, err)
	}

	return nil
}

// ListFiles returns all tracked files.
func (s *Store) ListFiles(ctx context.Context) ([]*FileRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, path, merkle_root, size, mod_time, chunk_count, created_at, updated_at
		FROM files ORDER BY path
	`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing files: %w", err)
	}
	defer rows.Close()

	var records []*FileRecord
	for rows.Next() {
		var record FileRecord
		var modTime, createdAt, updatedAt int64

		if err := rows.Scan(
			&record.ID,
			&record.Path,
			&record.MerkleRoot,
			&record.Size,
			&modTime,
			&record.ChunkCount,
			&createdAt,
			&updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning file row: %w", err)
		}

		record.ModTime = time.Unix(modTime, 0)
		record.CreatedAt = time.Unix(createdAt, 0)
		record.UpdatedAt = time.Unix(updatedAt, 0)
		records = append(records, &record)
	}

	return records, rows.Err()
}

// SetFileChunks stores the chunk hashes for a file.
func (s *Store) SetFileChunks(ctx context.Context, tx *sql.Tx, fileID int64, chunks [][]byte, sizes []int) error {
	// Delete existing chunks
	deleteQuery := `DELETE FROM file_chunks WHERE file_id = ?`
	if _, err := tx.ExecContext(ctx, deleteQuery, fileID); err != nil {
		return fmt.Errorf("deleting old chunks: %w", err)
	}

	// Insert new chunks
	insertQuery := `INSERT INTO file_chunks (file_id, chunk_index, chunk_hash, size) VALUES (?, ?, ?, ?)`
	for i, hash := range chunks {
		size := 4096
		if i < len(sizes) {
			size = sizes[i]
		}
		if _, err := tx.ExecContext(ctx, insertQuery, fileID, i, hash, size); err != nil {
			return fmt.Errorf("inserting chunk %d: %w", i, err)
		}
	}

	return nil
}

// CreateSnapshot creates a new snapshot of the current state.
func (s *Store) CreateSnapshot(ctx context.Context, rootHash []byte, comment string) (*Snapshot, error) {
	var snapshot Snapshot

	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		// Get current file stats
		var fileCount int
		var totalSize int64
		statsQuery := `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files`
		if err := tx.QueryRowContext(ctx, statsQuery).Scan(&fileCount, &totalSize); err != nil {
			return fmt.Errorf("getting file stats: %w", err)
		}

		// Insert snapshot
		insertQuery := `
			INSERT INTO snapshots (root_hash, file_count, total_size, comment)
			VALUES (?, ?, ?, ?)
			RETURNING id, created_at
		`
		var createdAt int64
		if err := tx.QueryRowContext(ctx, insertQuery, rootHash, fileCount, totalSize, comment).
			Scan(&snapshot.ID, &createdAt); err != nil {
			return fmt.Errorf("inserting snapshot: %w", err)
		}

		snapshot.RootHash = rootHash
		snapshot.FileCount = fileCount
		snapshot.TotalSize = totalSize
		snapshot.CreatedAt = time.Unix(createdAt, 0)
		snapshot.Comment = comment

		// Copy current files to snapshot_files
		copyQuery := `
			INSERT INTO snapshot_files (snapshot_id, file_path, merkle_root, size)
			SELECT ?, path, merkle_root, size FROM files
		`
		if _, err := tx.ExecContext(ctx, copyQuery, snapshot.ID); err != nil {
			return fmt.Errorf("copying files to snapshot: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return &snapshot, nil
}

// GetSnapshot retrieves a snapshot by ID.
func (s *Store) GetSnapshot(ctx context.Context, id int64) (*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, root_hash, file_count, total_size, created_at, comment
		FROM snapshots WHERE id = ?
	`

	var snapshot Snapshot
	var createdAt int64
	var comment sql.NullString

	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&snapshot.ID,
		&snapshot.RootHash,
		&snapshot.FileCount,
		&snapshot.TotalSize,
		&createdAt,
		&comment,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting snapshot %d: %w", id, err)
	}

	snapshot.CreatedAt = time.Unix(createdAt, 0)
	if comment.Valid {
		snapshot.Comment = comment.String
	}

	return &snapshot, nil
}

// ListSnapshots returns all snapshots, newest first.
func (s *Store) ListSnapshots(ctx context.Context) ([]*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, root_hash, file_count, total_size, created_at, comment
		FROM snapshots ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing snapshots: %w", err)
	}
	defer rows.Close()

	var snapshots []*Snapshot
	for rows.Next() {
		var snapshot Snapshot
		var createdAt int64
		var comment sql.NullString

		if err := rows.Scan(
			&snapshot.ID,
			&snapshot.RootHash,
			&snapshot.FileCount,
			&snapshot.TotalSize,
			&createdAt,
			&comment,
		); err != nil {
			return nil, fmt.Errorf("scanning snapshot row: %w", err)
		}

		snapshot.CreatedAt = time.Unix(createdAt, 0)
		if comment.Valid {
			snapshot.Comment = comment.String
		}
		snapshots = append(snapshots, &snapshot)
	}

	return snapshots, rows.Err()
}

// GetSnapshotFiles returns all files in a snapshot.
func (s *Store) GetSnapshotFiles(ctx context.Context, snapshotID int64) ([]*SnapshotFile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT snapshot_id, file_path, merkle_root, size
		FROM snapshot_files WHERE snapshot_id = ? ORDER BY file_path
	`

	rows, err := s.db.QueryContext(ctx, query, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("getting snapshot files: %w", err)
	}
	defer rows.Close()

	var files []*SnapshotFile
	for rows.Next() {
		var sf SnapshotFile
		if err := rows.Scan(&sf.SnapshotID, &sf.FilePath, &sf.MerkleRoot, &sf.Size); err != nil {
			return nil, fmt.Errorf("scanning snapshot file: %w", err)
		}
		files = append(files, &sf)
	}

	return files, rows.Err()
}

// NeedsUpdate checks if a file needs re-processing based on ModTime or Size.
func (s *Store) NeedsUpdate(ctx context.Context, path string, modTime time.Time, size int64) (bool, error) {
	record, err := s.GetFile(ctx, path)
	if err != nil {
		return false, err
	}

	// New file
	if record == nil {
		return true, nil
	}

	// Check if metadata changed
	// Compare Unix timestamps (second precision) to handle sub-second differences
	if record.Size != size || record.ModTime.Unix() != modTime.Unix() {
		return true, nil
	}

	return false, nil
}

// MerkleRootHex returns hex string of a merkle root.
func MerkleRootHex(hash []byte) string {
	return hex.EncodeToString(hash)
}

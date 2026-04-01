package cas

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	// DefaultStorageDir is the default root directory for chunk storage.
	DefaultStorageDir = ".storage/chunks"

	// PrefixLength is the number of hex characters used for subdirectory naming.
	// Using 2 chars gives us 256 subdirectories (00-ff) to avoid folder bloat.
	PrefixLength = 2

	// FilePermissions for chunk files (read/write for owner only).
	FilePermissions = 0600

	// DirPermissions for chunk directories.
	DirPermissions = 0700

	// BufferSize for buffered I/O operations (64KB for optimal disk throughput).
	BufferSize = 64 * 1024
)

// BlobStore manages persistent storage of content-addressed chunks.
type BlobStore struct {
	baseDir string
	mu      sync.RWMutex // Protects concurrent access to the same chunk
}

// NewBlobStore creates a BlobStore rooted at the given directory.
// If dir is empty, uses DefaultStorageDir.
func NewBlobStore(dir string) (*BlobStore, error) {
	if dir == "" {
		dir = DefaultStorageDir
	}

	// Ensure base directory exists
	if err := os.MkdirAll(dir, DirPermissions); err != nil {
		return nil, fmt.Errorf("creating storage directory: %w", err)
	}

	return &BlobStore{baseDir: dir}, nil
}

// chunkPath computes the filesystem path for a chunk hash.
// Format: baseDir/ab/cdef0123... (first 2 chars as subdir)
func (s *BlobStore) chunkPath(hashHex string) string {
	if len(hashHex) < PrefixLength {
		return filepath.Join(s.baseDir, hashHex)
	}
	prefix := hashHex[:PrefixLength]
	suffix := hashHex[PrefixLength:]
	return filepath.Join(s.baseDir, prefix, suffix)
}

// Put stores a chunk to disk. Returns true if the chunk was newly written,
// false if it already existed (deduplication).
func (s *BlobStore) Put(chunk *Chunk) (bool, error) {
	hashHex := chunk.HashHex()
	path := s.chunkPath(hashHex)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if chunk already exists (deduplication)
	if _, err := os.Stat(path); err == nil {
		return false, nil // Already exists, skip write
	}

	// Ensure subdirectory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirPermissions); err != nil {
		return false, fmt.Errorf("creating chunk directory %s: %w", dir, err)
	}

	// Write atomically: write to temp file, then rename
	tmpPath := path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, FilePermissions)
	if err != nil {
		return false, fmt.Errorf("creating temp file: %w", err)
	}

	// Use buffered writer for high-performance I/O
	writer := bufio.NewWriterSize(f, BufferSize)

	_, writeErr := writer.Write(chunk.Data)
	flushErr := writer.Flush()
	closeErr := f.Close()

	// Handle errors in order of priority
	if writeErr != nil {
		os.Remove(tmpPath)
		return false, fmt.Errorf("writing chunk data: %w", writeErr)
	}
	if flushErr != nil {
		os.Remove(tmpPath)
		return false, fmt.Errorf("flushing chunk data: %w", flushErr)
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return false, fmt.Errorf("closing chunk file: %w", closeErr)
	}

	// Atomic rename
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return false, fmt.Errorf("renaming chunk file: %w", err)
	}

	return true, nil
}

// Get retrieves a chunk by its hash. Returns the chunk with Data populated.
func (s *BlobStore) Get(hash []byte) (*Chunk, error) {
	hashHex := hex.EncodeToString(hash)
	path := s.chunkPath(hashHex)

	s.mu.RLock()
	defer s.mu.RUnlock()

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("chunk not found: %s", hashHex)
		}
		return nil, fmt.Errorf("opening chunk file: %w", err)
	}
	defer f.Close()

	// Get file size for pre-allocation
	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("getting file stats: %w", err)
	}

	// Use buffered reader for high-performance I/O
	reader := bufio.NewReaderSize(f, BufferSize)

	data := make([]byte, stat.Size())
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, fmt.Errorf("reading chunk data: %w", err)
	}

	return &Chunk{
		Hash: hash,
		Size: uint32(len(data)),
		Data: data,
	}, nil
}

// Has checks if a chunk exists without reading its data.
func (s *BlobStore) Has(hash []byte) bool {
	hashHex := hex.EncodeToString(hash)
	path := s.chunkPath(hashHex)

	s.mu.RLock()
	defer s.mu.RUnlock()

	_, err := os.Stat(path)
	return err == nil
}

// Verify checks that a stored chunk matches its hash (detects corruption).
// Returns nil if valid, error describing the corruption otherwise.
func (s *BlobStore) Verify(hash []byte) error {
	hashHex := hex.EncodeToString(hash)
	path := s.chunkPath(hashHex)

	s.mu.RLock()
	defer s.mu.RUnlock()

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("chunk not found: %s", hashHex)
		}
		return fmt.Errorf("opening chunk for verification: %w", err)
	}
	defer f.Close()

	// Stream hash computation to avoid loading entire chunk into memory
	hasher := sha256.New()
	reader := bufio.NewReaderSize(f, BufferSize)

	if _, err := io.Copy(hasher, reader); err != nil {
		return fmt.Errorf("reading chunk for verification: %w", err)
	}

	computed := hasher.Sum(nil)
	if !bytes.Equal(computed, hash) {
		return fmt.Errorf("corruption detected: expected %s, got %s",
			hashHex, hex.EncodeToString(computed))
	}

	return nil
}

// Delete removes a chunk from storage.
func (s *BlobStore) Delete(hash []byte) error {
	hashHex := hex.EncodeToString(hash)
	path := s.chunkPath(hashHex)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil // Already deleted, idempotent
		}
		return fmt.Errorf("deleting chunk: %w", err)
	}

	return nil
}

// StoreReader chunks an io.Reader and stores all chunks.
// Returns the list of chunks (without Data, to save memory) and any error.
func (s *BlobStore) StoreReader(r io.Reader) ([]*Chunk, error) {
	chunker := NewChunker(r)
	var stored []*Chunk

	for {
		chunk, err := chunker.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("chunking data: %w", err)
		}

		if _, err := s.Put(chunk); err != nil {
			return nil, fmt.Errorf("storing chunk %s: %w", chunk.HashHex(), err)
		}

		// Clear Data to reduce memory footprint
		stored = append(stored, &Chunk{
			Hash: chunk.Hash,
			Size: chunk.Size,
		})
	}

	return stored, nil
}

// VerifyAll checks all stored chunks for corruption.
// Returns a map of corrupted hash -> error.
func (s *BlobStore) VerifyAll() (map[string]error, error) {
	corrupted := make(map[string]error)

	err := filepath.Walk(s.baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		// Skip temp files
		if filepath.Ext(path) == ".tmp" {
			return nil
		}

		// Reconstruct hash from path
		rel, err := filepath.Rel(s.baseDir, path)
		if err != nil {
			return nil
		}

		// Extract hash: prefix/suffix -> prefixsuffix
		dir := filepath.Dir(rel)
		name := filepath.Base(rel)
		hashHex := dir + name

		hash, err := hex.DecodeString(hashHex)
		if err != nil {
			return nil // Skip non-hash files
		}

		if err := s.Verify(hash); err != nil {
			corrupted[hashHex] = err
		}

		return nil
	})

	if err != nil {
		return corrupted, fmt.Errorf("walking storage directory: %w", err)
	}

	return corrupted, nil
}

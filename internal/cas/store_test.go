package cas

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestChunker_FixedSizeBlocks(t *testing.T) {
	// Create data that spans multiple chunks
	data := make([]byte, ChunkSize*3+100) // 3 full chunks + partial
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("generating random data: %v", err)
	}

	chunker := NewChunker(bytes.NewReader(data))
	chunks, err := chunker.ChunkAll()
	if err != nil {
		t.Fatalf("chunking failed: %v", err)
	}

	if len(chunks) != 4 {
		t.Errorf("expected 4 chunks, got %d", len(chunks))
	}

	// Verify first 3 chunks are full size
	for i := 0; i < 3; i++ {
		if chunks[i].Size != ChunkSize {
			t.Errorf("chunk %d: expected size %d, got %d", i, ChunkSize, chunks[i].Size)
		}
	}

	// Verify last chunk is partial
	if chunks[3].Size != 100 {
		t.Errorf("last chunk: expected size 100, got %d", chunks[3].Size)
	}
}

func TestChunker_SHA256Hash(t *testing.T) {
	data := []byte("test data for hashing")
	expected := sha256.Sum256(data)

	chunker := NewChunker(bytes.NewReader(data))
	chunk, err := chunker.Next()
	if err != nil {
		t.Fatalf("chunking failed: %v", err)
	}

	if !bytes.Equal(chunk.Hash, expected[:]) {
		t.Errorf("hash mismatch:\nexpected: %x\ngot:      %x", expected, chunk.Hash)
	}
}

func TestChunker_EmptyInput(t *testing.T) {
	chunker := NewChunker(bytes.NewReader(nil))
	chunks, err := chunker.ChunkAll()
	if err != nil {
		t.Fatalf("chunking empty input failed: %v", err)
	}

	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty input, got %d", len(chunks))
	}
}

func TestBlobStore_PutAndGet(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewBlobStore(filepath.Join(tmpDir, "chunks"))
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	data := []byte("test chunk data")
	hash := sha256.Sum256(data)
	chunk := &Chunk{
		Hash: hash[:],
		Size: uint32(len(data)),
		Data: data,
	}

	// Store the chunk
	isNew, err := store.Put(chunk)
	if err != nil {
		t.Fatalf("storing chunk: %v", err)
	}
	if !isNew {
		t.Error("expected new chunk, got existing")
	}

	// Retrieve and verify
	retrieved, err := store.Get(hash[:])
	if err != nil {
		t.Fatalf("retrieving chunk: %v", err)
	}

	if !bytes.Equal(retrieved.Data, data) {
		t.Errorf("data mismatch:\nexpected: %s\ngot:      %s", data, retrieved.Data)
	}
}

func TestBlobStore_Deduplication(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewBlobStore(filepath.Join(tmpDir, "chunks"))
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	data := []byte("duplicate chunk")
	hash := sha256.Sum256(data)
	chunk := &Chunk{
		Hash: hash[:],
		Size: uint32(len(data)),
		Data: data,
	}

	// First write should be new
	isNew, err := store.Put(chunk)
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	if !isNew {
		t.Error("first put should be new")
	}

	// Second write should be deduplicated
	isNew, err = store.Put(chunk)
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if isNew {
		t.Error("second put should be deduplicated")
	}
}

func TestBlobStore_SubdirectoryPath(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "chunks")
	store, err := NewBlobStore(storePath)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	data := []byte("test for path structure")
	hash := sha256.Sum256(data)
	chunk := &Chunk{
		Hash: hash[:],
		Size: uint32(len(data)),
		Data: data,
	}

	if _, err := store.Put(chunk); err != nil {
		t.Fatalf("storing chunk: %v", err)
	}

	// Verify the subdirectory structure
	hashHex := chunk.HashHex()
	expectedPath := filepath.Join(storePath, hashHex[:2], hashHex[2:])

	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("expected chunk at %s, but file doesn't exist: %v", expectedPath, err)
	}
}

func TestBlobStore_Verify(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewBlobStore(filepath.Join(tmpDir, "chunks"))
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	data := []byte("data to verify")
	hash := sha256.Sum256(data)
	chunk := &Chunk{
		Hash: hash[:],
		Size: uint32(len(data)),
		Data: data,
	}

	if _, err := store.Put(chunk); err != nil {
		t.Fatalf("storing chunk: %v", err)
	}

	// Verify should pass
	if err := store.Verify(hash[:]); err != nil {
		t.Errorf("verification failed for valid chunk: %v", err)
	}
}

func TestBlobStore_VerifyCorruption(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "chunks")
	store, err := NewBlobStore(storePath)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	data := []byte("original data")
	hash := sha256.Sum256(data)
	chunk := &Chunk{
		Hash: hash[:],
		Size: uint32(len(data)),
		Data: data,
	}

	if _, err := store.Put(chunk); err != nil {
		t.Fatalf("storing chunk: %v", err)
	}

	// Corrupt the chunk on disk
	hashHex := chunk.HashHex()
	chunkPath := filepath.Join(storePath, hashHex[:2], hashHex[2:])
	if err := os.WriteFile(chunkPath, []byte("corrupted!"), 0600); err != nil {
		t.Fatalf("corrupting chunk: %v", err)
	}

	// Verify should detect corruption
	if err := store.Verify(hash[:]); err == nil {
		t.Error("verification should have detected corruption")
	}
}

func TestBlobStore_StoreReader(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewBlobStore(filepath.Join(tmpDir, "chunks"))
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	// Create multi-chunk data
	data := make([]byte, ChunkSize*2+500)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("generating random data: %v", err)
	}

	chunks, err := store.StoreReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("storing reader: %v", err)
	}

	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks, got %d", len(chunks))
	}

	// Verify all chunks exist and are valid
	for _, chunk := range chunks {
		if !store.Has(chunk.Hash) {
			t.Errorf("chunk %x not found in store", chunk.Hash)
		}
		if err := store.Verify(chunk.Hash); err != nil {
			t.Errorf("chunk %x verification failed: %v", chunk.Hash, err)
		}
	}
}

func TestBlobStore_Has(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewBlobStore(filepath.Join(tmpDir, "chunks"))
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	data := []byte("existence test")
	hash := sha256.Sum256(data)
	chunk := &Chunk{
		Hash: hash[:],
		Size: uint32(len(data)),
		Data: data,
	}

	// Should not exist initially
	if store.Has(hash[:]) {
		t.Error("chunk should not exist before put")
	}

	if _, err := store.Put(chunk); err != nil {
		t.Fatalf("storing chunk: %v", err)
	}

	// Should exist after put
	if !store.Has(hash[:]) {
		t.Error("chunk should exist after put")
	}
}

func TestBlobStore_Delete(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewBlobStore(filepath.Join(tmpDir, "chunks"))
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	data := []byte("delete me")
	hash := sha256.Sum256(data)
	chunk := &Chunk{
		Hash: hash[:],
		Size: uint32(len(data)),
		Data: data,
	}

	if _, err := store.Put(chunk); err != nil {
		t.Fatalf("storing chunk: %v", err)
	}

	if err := store.Delete(hash[:]); err != nil {
		t.Fatalf("deleting chunk: %v", err)
	}

	if store.Has(hash[:]) {
		t.Error("chunk should not exist after delete")
	}

	// Deleting again should be idempotent
	if err := store.Delete(hash[:]); err != nil {
		t.Errorf("second delete should be idempotent: %v", err)
	}
}

// Benchmark buffer pool effectiveness
func BenchmarkChunker_WithPool(b *testing.B) {
	data := make([]byte, ChunkSize*100) // 400KB
	rand.Read(data)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunker := NewChunker(bytes.NewReader(data))
		for {
			_, err := chunker.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkBlobStore_Put(b *testing.B) {
	tmpDir := b.TempDir()
	store, _ := NewBlobStore(filepath.Join(tmpDir, "chunks"))

	chunks := make([]*Chunk, b.N)
	for i := range chunks {
		data := make([]byte, ChunkSize)
		rand.Read(data)
		hash := sha256.Sum256(data)
		chunks[i] = &Chunk{Hash: hash[:], Size: ChunkSize, Data: data}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Put(chunks[i])
	}
}

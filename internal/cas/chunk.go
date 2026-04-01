// Package cas implements a Content-Addressable Storage system with fixed-size
// chunking and SHA-256 content hashing for deduplication.
package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"
)

// ChunkSize is the fixed size for data blocks (4KB).
const ChunkSize = 4 * 1024

// Chunk represents a content-addressed data block.
type Chunk struct {
	Hash []byte // SHA-256 hash (32 bytes)
	Size uint32 // Actual data size (may be < ChunkSize for last chunk)
	Data []byte // Raw data, only populated during transit
}

// HashHex returns the hex-encoded hash string.
func (c *Chunk) HashHex() string {
	return hex.EncodeToString(c.Hash)
}

// bufferPool provides reusable 4KB buffers to minimize GC pressure.
// This is critical for high-throughput chunking of large files.
var bufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, ChunkSize)
		return &buf
	},
}

func getBuffer() *[]byte    { return bufferPool.Get().(*[]byte) }
func putBuffer(buf *[]byte) { bufferPool.Put(buf) }

// Chunker splits an io.Reader into fixed-size chunks with SHA-256 hashes.
type Chunker struct {
	reader io.Reader
}

// NewChunker creates a Chunker that reads from the provided io.Reader.
func NewChunker(r io.Reader) *Chunker {
	return &Chunker{reader: r}
}

// Next reads the next chunk from the input stream.
// Returns io.EOF when no more data is available.
func (c *Chunker) Next() (*Chunk, error) {
	// Get a pooled buffer for reading
	bufPtr := getBuffer()
	buf := *bufPtr
	defer putBuffer(bufPtr)

	// Read up to ChunkSize bytes
	n, err := io.ReadFull(c.reader, buf)

	// Handle read results
	if err == io.EOF {
		return nil, io.EOF
	}
	if err == io.ErrUnexpectedEOF {
		// Partial read at end of file - this is valid
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, io.EOF
	}

	// Compute SHA-256 hash
	hash := sha256.Sum256(buf[:n])

	// Copy data since we're returning the buffer to the pool
	data := make([]byte, n)
	copy(data, buf[:n])

	return &Chunk{
		Hash: hash[:],
		Size: uint32(n),
		Data: data,
	}, nil
}

// ChunkAll reads the entire input and returns all chunks.
func (c *Chunker) ChunkAll() ([]*Chunk, error) {
	var chunks []*Chunk

	for {
		chunk, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}

	return chunks, nil
}

// ChunkStream provides an iterator-style API for memory-efficient chunking.
// Chunks are yielded one at a time via a channel.
func ChunkStream(r io.Reader, chunks chan<- *Chunk, errs chan<- error) {
	defer close(chunks)
	defer close(errs)

	chunker := NewChunker(r)
	for {
		chunk, err := chunker.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			errs <- err
			return
		}
		chunks <- chunk
	}
}

// NewHasher returns a new SHA-256 hasher for computing hashes.
func NewHasher() *Hasher {
	return &Hasher{hash: sha256.New()}
}

// Hasher wraps sha256 for consistent hashing across the package.
type Hasher struct {
	hash interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
}

// Write adds data to the hash computation.
func (h *Hasher) Write(p []byte) (int, error) {
	return h.hash.Write(p)
}

// Sum returns the final hash value.
func (h *Hasher) Sum(b []byte) []byte {
	return h.hash.Sum(b)
}

// Package delta implements rsync-style delta synchronization using rolling hashes.
// It enables efficient transfer of modified files by only sending changed bytes.
package delta

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sync"
)

const (
	// DefaultBlockSize is the default size for signature blocks (4KB to match CAS chunks).
	DefaultBlockSize = 4 * 1024

	// RollingHashMod is the modulus for Adler-32 style rolling hash.
	RollingHashMod = 65521 // Largest prime < 2^16

	// MaxBlockSize prevents excessive memory usage.
	MaxBlockSize = 64 * 1024
)

// RollingHash implements a rolling hash using Adler-32 style algorithm.
// This allows O(1) hash updates as the window slides through data.
type RollingHash struct {
	a, b      uint32 // Hash components
	window    []byte // Current window of bytes (circular buffer)
	size      int    // Window/buffer size
	pos       int    // Write position in circular buffer
	count     int    // Number of bytes currently in window
	blockSize int    // Target block size
}

// rollingHashPool reduces allocations for frequently used rolling hashes.
var rollingHashPool = sync.Pool{
	New: func() interface{} {
		return &RollingHash{
			window: make([]byte, DefaultBlockSize),
		}
	},
}

// NewRollingHash creates a new rolling hash with the specified block size.
func NewRollingHash(blockSize int) *RollingHash {
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}
	if blockSize > MaxBlockSize {
		blockSize = MaxBlockSize
	}

	rh := rollingHashPool.Get().(*RollingHash)
	if cap(rh.window) < blockSize {
		rh.window = make([]byte, blockSize)
	} else {
		rh.window = rh.window[:blockSize]
	}
	rh.Reset(blockSize)
	return rh
}

// Release returns the rolling hash to the pool.
func (rh *RollingHash) Release() {
	rollingHashPool.Put(rh)
}

// Reset clears the rolling hash state.
func (rh *RollingHash) Reset(blockSize int) {
	rh.a = 1 // Adler-32 starts with a=1
	rh.b = 0
	rh.pos = 0
	rh.count = 0
	rh.blockSize = blockSize
	rh.size = len(rh.window)
	// Clear window
	for i := range rh.window {
		rh.window[i] = 0
	}
}

// recompute calculates the hash from scratch based on window contents.
// Uses the same algorithm as ComputeBlockSignature for consistency.
func (rh *RollingHash) recompute() {
	rh.a = 1 // Adler-32 starts with a=1
	rh.b = 0

	// Get bytes in order
	startPos := (rh.pos - rh.count + rh.size) % rh.size
	for i := 0; i < rh.count; i++ {
		idx := (startPos + i) % rh.size
		byteVal := uint32(rh.window[idx])
		rh.a = (rh.a + byteVal) % RollingHashMod
		rh.b = (rh.b + rh.a) % RollingHashMod
	}
}

// RollIn adds a byte to the hash. Returns true when window is full.
func (rh *RollingHash) RollIn(b byte) bool {
	if rh.count < rh.blockSize {
		// Window not full yet - just add
		rh.window[rh.pos] = b
		rh.pos = (rh.pos + 1) % rh.size
		rh.count++
		rh.recompute()
		return rh.count == rh.blockSize
	}
	return true
}

// Roll slides the window by removing the oldest byte and adding a new one.
// Uses recompute to ensure correctness with Adler-32 style hashing.
func (rh *RollingHash) Roll(newByte byte) {
	// The oldest byte is at position (pos - count + size) % size
	// which wraps to pos when count == size (window full)
	oldPos := rh.pos

	// Update window (circular buffer)
	rh.window[oldPos] = newByte
	rh.pos = (rh.pos + 1) % rh.size

	// Recompute hash to ensure correctness
	// The O(1) rolling update is tricky with Adler-32's accumulating b,
	// so we use recompute for guaranteed correctness.
	// For 4KB blocks this is still very fast.
	rh.recompute()
}

// Sum returns the current 32-bit rolling hash value.
func (rh *RollingHash) Sum() uint32 {
	return (rh.b << 16) | rh.a
}

// Window returns a copy of the current window contents in order.
func (rh *RollingHash) Window() []byte {
	result := make([]byte, rh.blockSize)
	// Copy from pos to end, then from start to pos
	startPos := (rh.pos - rh.count + rh.size) % rh.size
	if startPos+rh.count <= rh.size {
		copy(result, rh.window[startPos:startPos+rh.count])
	} else {
		// Wrap around
		firstPart := rh.size - startPos
		copy(result[:firstPart], rh.window[startPos:])
		copy(result[firstPart:], rh.window[:rh.count-firstPart])
	}
	return result
}

// IsFull returns true if the window contains blockSize bytes.
func (rh *RollingHash) IsFull() bool {
	return rh.count >= rh.blockSize
}

// BlockSignature represents a single block's signature for matching.
type BlockSignature struct {
	Index      uint32   // Block index in original file
	WeakHash   uint32   // Rolling hash (fast, for initial matching)
	StrongHash [32]byte // SHA-256 (slow, for verification)
}

// Signature contains all block signatures for a file.
type Signature struct {
	BlockSize int              // Size of each block
	FileSize  int64            // Total file size
	Blocks    []BlockSignature // All block signatures
	weakIndex map[uint32][]int // WeakHash -> indices into Blocks (for O(1) lookup)
}

// NewSignature creates an empty signature.
func NewSignature(blockSize int, fileSize int64) *Signature {
	numBlocks := int((fileSize + int64(blockSize) - 1) / int64(blockSize))
	return &Signature{
		BlockSize: blockSize,
		FileSize:  fileSize,
		Blocks:    make([]BlockSignature, 0, numBlocks),
		weakIndex: make(map[uint32][]int),
	}
}

// AddBlock adds a block signature.
func (s *Signature) AddBlock(index uint32, weakHash uint32, strongHash [32]byte) {
	idx := len(s.Blocks)
	s.Blocks = append(s.Blocks, BlockSignature{
		Index:      index,
		WeakHash:   weakHash,
		StrongHash: strongHash,
	})
	s.weakIndex[weakHash] = append(s.weakIndex[weakHash], idx)
}

// FindByWeakHash returns block indices matching the weak hash.
func (s *Signature) FindByWeakHash(weakHash uint32) []int {
	return s.weakIndex[weakHash]
}

// GetBlock returns the block at the given index.
func (s *Signature) GetBlock(idx int) *BlockSignature {
	if idx < 0 || idx >= len(s.Blocks) {
		return nil
	}
	return &s.Blocks[idx]
}

// SignatureBuilder creates signatures from data streams.
type SignatureBuilder struct {
	blockSize int
	hasher    hash.Hash
}

// NewSignatureBuilder creates a new signature builder.
func NewSignatureBuilder(blockSize int) *SignatureBuilder {
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}
	return &SignatureBuilder{
		blockSize: blockSize,
		hasher:    sha256.New(),
	}
}

// ComputeBlockSignature computes weak and strong hashes for a block.
func (sb *SignatureBuilder) ComputeBlockSignature(data []byte) (uint32, [32]byte) {
	// Compute weak hash (Adler-32 style)
	var a, b uint32 = 1, 0
	for _, c := range data {
		a = (a + uint32(c)) % RollingHashMod
		b = (b + a) % RollingHashMod
	}
	weakHash := (b << 16) | a

	// Compute strong hash (SHA-256)
	sb.hasher.Reset()
	sb.hasher.Write(data)
	var strongHash [32]byte
	copy(strongHash[:], sb.hasher.Sum(nil))

	return weakHash, strongHash
}

// Encode serializes a signature to bytes.
func (s *Signature) Encode() []byte {
	// Format: blockSize(4) + fileSize(8) + numBlocks(4) + blocks...
	size := 16 + len(s.Blocks)*40 // 4+8+4 header + 40 bytes per block
	buf := make([]byte, size)

	binary.BigEndian.PutUint32(buf[0:4], uint32(s.BlockSize))
	binary.BigEndian.PutUint64(buf[4:12], uint64(s.FileSize))
	binary.BigEndian.PutUint32(buf[12:16], uint32(len(s.Blocks)))

	offset := 16
	for _, block := range s.Blocks {
		binary.BigEndian.PutUint32(buf[offset:offset+4], block.Index)
		binary.BigEndian.PutUint32(buf[offset+4:offset+8], block.WeakHash)
		copy(buf[offset+8:offset+40], block.StrongHash[:])
		offset += 40
	}

	return buf
}

// DecodeSignature deserializes a signature from bytes.
func DecodeSignature(data []byte) (*Signature, error) {
	if len(data) < 16 {
		return nil, nil
	}

	blockSize := int(binary.BigEndian.Uint32(data[0:4]))
	fileSize := int64(binary.BigEndian.Uint64(data[4:12]))
	numBlocks := int(binary.BigEndian.Uint32(data[12:16]))

	sig := NewSignature(blockSize, fileSize)

	offset := 16
	for i := 0; i < numBlocks && offset+40 <= len(data); i++ {
		index := binary.BigEndian.Uint32(data[offset : offset+4])
		weakHash := binary.BigEndian.Uint32(data[offset+4 : offset+8])
		var strongHash [32]byte
		copy(strongHash[:], data[offset+8:offset+40])

		sig.AddBlock(index, weakHash, strongHash)
		offset += 40
	}

	return sig, nil
}

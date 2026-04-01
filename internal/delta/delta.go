package delta

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// DeltaOp represents a single delta operation.
type DeltaOp interface {
	isDeltaOp()
	Encode() []byte
}

// OpCopy instructs the receiver to copy a block from their existing data.
type OpCopy struct {
	BlockIndex uint32 // Index of block to copy
}

func (OpCopy) isDeltaOp() {}

// Encode serializes the copy operation.
func (op OpCopy) Encode() []byte {
	buf := make([]byte, 5)
	buf[0] = 0x01 // Copy opcode
	binary.BigEndian.PutUint32(buf[1:5], op.BlockIndex)
	return buf
}

// OpData instructs the receiver to use literal data.
type OpData struct {
	Data []byte
}

func (OpData) isDeltaOp() {}

// Encode serializes the data operation.
func (op OpData) Encode() []byte {
	buf := make([]byte, 5+len(op.Data))
	buf[0] = 0x02 // Data opcode
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(op.Data)))
	copy(buf[5:], op.Data)
	return buf
}

// Delta contains all operations to transform an old file to a new file.
type Delta struct {
	Ops          []DeltaOp
	NewFileSize  int64
	NewFileHash  [32]byte // SHA-256 of new file for verification
	BlocksCopied int      // Stats: blocks reused
	BytesCopied  int64    // Stats: bytes reused
	BytesLiteral int64    // Stats: new bytes sent
}

// Encode serializes the delta to bytes.
func (d *Delta) Encode() []byte {
	var buf bytes.Buffer

	// Header: fileSize(8) + hash(32) + numOps(4)
	header := make([]byte, 44)
	binary.BigEndian.PutUint64(header[0:8], uint64(d.NewFileSize))
	copy(header[8:40], d.NewFileHash[:])
	binary.BigEndian.PutUint32(header[40:44], uint32(len(d.Ops)))
	buf.Write(header)

	// Operations
	for _, op := range d.Ops {
		buf.Write(op.Encode())
	}

	return buf.Bytes()
}

// DecodeDelta deserializes a delta from bytes.
func DecodeDelta(data []byte) (*Delta, error) {
	if len(data) < 44 {
		return nil, fmt.Errorf("delta too short")
	}

	d := &Delta{
		NewFileSize: int64(binary.BigEndian.Uint64(data[0:8])),
	}
	copy(d.NewFileHash[:], data[8:40])
	numOps := int(binary.BigEndian.Uint32(data[40:44]))

	offset := 44
	for i := 0; i < numOps && offset < len(data); i++ {
		opcode := data[offset]
		offset++

		switch opcode {
		case 0x01: // Copy
			if offset+4 > len(data) {
				return nil, fmt.Errorf("truncated copy op")
			}
			blockIdx := binary.BigEndian.Uint32(data[offset : offset+4])
			d.Ops = append(d.Ops, OpCopy{BlockIndex: blockIdx})
			offset += 4

		case 0x02: // Data
			if offset+4 > len(data) {
				return nil, fmt.Errorf("truncated data op length")
			}
			dataLen := int(binary.BigEndian.Uint32(data[offset : offset+4]))
			offset += 4
			if offset+dataLen > len(data) {
				return nil, fmt.Errorf("truncated data op")
			}
			opData := make([]byte, dataLen)
			copy(opData, data[offset:offset+dataLen])
			d.Ops = append(d.Ops, OpData{Data: opData})
			offset += dataLen
		}
	}

	return d, nil
}

// DeltaGenerator computes deltas between files using signatures.
type DeltaGenerator struct {
	blockSize int
	bufPool   *bufferPool
}

// bufferPool manages reusable buffers.
type bufferPool struct {
	pool chan []byte
	size int
}

func newBufferPool(size, count int) *bufferPool {
	bp := &bufferPool{
		pool: make(chan []byte, count),
		size: size,
	}
	for i := 0; i < count; i++ {
		bp.pool <- make([]byte, size)
	}
	return bp
}

func (bp *bufferPool) get() []byte {
	select {
	case buf := <-bp.pool:
		return buf
	default:
		return make([]byte, bp.size)
	}
}

func (bp *bufferPool) put(buf []byte) {
	select {
	case bp.pool <- buf:
	default:
		// Pool full, let GC handle it
	}
}

// NewDeltaGenerator creates a new delta generator.
func NewDeltaGenerator(blockSize int) *DeltaGenerator {
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}
	return &DeltaGenerator{
		blockSize: blockSize,
		bufPool:   newBufferPool(blockSize, 4),
	}
}

// GenerateDelta computes a delta from newFile using the remote signature.
// This is memory-efficient: reads the file in a streaming fashion.
func (dg *DeltaGenerator) GenerateDelta(newFile io.Reader, sig *Signature) (*Delta, error) {
	delta := &Delta{
		Ops: make([]DeltaOp, 0, len(sig.Blocks)),
	}

	// Buffer for accumulating non-matching data
	literalBuf := new(bytes.Buffer)

	// Create rolling hash
	rh := NewRollingHash(sig.BlockSize)
	defer rh.Release()

	// Use buffered reader for performance
	reader := bufio.NewReaderSize(newFile, 64*1024)

	// SHA-256 of entire new file
	fileHasher := sha256.New()

	// Signature builder for strong hash verification
	sb := NewSignatureBuilder(sig.BlockSize)

	var totalBytes int64
	eofReached := false

	// Fill initial window
	for i := 0; i < sig.BlockSize; i++ {
		b, err := reader.ReadByte()
		if err == io.EOF {
			// File smaller than one block
			if i > 0 {
				window := rh.Window()[:i]
				literalBuf.Write(window)
				fileHasher.Write(window)
				totalBytes += int64(i)
			}
			eofReached = true
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading initial window: %w", err)
		}
		rh.RollIn(b)
	}

	// Main processing loop
	for !eofReached {
		matchFound := false

		if rh.IsFull() {
			weakHash := rh.Sum()

			// Check for match
			if candidates := sig.FindByWeakHash(weakHash); len(candidates) > 0 {
				window := rh.Window()
				_, strongHash := sb.ComputeBlockSignature(window)

				for _, idx := range candidates {
					block := sig.GetBlock(idx)
					if block != nil && strongHash == block.StrongHash {
						// Match found!
						if literalBuf.Len() > 0 {
							delta.Ops = append(delta.Ops, OpData{Data: literalBuf.Bytes()})
							delta.BytesLiteral += int64(literalBuf.Len())
							literalBuf = new(bytes.Buffer)
						}

						delta.Ops = append(delta.Ops, OpCopy{BlockIndex: block.Index})
						delta.BlocksCopied++
						delta.BytesCopied += int64(sig.BlockSize)

						fileHasher.Write(window)
						totalBytes += int64(len(window))

						rh.Reset(sig.BlockSize)

						// Fill next window
						partialCount := 0
						for i := 0; i < sig.BlockSize; i++ {
							nextByte, readErr := reader.ReadByte()
							if readErr == io.EOF {
								eofReached = true
								break
							}
							if readErr != nil {
								return nil, fmt.Errorf("reading after match: %w", readErr)
							}
							rh.RollIn(nextByte)
							partialCount++
						}

						// If we hit EOF with partial data, check for partial block match
						if eofReached && partialCount > 0 {
							partialData := rh.Window()[:partialCount]
							partialWeakHash, partialStrongHash := sb.ComputeBlockSignature(partialData)

							// Look for matching partial block in signature
							if partialCandidates := sig.FindByWeakHash(partialWeakHash); len(partialCandidates) > 0 {
								for _, pIdx := range partialCandidates {
									pBlock := sig.GetBlock(pIdx)
									if pBlock != nil && partialStrongHash == pBlock.StrongHash {
										// Partial block match!
										delta.Ops = append(delta.Ops, OpCopy{BlockIndex: pBlock.Index})
										delta.BlocksCopied++
										delta.BytesCopied += int64(partialCount)
										fileHasher.Write(partialData)
										totalBytes += int64(partialCount)
										partialCount = 0 // Mark as handled
										break
									}
								}
							}

							// If no match, emit as literal
							if partialCount > 0 {
								literalBuf.Write(partialData)
								fileHasher.Write(partialData)
								totalBytes += int64(partialCount)
							}
						}

						matchFound = true
						break
					}
				}
			}
		}

		if matchFound || eofReached {
			continue
		}

		// No match - emit oldest byte as literal and roll
		if rh.IsFull() {
			oldestPos := (rh.pos - rh.count + rh.size) % rh.size
			oldestByte := rh.window[oldestPos]
			literalBuf.WriteByte(oldestByte)
			fileHasher.Write([]byte{oldestByte})
			totalBytes++
		}

		// Read next byte and roll
		nextByte, readErr := reader.ReadByte()
		if readErr == io.EOF {
			if rh.IsFull() {
				window := rh.Window()
				literalBuf.Write(window[1:])
				fileHasher.Write(window[1:])
				totalBytes += int64(len(window) - 1)
			}
			eofReached = true
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("reading file: %w", readErr)
		}

		if rh.IsFull() {
			rh.Roll(nextByte)
		} else {
			rh.RollIn(nextByte)
		}
	}

	// Emit any remaining literal data
	if literalBuf.Len() > 0 {
		delta.Ops = append(delta.Ops, OpData{Data: literalBuf.Bytes()})
		delta.BytesLiteral += int64(literalBuf.Len())
	}

	delta.NewFileSize = totalBytes
	copy(delta.NewFileHash[:], fileHasher.Sum(nil))

	return delta, nil
}

// GenerateSignature creates a signature from a file for delta computation.
// Reads file in streaming fashion for memory efficiency.
func GenerateSignature(r io.Reader, blockSize int) (*Signature, error) {
	if blockSize <= 0 {
		blockSize = DefaultBlockSize
	}

	// First pass: determine file size (if seekable) or count while reading
	var fileSize int64

	// Buffer for reading blocks
	buf := make([]byte, blockSize)
	sb := NewSignatureBuilder(blockSize)

	// We'll build signature as we read
	sig := &Signature{
		BlockSize: blockSize,
		Blocks:    make([]BlockSignature, 0),
		weakIndex: make(map[uint32][]int),
	}

	var blockIndex uint32
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			weakHash, strongHash := sb.ComputeBlockSignature(buf[:n])
			sig.AddBlock(blockIndex, weakHash, strongHash)
			fileSize += int64(n)
			blockIndex++
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading block %d: %w", blockIndex, err)
		}
	}

	sig.FileSize = fileSize
	return sig, nil
}

// ApplyDelta applies a delta to reconstruct the new file.
// oldBlocks is a function that returns block data by index.
func ApplyDelta(delta *Delta, oldBlocks func(index uint32) ([]byte, error), w io.Writer) error {
	hasher := sha256.New()
	mw := io.MultiWriter(w, hasher)

	var written int64

	for _, op := range delta.Ops {
		switch o := op.(type) {
		case OpCopy:
			data, err := oldBlocks(o.BlockIndex)
			if err != nil {
				return fmt.Errorf("getting block %d: %w", o.BlockIndex, err)
			}
			n, err := mw.Write(data)
			if err != nil {
				return fmt.Errorf("writing block: %w", err)
			}
			written += int64(n)

		case OpData:
			n, err := mw.Write(o.Data)
			if err != nil {
				return fmt.Errorf("writing literal: %w", err)
			}
			written += int64(n)
		}
	}

	// Verify hash
	var computedHash [32]byte
	copy(computedHash[:], hasher.Sum(nil))
	if computedHash != delta.NewFileHash {
		return fmt.Errorf("hash mismatch: file corrupted")
	}

	if written != delta.NewFileSize {
		return fmt.Errorf("size mismatch: expected %d, wrote %d", delta.NewFileSize, written)
	}

	return nil
}

// CompressionRatio returns the ratio of data saved (0.0 to 1.0).
func (d *Delta) CompressionRatio() float64 {
	if d.NewFileSize == 0 {
		return 0
	}
	return float64(d.BytesCopied) / float64(d.NewFileSize)
}

// TransferSize returns the number of bytes that need to be transferred.
func (d *Delta) TransferSize() int64 {
	// Approximate: operations overhead + literal bytes
	opOverhead := int64(len(d.Ops) * 5) // ~5 bytes per op
	return opOverhead + d.BytesLiteral
}

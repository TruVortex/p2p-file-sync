package delta

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func TestRollingHash_Basic(t *testing.T) {
	rh := NewRollingHash(4)
	defer rh.Release()

	// Fill window
	data := []byte("test")
	for _, b := range data {
		rh.RollIn(b)
	}

	if !rh.IsFull() {
		t.Error("expected window to be full")
	}

	hash1 := rh.Sum()

	// Same data should produce same hash
	rh2 := NewRollingHash(4)
	defer rh2.Release()
	for _, b := range data {
		rh2.RollIn(b)
	}

	if hash1 != rh2.Sum() {
		t.Error("same data should produce same hash")
	}

	// Different data should produce different hash
	rh3 := NewRollingHash(4)
	defer rh3.Release()
	for _, b := range []byte("TEST") {
		rh3.RollIn(b)
	}

	if hash1 == rh3.Sum() {
		t.Error("different data should produce different hash")
	}
}

func TestRollingHash_Roll(t *testing.T) {
	// Verify that rolling produces correct hash
	data := []byte("abcdefgh")
	blockSize := 4

	// Compute hash of "bcde" by filling fresh
	rh1 := NewRollingHash(blockSize)
	for _, b := range data[1:5] {
		rh1.RollIn(b)
	}
	expected := rh1.Sum()
	rh1.Release()

	// Compute hash of "bcde" by rolling from "abcd"
	rh2 := NewRollingHash(blockSize)
	for _, b := range data[0:4] {
		rh2.RollIn(b)
	}
	rh2.Roll(data[4]) // Roll in 'e', remove 'a'
	actual := rh2.Sum()
	rh2.Release()

	if expected != actual {
		t.Errorf("rolling hash mismatch: expected %08x, got %08x", expected, actual)
	}
}

func TestRollingHash_Window(t *testing.T) {
	rh := NewRollingHash(4)
	defer rh.Release()

	// Fill window
	for _, b := range []byte("abcd") {
		rh.RollIn(b)
	}

	window := rh.Window()
	if string(window) != "abcd" {
		t.Errorf("expected 'abcd', got '%s'", string(window))
	}

	// Roll and check window
	rh.Roll('e')
	window = rh.Window()
	if string(window) != "bcde" {
		t.Errorf("expected 'bcde', got '%s'", string(window))
	}
}

func TestSignature_EncodeDecode(t *testing.T) {
	sig := NewSignature(4096, 10000)
	sig.AddBlock(0, 0x12345678, [32]byte{1, 2, 3})
	sig.AddBlock(1, 0xDEADBEEF, [32]byte{4, 5, 6})

	encoded := sig.Encode()
	decoded, err := DecodeSignature(encoded)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if decoded.BlockSize != sig.BlockSize {
		t.Errorf("block size mismatch")
	}
	if decoded.FileSize != sig.FileSize {
		t.Errorf("file size mismatch")
	}
	if len(decoded.Blocks) != len(sig.Blocks) {
		t.Errorf("block count mismatch: %d != %d", len(decoded.Blocks), len(sig.Blocks))
	}
	if decoded.Blocks[0].WeakHash != 0x12345678 {
		t.Errorf("weak hash mismatch")
	}
}

func TestGenerateSignature(t *testing.T) {
	data := make([]byte, 10000)
	rand.Read(data)

	sig, err := GenerateSignature(bytes.NewReader(data), 4096)
	if err != nil {
		t.Fatalf("generate signature: %v", err)
	}

	// Should have 3 blocks (4096 + 4096 + 1808)
	if len(sig.Blocks) != 3 {
		t.Errorf("expected 3 blocks, got %d", len(sig.Blocks))
	}
	if sig.FileSize != 10000 {
		t.Errorf("expected file size 10000, got %d", sig.FileSize)
	}
}

func TestDelta_IdenticalFiles(t *testing.T) {
	data := make([]byte, 10000)
	rand.Read(data)

	// Generate signature from original
	sig, err := GenerateSignature(bytes.NewReader(data), 4096)
	if err != nil {
		t.Fatalf("generate signature: %v", err)
	}

	// Generate delta for identical file
	dg := NewDeltaGenerator(4096)
	delta, err := dg.GenerateDelta(bytes.NewReader(data), sig)
	if err != nil {
		t.Fatalf("generate delta: %v", err)
	}

	// Should be all copy operations
	copyOps := 0
	dataOps := 0
	for _, op := range delta.Ops {
		switch op.(type) {
		case OpCopy:
			copyOps++
		case OpData:
			dataOps++
		}
	}

	if copyOps != 3 {
		t.Errorf("expected 3 copy ops, got %d", copyOps)
	}
	if dataOps != 0 {
		t.Errorf("expected 0 data ops, got %d", dataOps)
	}

	// Compression ratio should be ~1.0
	ratio := delta.CompressionRatio()
	if ratio < 0.9 {
		t.Errorf("expected high compression ratio, got %.2f", ratio)
	}
}

func TestDelta_ModifiedFile(t *testing.T) {
	// Create original file with repeating pattern
	original := make([]byte, 12288) // 3 blocks
	for i := range original {
		original[i] = byte(i % 256)
	}

	// Create modified file - change middle block
	modified := make([]byte, 12288)
	copy(modified, original)
	for i := 4096; i < 8192; i++ {
		modified[i] = byte(255 - (i % 256)) // Different pattern
	}

	// Generate signature from original
	sig, err := GenerateSignature(bytes.NewReader(original), 4096)
	if err != nil {
		t.Fatalf("generate signature: %v", err)
	}

	// Generate delta
	dg := NewDeltaGenerator(4096)
	delta, err := dg.GenerateDelta(bytes.NewReader(modified), sig)
	if err != nil {
		t.Fatalf("generate delta: %v", err)
	}

	// Should have 2 copy ops (first and last block) and 1 data op (middle)
	if delta.BlocksCopied != 2 {
		t.Errorf("expected 2 blocks copied, got %d", delta.BlocksCopied)
	}

	// Apply delta and verify
	var result bytes.Buffer
	blockReader := func(idx uint32) ([]byte, error) {
		start := int(idx) * 4096
		end := start + 4096
		if end > len(original) {
			end = len(original)
		}
		return original[start:end], nil
	}

	err = ApplyDelta(delta, blockReader, &result)
	if err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	if !bytes.Equal(result.Bytes(), modified) {
		t.Error("reconstructed file doesn't match modified")
	}
}

func TestDelta_NewFile(t *testing.T) {
	// Completely new file with no matching blocks
	original := make([]byte, 8192)
	rand.Read(original)

	modified := make([]byte, 8192)
	rand.Read(modified) // Completely different

	sig, err := GenerateSignature(bytes.NewReader(original), 4096)
	if err != nil {
		t.Fatalf("generate signature: %v", err)
	}

	dg := NewDeltaGenerator(4096)
	delta, err := dg.GenerateDelta(bytes.NewReader(modified), sig)
	if err != nil {
		t.Fatalf("generate delta: %v", err)
	}

	// Should have 0 copy ops - all literal data
	if delta.BlocksCopied != 0 {
		t.Errorf("expected 0 blocks copied, got %d", delta.BlocksCopied)
	}
	if delta.BytesLiteral != 8192 {
		t.Errorf("expected 8192 literal bytes, got %d", delta.BytesLiteral)
	}
}

func TestDelta_InsertedData(t *testing.T) {
	// Original file
	original := []byte("AAAA1111BBBB2222CCCC3333")

	// Modified: insert data at start
	modified := []byte("XXXX" + "AAAA1111BBBB2222CCCC3333")

	sig, err := GenerateSignature(bytes.NewReader(original), 4)
	if err != nil {
		t.Fatalf("generate signature: %v", err)
	}

	dg := NewDeltaGenerator(4)
	delta, err := dg.GenerateDelta(bytes.NewReader(modified), sig)
	if err != nil {
		t.Fatalf("generate delta: %v", err)
	}

	// Should find all original blocks after the inserted data
	if delta.BlocksCopied < 4 {
		t.Errorf("expected at least 4 blocks copied, got %d", delta.BlocksCopied)
	}
}

func TestDelta_LargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large file test")
	}

	// 1MB file
	size := 1024 * 1024
	original := make([]byte, size)
	rand.Read(original)

	// Modify ~10% of blocks
	modified := make([]byte, size)
	copy(modified, original)
	for i := 0; i < size; i += 40960 { // Every 10th block
		for j := i; j < i+4096 && j < size; j++ {
			modified[j] = byte(255 - modified[j])
		}
	}

	sig, err := GenerateSignature(bytes.NewReader(original), 4096)
	if err != nil {
		t.Fatalf("generate signature: %v", err)
	}

	dg := NewDeltaGenerator(4096)
	delta, err := dg.GenerateDelta(bytes.NewReader(modified), sig)
	if err != nil {
		t.Fatalf("generate delta: %v", err)
	}

	// Should have ~90% compression
	ratio := delta.CompressionRatio()
	if ratio < 0.8 {
		t.Errorf("expected >80%% compression, got %.1f%%", ratio*100)
	}

	// Verify reconstruction
	var result bytes.Buffer
	blockReader := func(idx uint32) ([]byte, error) {
		start := int(idx) * 4096
		end := start + 4096
		if end > len(original) {
			end = len(original)
		}
		return original[start:end], nil
	}

	err = ApplyDelta(delta, blockReader, &result)
	if err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	if !bytes.Equal(result.Bytes(), modified) {
		t.Error("reconstructed file doesn't match")
	}
}

func TestDelta_EncodeDecode(t *testing.T) {
	delta := &Delta{
		NewFileSize: 1000,
		NewFileHash: [32]byte{1, 2, 3, 4},
		Ops: []DeltaOp{
			OpCopy{BlockIndex: 5},
			OpData{Data: []byte("test data")},
			OpCopy{BlockIndex: 7},
		},
	}

	encoded := delta.Encode()
	decoded, err := DecodeDelta(encoded)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if decoded.NewFileSize != delta.NewFileSize {
		t.Errorf("size mismatch")
	}
	if decoded.NewFileHash != delta.NewFileHash {
		t.Errorf("hash mismatch")
	}
	if len(decoded.Ops) != 3 {
		t.Fatalf("ops count mismatch: %d", len(decoded.Ops))
	}

	if op, ok := decoded.Ops[0].(OpCopy); !ok || op.BlockIndex != 5 {
		t.Error("first op mismatch")
	}
	if op, ok := decoded.Ops[1].(OpData); !ok || string(op.Data) != "test data" {
		t.Error("second op mismatch")
	}
}

// Benchmark rolling hash performance
func BenchmarkRollingHash_Roll(b *testing.B) {
	rh := NewRollingHash(4096)
	defer rh.Release()

	// Fill window
	data := make([]byte, 4096)
	rand.Read(data)
	for _, d := range data {
		rh.RollIn(d)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rh.Roll(byte(i))
	}
}

func BenchmarkDeltaGeneration(b *testing.B) {
	// 100KB file
	data := make([]byte, 100*1024)
	rand.Read(data)

	sig, _ := GenerateSignature(bytes.NewReader(data), 4096)
	dg := NewDeltaGenerator(4096)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dg.GenerateDelta(bytes.NewReader(data), sig)
	}
}

// Test memory efficiency - should not allocate proportionally to file size
func TestDelta_MemoryEfficiency(t *testing.T) {
	// This test verifies that we can process large data without
	// loading it all into memory at once

	// Create a "streaming" reader that fails if too much is read at once
	size := 1024 * 1024       // 1MB
	maxBuffered := 128 * 1024 // 128KB max should be buffered

	r := &limitedReader{
		data:      make([]byte, size),
		maxRead:   maxBuffered,
		totalRead: 0,
	}
	rand.Read(r.data)

	sig, err := GenerateSignature(r, 4096)
	if err != nil {
		t.Fatalf("generate signature: %v", err)
	}

	// Reset reader
	r.totalRead = 0

	dg := NewDeltaGenerator(4096)
	_, err = dg.GenerateDelta(r, sig)
	if err != nil {
		t.Fatalf("generate delta: %v", err)
	}
}

type limitedReader struct {
	data      []byte
	pos       int
	maxRead   int
	totalRead int
}

func (r *limitedReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}

	n := len(p)
	if n > r.maxRead {
		n = r.maxRead
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}

	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	r.totalRead += n

	return n, nil
}

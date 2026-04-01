// Package proto defines the wire protocol messages for P2P sync.
// This file provides helper functions for working with generated protobuf types.
package proto

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

// ProtocolVersion is the current protocol version.
const ProtocolVersion uint32 = 1

// WireMessage provides methods for reading/writing protobuf messages over the wire.

// WriteEnvelope writes an envelope with the given message to the writer.
func WriteEnvelope(w io.Writer, msgType MessageType, requestID uint64, msg proto.Message) error {
	// Serialize the inner message
	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshaling payload: %w", err)
	}

	// Create envelope
	env := &Envelope{
		Type:      msgType,
		RequestId: requestID,
		Payload:   payload,
	}

	// Serialize envelope
	data, err := proto.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshaling envelope: %w", err)
	}

	// Write length prefix (4 bytes, big-endian)
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	if _, err := w.Write(lenBuf); err != nil {
		return fmt.Errorf("writing length: %w", err)
	}

	// Write data
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("writing data: %w", err)
	}

	return nil
}

// ReadEnvelope reads an envelope from the reader.
func ReadEnvelope(r io.Reader) (*Envelope, error) {
	// Read length prefix
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return nil, fmt.Errorf("reading length: %w", err)
	}

	length := binary.BigEndian.Uint32(lenBuf)
	if length > 16*1024*1024 { // 16MB max message size
		return nil, fmt.Errorf("message too large: %d bytes", length)
	}

	// Read data
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("reading data: %w", err)
	}

	// Unmarshal envelope
	env := &Envelope{}
	if err := proto.Unmarshal(data, env); err != nil {
		return nil, fmt.Errorf("unmarshaling envelope: %w", err)
	}

	return env, nil
}

// UnmarshalPayload unmarshals the payload of an envelope into the given message.
func UnmarshalPayload(env *Envelope, msg proto.Message) error {
	return proto.Unmarshal(env.Payload, msg)
}

// Helper functions for creating common messages

// NewHandshake creates a handshake message.
func NewHandshake(certFingerprint []byte, peerName string, merkleRoot []byte, fileCount, totalSize uint64) *Handshake {
	return &Handshake{
		ProtocolVersion: ProtocolVersion,
		CertFingerprint: certFingerprint,
		PeerName:        peerName,
		MerkleRoot:      merkleRoot,
		FileCount:       fileCount,
		TotalSize:       totalSize,
	}
}

// NewHandshakeAck creates a handshake acknowledgment.
func NewHandshakeAck(accepted bool, certFingerprint []byte, peerName string, merkleRoot []byte) *HandshakeAck {
	return &HandshakeAck{
		Accepted:        accepted,
		CertFingerprint: certFingerprint,
		PeerName:        peerName,
		MerkleRoot:      merkleRoot,
		ProtocolVersion: ProtocolVersion,
	}
}

// NewChunkRequest creates a chunk request.
func NewChunkRequest(hashes [][]byte, priority uint32) *ChunkRequest {
	return &ChunkRequest{
		ChunkHashes: hashes,
		Priority:    priority,
	}
}

// NewChunkResponse creates a chunk response.
func NewChunkResponse(chunks []*Chunk, notFound [][]byte) *ChunkResponse {
	return &ChunkResponse{
		Chunks:   chunks,
		NotFound: notFound,
	}
}

// NewChunk creates a chunk message.
func NewChunk(hash, data []byte) *Chunk {
	return &Chunk{
		Hash: hash,
		Data: data,
		Size: uint32(len(data)),
	}
}

// NewManifestRequest creates a manifest request.
func NewManifestRequest(subtreePath string, maxDepth int32, includeChunks bool) *ManifestRequest {
	return &ManifestRequest{
		SubtreePath:   subtreePath,
		MaxDepth:      maxDepth,
		IncludeChunks: includeChunks,
	}
}

// NewErrorMessage creates an error message.
func NewErrorMessage(code uint32, message string, requestID uint64) *ErrorMessage {
	return &ErrorMessage{
		Code:      code,
		Message:   message,
		RequestId: requestID,
	}
}

// Error codes
const (
	ErrorCodeUnknown          uint32 = 0
	ErrorCodeInvalidMessage   uint32 = 1
	ErrorCodeChunkNotFound    uint32 = 2
	ErrorCodePermissionDenied uint32 = 3
	ErrorCodeInternalError    uint32 = 4
	ErrorCodeVersionMismatch  uint32 = 5
)

// SendHandshake sends a handshake message.
func SendHandshake(w io.Writer, requestID uint64, h *Handshake) error {
	return WriteEnvelope(w, MessageType_MESSAGE_TYPE_HANDSHAKE, requestID, h)
}

// SendHandshakeAck sends a handshake acknowledgment.
func SendHandshakeAck(w io.Writer, requestID uint64, h *HandshakeAck) error {
	return WriteEnvelope(w, MessageType_MESSAGE_TYPE_HANDSHAKE_ACK, requestID, h)
}

// SendManifestRequest sends a manifest request.
func SendManifestRequest(w io.Writer, requestID uint64, m *ManifestRequest) error {
	return WriteEnvelope(w, MessageType_MESSAGE_TYPE_MANIFEST_REQUEST, requestID, m)
}

// SendManifestResponse sends a manifest response.
func SendManifestResponse(w io.Writer, requestID uint64, m *ManifestResponse) error {
	return WriteEnvelope(w, MessageType_MESSAGE_TYPE_MANIFEST_RESPONSE, requestID, m)
}

// SendChunkRequest sends a chunk request.
func SendChunkRequest(w io.Writer, requestID uint64, c *ChunkRequest) error {
	return WriteEnvelope(w, MessageType_MESSAGE_TYPE_CHUNK_REQUEST, requestID, c)
}

// SendChunkResponse sends a chunk response.
func SendChunkResponse(w io.Writer, requestID uint64, c *ChunkResponse) error {
	return WriteEnvelope(w, MessageType_MESSAGE_TYPE_CHUNK_RESPONSE, requestID, c)
}

// SendError sends an error message.
func SendError(w io.Writer, requestID uint64, e *ErrorMessage) error {
	return WriteEnvelope(w, MessageType_MESSAGE_TYPE_ERROR, requestID, e)
}

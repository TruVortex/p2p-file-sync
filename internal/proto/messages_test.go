package proto

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestHandshake_ProtobufRoundTrip(t *testing.T) {
	original := NewHandshake(
		make([]byte, 32),
		"test-peer",
		make([]byte, 32),
		100,
		1024*1024,
	)
	// Fill with test data
	for i := range original.CertFingerprint {
		original.CertFingerprint[i] = byte(i)
	}
	for i := range original.MerkleRoot {
		original.MerkleRoot[i] = byte(i + 32)
	}

	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	decoded := &Handshake{}
	if err := proto.Unmarshal(data, decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.ProtocolVersion != original.ProtocolVersion {
		t.Errorf("protocol version mismatch: %d != %d", decoded.ProtocolVersion, original.ProtocolVersion)
	}
	if !bytes.Equal(decoded.CertFingerprint, original.CertFingerprint) {
		t.Error("cert fingerprint mismatch")
	}
	if decoded.PeerName != original.PeerName {
		t.Errorf("peer name mismatch: %s != %s", decoded.PeerName, original.PeerName)
	}
	if !bytes.Equal(decoded.MerkleRoot, original.MerkleRoot) {
		t.Error("merkle root mismatch")
	}
	if decoded.FileCount != original.FileCount {
		t.Errorf("file count mismatch: %d != %d", decoded.FileCount, original.FileCount)
	}
	if decoded.TotalSize != original.TotalSize {
		t.Errorf("total size mismatch: %d != %d", decoded.TotalSize, original.TotalSize)
	}
}

func TestHandshakeAck_ProtobufRoundTrip(t *testing.T) {
	original := NewHandshakeAck(true, make([]byte, 32), "responder", make([]byte, 32))

	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	decoded := &HandshakeAck{}
	if err := proto.Unmarshal(data, decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Accepted != original.Accepted {
		t.Errorf("accepted mismatch: %v != %v", decoded.Accepted, original.Accepted)
	}
	if decoded.PeerName != original.PeerName {
		t.Errorf("peer name mismatch: %s != %s", decoded.PeerName, original.PeerName)
	}
}

func TestChunkRequest_ProtobufRoundTrip(t *testing.T) {
	hashes := [][]byte{
		make([]byte, 32),
		make([]byte, 32),
		make([]byte, 32),
	}
	for i, h := range hashes {
		for j := range h {
			h[j] = byte(i*32 + j)
		}
	}
	original := NewChunkRequest(hashes, 5)

	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	decoded := &ChunkRequest{}
	if err := proto.Unmarshal(data, decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(decoded.ChunkHashes) != len(original.ChunkHashes) {
		t.Fatalf("hash count mismatch: %d != %d", len(decoded.ChunkHashes), len(original.ChunkHashes))
	}
	for i := range original.ChunkHashes {
		if !bytes.Equal(decoded.ChunkHashes[i], original.ChunkHashes[i]) {
			t.Errorf("hash %d mismatch", i)
		}
	}
	if decoded.Priority != original.Priority {
		t.Errorf("priority mismatch: %d != %d", decoded.Priority, original.Priority)
	}
}

func TestChunkResponse_ProtobufRoundTrip(t *testing.T) {
	original := NewChunkResponse(
		[]*Chunk{
			NewChunk(make([]byte, 32), []byte("chunk data 1")),
			NewChunk(make([]byte, 32), []byte("chunk data 2")),
		},
		[][]byte{make([]byte, 32)},
	)

	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	decoded := &ChunkResponse{}
	if err := proto.Unmarshal(data, decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(decoded.Chunks) != len(original.Chunks) {
		t.Fatalf("chunk count mismatch: %d != %d", len(decoded.Chunks), len(original.Chunks))
	}
	for i, c := range original.Chunks {
		if !bytes.Equal(decoded.Chunks[i].Data, c.Data) {
			t.Errorf("chunk %d data mismatch", i)
		}
		if decoded.Chunks[i].Size != c.Size {
			t.Errorf("chunk %d size mismatch", i)
		}
	}
	if len(decoded.NotFound) != len(original.NotFound) {
		t.Errorf("not found count mismatch: %d != %d", len(decoded.NotFound), len(original.NotFound))
	}
}

func TestWriteReadEnvelope(t *testing.T) {
	buf := &bytes.Buffer{}

	// Create and send a handshake
	handshake := NewHandshake(
		[]byte("fingerprint-data-here-32-bytes!!"),
		"test-peer",
		[]byte("merkle-root-hash-32-bytes-here!!"),
		42,
		1024,
	)

	err := SendHandshake(buf, 123, handshake)
	if err != nil {
		t.Fatalf("send error: %v", err)
	}

	// Read the envelope
	env, err := ReadEnvelope(buf)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}

	if env.Type != MessageType_MESSAGE_TYPE_HANDSHAKE {
		t.Errorf("message type mismatch: %d != %d", env.Type, MessageType_MESSAGE_TYPE_HANDSHAKE)
	}
	if env.RequestId != 123 {
		t.Errorf("request ID mismatch: %d != 123", env.RequestId)
	}

	// Unmarshal the payload
	decoded := &Handshake{}
	if err := UnmarshalPayload(env, decoded); err != nil {
		t.Fatalf("unmarshal payload error: %v", err)
	}

	if decoded.PeerName != "test-peer" {
		t.Errorf("peer name mismatch: %s", decoded.PeerName)
	}
	if decoded.FileCount != 42 {
		t.Errorf("file count mismatch: %d", decoded.FileCount)
	}
}

func TestManifestNode_ProtobufRoundTrip(t *testing.T) {
	original := &ManifestNode{
		Name:  "root",
		IsDir: true,
		Hash:  make([]byte, 32),
		Mode:  0755,
		Size:  1000,
		Children: []*ManifestNode{
			{
				Name:   "file.txt",
				IsDir:  false,
				Hash:   make([]byte, 32),
				Mode:   0644,
				Size:   500,
				Chunks: [][]byte{make([]byte, 32), make([]byte, 32)},
			},
			{
				Name:  "subdir",
				IsDir: true,
				Hash:  make([]byte, 32),
				Mode:  0755,
				Size:  500,
			},
		},
	}

	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	decoded := &ManifestNode{}
	if err := proto.Unmarshal(data, decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Name != original.Name {
		t.Errorf("name mismatch: %s != %s", decoded.Name, original.Name)
	}
	if decoded.IsDir != original.IsDir {
		t.Errorf("isDir mismatch")
	}
	if len(decoded.Children) != len(original.Children) {
		t.Fatalf("children count mismatch: %d != %d", len(decoded.Children), len(original.Children))
	}
	if decoded.Children[0].Name != "file.txt" {
		t.Errorf("child name mismatch")
	}
	if len(decoded.Children[0].Chunks) != 2 {
		t.Errorf("chunks count mismatch: %d != 2", len(decoded.Children[0].Chunks))
	}
}

func TestErrorMessage_ProtobufRoundTrip(t *testing.T) {
	original := NewErrorMessage(ErrorCodeChunkNotFound, "chunk not found", 42)

	data, err := proto.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	decoded := &ErrorMessage{}
	if err := proto.Unmarshal(data, decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Code != original.Code {
		t.Errorf("code mismatch: %d != %d", decoded.Code, original.Code)
	}
	if decoded.Message != original.Message {
		t.Errorf("message mismatch: %s != %s", decoded.Message, original.Message)
	}
	if decoded.RequestId != original.RequestId {
		t.Errorf("request id mismatch: %d != %d", decoded.RequestId, original.RequestId)
	}
}

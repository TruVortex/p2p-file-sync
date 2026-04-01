# AGENTS.md

## Project Vision: High-Performance P2P File Synchronization
This project is a decentralized, content-addressable file synchronization engine. The goal is to achieve $O(\log N)$ state reconciliation and minimal bandwidth consumption through delta-encoded transfers.

## 1. Technical Philosophy
*   **Content-Addressability over Naming:** Files are secondary; Blobs are primary. Every piece of data is identified by its SHA-256 hash.
*   **Zero-Trust Networking:** All communication is encrypted via TLS/Noise. Peer identity is verified via public-key fingerprints.
*   **Minimal Dependencies:** Favor the Go standard library (`net`, `crypto`, `io`). Avoid "bloatware" frameworks. If a library is used (e.g., `libp2p`), the agent should explain *why* and how the underlying protocol (DHT/Kademlia) functions.
*   **Performance-First:** Use `io.Reader` and `io.Writer` interfaces to stream data. Avoid loading entire large files into memory (RAM-efficient).

## 2. Core Architecture Modules

### A. The Blob Store (CAS)
*   **Storage:** Data is split into 4KB chunks.
*   **Pathing:** Chunks are stored at `.data/chunks/[first-two-chars-of-hash]/[remaining-hash]`.
*   **Deduplication:** Before writing a chunk, check for existence. If it exists, update the reference count only.

### B. The Merkle Tree (State Sync)
*   **Implementation:** Build a Prefix Hash Tree or a standard Merkle Tree representing the local filesystem state.
*   **Reconciliation Algorithm:** 
    1. Exchange Root Hashes.
    2. If different, descend to children.
    3. Identify "Dirty" leaf nodes.
    4. Request only missing hashes.

### C. The Rolling Hash (Rsync-lite)
*   **Constraint:** Use a sliding window (Adler-32 or Rabin Fingerprint) to detect byte-level shifts in files.
*   **Output:** Generate a "Signature" of the remote file and a "Delta" of the local file.

### D. Networking (The Transport)
*   **Protocol:** Prefer QUIC for its multiplexing and head-of-line blocking avoidance.
*   **NAT Traversal:** Implement STUN-based UDP Hole Punching logic. If NAT is symmetric, fallback to a Relay node.

## 3. Implementation Rules for AI Agents
When generating code for this repository, follow these rules:

1.  **Concurrency:** Use `context.Context` for all network and long-running operations. Use worker pools for hashing large directories to prevent goroutine explosion.
2.  **Error Handling:** No `panic()`. Wrap errors with context (e.g., `fmt.Errorf("hashing failed: %w", err)`).
3.  **Memory Management:** Use `sync.Pool` for frequently allocated buffers during the rolling hash process.
4.  **Binary Protocol:** Use `encoding/binary` or Protobuf for the wire protocol. **No JSON over TCP.**
5.  **Idempotency:** All sync operations must be idempotent. Re-running a sync on an identical folder should result in zero disk I/O and zero network traffic after the Merkle Root comparison.

## 4. Key Data Structures
```go
type Chunk struct {
    Hash     []byte // SHA-256
    Size     uint32
    Data     []byte // Only populated during transit
}

type FileManifest struct {
    Path       string
    Mode       uint32
    MerkleRoot []byte
    Chunks     []byte // Ordered list of chunk hashes
}
```

## 5. Development Roadmap (Context for Agent)
1.  **Phase 1:** Local CAS and File-to-Chunker logic.
2.  **Phase 2:** Merkle Tree generation and diffing logic.
3.  **Phase 3:** P2P Handshake (mTLS) and manifest exchange.
4.  **Phase 4:** Delta-sync implementation for modified chunks.
5.  **Phase 5:** NAT Traversal and Signaling server.

## 6. Interview "Talking Points" to Protect
If the agent suggests a simplification, ensure it doesn't remove these "FAANG-level" features:
*   **The "Thundering Herd" Problem:** How do we handle 10,000 files changing at once? (Rate limiting/Batching).
*   **Hash Collisions:** Why SHA-256 and what is the birthday paradox probability in this context?
*   **Backpressure:** Using bounded channels to ensure the network doesn't outpace the disk.
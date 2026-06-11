# P2P File Sync Engine - Architecture Documentation

## Overview

This document describes the architecture of a high-performance, decentralized file synchronization engine. The system achieves O(log N) state reconciliation through Merkle trees and minimizes bandwidth via content-addressable storage with delta encoding.

## System Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                           P2P Sync Engine                           │
├─────────────────────────────────────────────────────────────────────┤
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌────────────┐  │
│  │     CLI     │  │   Watcher   │  │   Metrics   │  │  Logging   │  │
│  │   (cobra)   │  │  (fsnotify) │  │(prometheus) │  │   (slog)   │  │
│  └──────┬──────┘  └──────┬──────┘  └──────┬──────┘  └─────┬──────┘  │
│         │                │                │                │        │
├─────────┴────────────────┴────────────────┴────────────────┴────────┤
│                         Sync Manager                                │
│   ┌─────────────────────────────────────────────────────────────┐   │
│   │  • Coordinates peer synchronization                         │   │
│   │  • Manages concurrent chunk transfers                       │   │
│   │  • Implements backpressure via bounded channels             │   │
│   └─────────────────────────────────────────────────────────────┘   │
├─────────────────────────────────────────────────────────────────────┤
│         ┌──────────────┐              ┌──────────────────┐          │
│         │  Merkle Tree │◄────────────►│   Delta Sync     │          │
│         │    (diff)    │              │ (rolling hash)   │          │
│         └──────┬───────┘              └────────┬─────────┘          │
│                │                               │                    │
│                ▼                               ▼                    │
│   ┌────────────────────────────────────────────────────────┐        │
│   │             Content-Addressable Storage (CAS)          │        │
│   │  ┌──────────┐  ┌──────────┐  ┌──────────────────────┐  │        │
│   │  │ Chunker  │  │BlobStore │  │   Deduplication      │  │        │
│   │  │ (4KB)    │  │ (sharded)│  │   (hash lookup)      │  │        │
│   │  └──────────┘  └──────────┘  └──────────────────────┘  │        │
│   └────────────────────────────────────────────────────────┘        │
├─────────────────────────────────────────────────────────────────────┤
│                        Network Layer                                │
│   ┌─────────────┐  ┌─────────────┐  ┌──────────────────────┐        │
│   │    QUIC     │  │    NAT      │  │     Signaling        │        │
│   │ (transport) │  │ (traversal) │  │     (discovery)      │        │
│   └─────────────┘  └─────────────┘  └──────────────────────┘        │
├─────────────────────────────────────────────────────────────────────┤
│                         Persistence                                 │
│   ┌─────────────────────────┐  ┌────────────────────────────┐       │
│   │     SQLite (metadata)   │  │    Filesystem (chunks)     │       │
│   │  • File paths → hashes  │  │  • .storage/chunks/ab/...  │       │
│   │  • Snapshot versioning  │  │  • Atomic writes           │       │
│   └─────────────────────────┘  └────────────────────────────┘       │
└─────────────────────────────────────────────────────────────────────┘
```

## Core Components

### 1. Content-Addressable Storage (CAS)

**Location:** `internal/cas/`

The CAS layer is the foundation of the sync engine. Every piece of data is identified by its SHA-256 hash.

#### Chunker (`chunk.go`)
- Splits files into fixed 4KB blocks
- Uses `sync.Pool` for buffer reuse to minimize GC pressure
- Returns ordered list of chunk hashes

```go
type Chunk struct {
    Hash []byte  // SHA-256 (32 bytes)
    Size uint32
    Data []byte  // Only populated during transit
}
```

#### BlobStore (`store.go`)
- Stores chunks in sharded directories: `.storage/chunks/ab/cdef...`
- Sharding prevents filesystem bloat (256 possible first-level directories)
- Atomic writes via temp file + rename
- Reference counting for deduplication

**Deduplication Strategy:**
```
1. Compute hash of new chunk
2. Check if hash exists in store
3. If exists → increment reference count, skip write
4. If not → write to temp file, rename atomically
```

### 2. Merkle Tree

**Location:** `internal/merkle/`

The Merkle tree represents the entire filesystem state in a hash tree structure, enabling O(log N) difference detection.

#### Node Types
```go
const (
    NodeTypeFile = iota  // Leaf node - contains chunk hashes
    NodeTypeDir          // Internal node - contains child hashes
)
```

#### Tree Structure
```
            Root (hash of children)
           /          \
      Dir A           Dir B
     /    \          /    \
  File1  File2    File3  Dir C
   │        │       │       │
[chunks] [chunks] [chunks] [children]
```

#### Diff Algorithm
The `Diff(local, remote *Node)` function achieves O(log N) by only descending into subtrees with different hashes:

```
1. Compare root hashes
2. If equal → no changes, return empty diff
3. If different → compare children
4. For each child:
   a. If only in local → mark as deleted
   b. If only in remote → mark as added
   c. If in both but different hash → recurse
5. Collect "dirty" nodes at leaf level
```

### 3. Delta Sync (Rsync Algorithm)

**Location:** `internal/delta/`

For files that exist on both sides but have changed, we use rolling hash to transfer only the differences.

#### Rolling Hash (`rolling.go`)
- Adler-32 style weak hash for fast matching
- SHA-256 strong hash for verification
- O(1) window slide operation

#### Delta Generation (`delta.go`)
```
Sender                              Receiver
  │                                     │
  │◄────── Signature (block hashes) ────│
  │                                     │
  │  1. Slide rolling hash through file │
  │  2. On weak hash match:             │
  │     - Verify with strong hash       │
  │     - Emit OpCopy if match          │
  │  3. On no match:                    │
  │     - Emit OpData (literal bytes)   │
  │                                     │
  │─────────── Delta (ops) ────────────►│
  │                                     │
```

### 4. Network Layer

**Location:** `internal/net/`, `internal/nat/`

#### QUIC Transport (`transport.go`)
- Built-in TLS encryption
- Stream multiplexing over single connection
- Head-of-line blocking avoidance

#### Peer Identity
Peers are identified by SHA-256 fingerprint of their TLS certificate:
```go
peerID := sha256.Sum256(cert.Raw)
```

#### NAT Traversal (`nat/`)
1. **STUN** - Discover public IP:port
2. **Signaling Server** - Exchange peer addresses
3. **UDP Hole Punching** - Simultaneous open
4. **Relay Fallback** - TCP proxy for symmetric NAT

## Concurrency Model

### Thread Safety Guarantees

#### 1. BlobStore Writes
```go
// Atomic write pattern - prevents partial/corrupt chunks
func (s *BlobStore) Put(hash []byte, data []byte) error {
    tempPath := s.tempPath()
    finalPath := s.chunkPath(hash)
    
    // Write to temp file
    if err := os.WriteFile(tempPath, data, 0644); err != nil {
        return err
    }
    
    // Atomic rename (POSIX guarantees atomicity)
    return os.Rename(tempPath, finalPath)
}
```

#### 2. Merkle Tree Updates
```go
// Rebuilding uses copy-on-write semantics
func rebuildTree(root *Node, changes []Change) *Node {
    // Create new tree with modifications
    newRoot := deepCopy(root)
    
    for _, change := range changes {
        applyChange(newRoot, change)
    }
    
    // Old tree remains valid until swap
    return newRoot
}
```

#### 3. Concurrent Chunk Transfers
```go
// Worker pool with bounded channel prevents goroutine explosion
const maxConcurrentTransfers = 10

func (sm *SyncManager) transferChunks(chunks [][]byte) {
    sem := make(chan struct{}, maxConcurrentTransfers)
    var wg sync.WaitGroup
    
    for _, chunk := range chunks {
        sem <- struct{}{}  // Acquire semaphore
        wg.Add(1)
        
        go func(hash []byte) {
            defer wg.Done()
            defer func() { <-sem }()  // Release semaphore
            
            sm.fetchChunk(hash)
        }(chunk)
    }
    
    wg.Wait()
}
```

### Race Condition Prevention

#### File Write Conflicts
**Problem:** Multiple sync operations could try to write the same file.

**Solution:** Write-ahead locking with file-level mutex map:
```go
type FileLocker struct {
    mu    sync.Mutex
    locks map[string]*sync.Mutex
}

func (fl *FileLocker) Lock(path string) {
    fl.mu.Lock()
    lock, exists := fl.locks[path]
    if !exists {
        lock = &sync.Mutex{}
        fl.locks[path] = lock
    }
    fl.mu.Unlock()
    
    lock.Lock()  // Acquire file-specific lock
}
```

#### Chunk Deduplication Race
**Problem:** Two goroutines check "chunk exists?" simultaneously, both see false.

**Solution:** Check-then-write with atomic flag:
```go
func (s *BlobStore) PutIfAbsent(hash []byte, data []byte) (bool, error) {
    path := s.chunkPath(hash)
    
    // Use O_EXCL flag - fails if file exists
    f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
    if os.IsExist(err) {
        return false, nil  // Already exists, deduplicated
    }
    if err != nil {
        return false, err
    }
    defer f.Close()
    
    _, err = f.Write(data)
    return true, err
}
```

#### Database Transaction Isolation
```go
func (db *Store) Transaction(fn func(tx *sql.Tx) error) error {
    tx, err := db.db.Begin()
    if err != nil {
        return err
    }
    
    defer func() {
        if p := recover(); p != nil {
            tx.Rollback()
            panic(p)
        }
    }()
    
    if err := fn(tx); err != nil {
        tx.Rollback()
        return err
    }
    
    return tx.Commit()
}
```

### Backpressure Mechanisms

#### 1. Network → Disk
```go
// Bounded channel prevents memory exhaustion
chunkQueue := make(chan Chunk, 100)  // Max 100 chunks buffered

// Producer (network)
select {
case chunkQueue <- chunk:
    // Sent successfully
case <-time.After(5 * time.Second):
    // Backpressure - slow down network reads
    conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
}

// Consumer (disk)
for chunk := range chunkQueue {
    store.Put(chunk.Hash, chunk.Data)
}
```

#### 2. Watcher → Sync
```go
// Debounce rapid file changes
type Debouncer struct {
    events  map[string]time.Time
    delay   time.Duration
    mu      sync.Mutex
}

func (d *Debouncer) ShouldProcess(path string) bool {
    d.mu.Lock()
    defer d.mu.Unlock()
    
    lastEvent, exists := d.events[path]
    if exists && time.Since(lastEvent) < d.delay {
        return false  // Too soon, skip
    }
    
    d.events[path] = time.Now()
    return true
}
```

## Data Flow

### Sync Operation Flow

```
┌──────────────────────────────────────────────────────────────────┐
│                        SYNC OPERATION FLOW                       │
└──────────────────────────────────────────────────────────────────┘

  Local Peer                                          Remote Peer
      │                                                    │
      │  1. Build local Merkle tree                        │
      │────────────────────────────────────────────────────│
      │                                                    │
      │  2. Exchange root hashes                           │
      │◄──────────────────────────────────────────────────►│
      │                                                    │
      │  3. If roots differ, compute Diff()                │
      │────────────────────────────────────────────────────│
      │                                                    │
      │  4. For each dirty file:                           │
      │     a. Request signature (if file exists locally)  │
      │◄──────────────────────────────────────────────────►│
      │     b. Generate delta                              │
      │────────────────────────────────────────────────────│
      │     c. Apply delta / fetch full chunks             │
      │◄──────────────────────────────────────────────────►│
      │                                                    │
      │  5. Update local Merkle tree                       │
      │────────────────────────────────────────────────────│
      │                                                    │
      │  6. Verify root hashes match                       │
      │◄──────────────────────────────────────────────────►│
      │                                                    │
```

### Chunk Storage Flow

```
File → Chunker → Hash Check → [exists?] → Yes → Dedupe (ref count)
                      │
                      └→ No → Write to temp → Atomic rename → Index
```

## Performance Characteristics

| Operation | Time Complexity | Notes |
|-----------|-----------------|-------|
| Merkle Diff | O(log N) | Only descends differing subtrees |
| Chunk Lookup | O(1) | Hash-based addressing |
| Delta Generation | O(N) | Linear scan with rolling hash |
| Full File Sync | O(N) | Proportional to file size |
| Directory Scan | O(F) | F = number of files |

## Memory Management

### Buffer Pools
```go
// Chunk buffer pool - avoids allocation per chunk
var chunkPool = sync.Pool{
    New: func() interface{} {
        return make([]byte, 4096)
    },
}

// Usage
buf := chunkPool.Get().([]byte)
defer chunkPool.Put(buf)
```

### Large File Streaming
```go
// Never load full file into memory
func processLargeFile(path string) error {
    f, _ := os.Open(path)
    defer f.Close()
    
    reader := bufio.NewReaderSize(f, 64*1024)  // 64KB buffer
    
    for {
        chunk, err := readNextChunk(reader)
        if err == io.EOF {
            break
        }
        processChunk(chunk)  // Process immediately, don't accumulate
    }
}
```

## Security Model

### Transport Security
- All connections use TLS 1.3 via QUIC
- Self-signed certificates with fingerprint verification
- No certificate authorities required (zero-trust)

### Content Integrity
- SHA-256 for all content addressing
- Chunks verified on read via `Verify()` function
- Merkle root provides cryptographic commitment to entire state

### Peer Authentication
```go
// Verify peer identity matches expected fingerprint
func verifyPeer(cert *x509.Certificate, expectedFingerprint [32]byte) bool {
    actualFingerprint := sha256.Sum256(cert.Raw)
    return actualFingerprint == expectedFingerprint
}
```

## Observability

### Structured Logging (slog)
```go
logging.Info("sync completed",
    "peer_id", peerID,
    "chunks_transferred", 42,
    "bytes_total", 172032,
    "duration_ms", 1523,
)
```

### Prometheus Metrics
- `p2psync_transfer_bytes_total` - Bytes uploaded/downloaded
- `p2psync_chunks_deduplicated_total` - Deduplication count
- `p2psync_merkle_tree_depth` - Current tree depth
- `p2psync_delta_compression_ratio` - Delta efficiency

### Health Endpoints
- `/health` - Liveness check
- `/metrics` - Prometheus scrape endpoint

## Error Handling Philosophy

1. **No panics** - All errors wrapped with context
2. **Idempotent operations** - Safe to retry
3. **Graceful degradation** - Partial sync > no sync
4. **Clear error messages** - Include file paths, hashes, peer IDs

```go
// Error wrapping pattern
if err != nil {
    return fmt.Errorf("syncing file %s from peer %s: %w", path, peerID, err)
}
```

## Future Considerations

### Scalability Improvements
- [ ] Parallel Merkle tree construction
- [ ] Tiered storage (hot/cold chunks)
- [ ] Chunk compression (zstd)

### Feature Extensions
- [ ] Conflict resolution strategies
- [ ] Selective sync (gitignore-style)
- [ ] Encryption at rest

### Operational
- [ ] Distributed tracing (OpenTelemetry)
- [ ] Rate limiting per peer
- [ ] Bandwidth scheduling

# P2P File Sync

A high-performance, decentralized file synchronization engine built in Go. Uses content-addressable storage, Merkle trees, and rsync-style delta encoding to minimize bandwidth and achieve O(log N) state reconciliation.

## Features

### Core Technology
- **Content-Addressable Storage (CAS)** - Data split into 4KB chunks, identified by SHA-256 hashes
- **Merkle Tree Diffing** - O(log N) directory state reconciliation
- **Delta Sync** - Rsync algorithm for bandwidth-efficient transfers of modified files
- **Zero-Trust Networking** - TLS-encrypted QUIC transport with certificate fingerprint authentication

### Networking
- **QUIC Transport** - Multiplexed streams over UDP with built-in encryption
- **NAT Traversal** - STUN for public IP discovery, UDP hole punching, TCP relay fallback
- **Protocol Buffers** - Efficient binary wire protocol

### Performance & Observability
- **Memory Efficient** - Streaming I/O with `sync.Pool` for buffer reuse
- **Structured Logging** - JSON/text output via `slog`
- **Prometheus Metrics** - Upload/download speed, deduplication stats, Merkle depth
- **Progress Bars** - Real-time sync progress visualization

## Architecture

```
┌─────────────────────────────────────────────────────┐
│  CLI (cobra + mpb progress bars)                    │
├─────────────────────────────────────────────────────┤
│  Sync Manager                                       │
│  ├─ Merkle Tree Builder & Differ                    │
│  ├─ Delta Generator (rolling hash)                  │
│  └─ File Watcher (fsnotify)                         │
├─────────────────────────────────────────────────────┤
│  Transport Layer (QUIC + Protocol Buffers)          │
│  ├─ TLS Handshake (cert fingerprint auth)           │
│  ├─ Chunk Request/Response                          │
│  └─ Manifest Exchange                               │
├─────────────────────────────────────────────────────┤
│  NAT Traversal                                      │
│  ├─ STUN Client                                     │
│  ├─ UDP Hole Puncher                                │
│  └─ TCP Relay (fallback)                            │
├─────────────────────────────────────────────────────┤
│  Storage                                            │
│  ├─ Blob Store (CAS with sharding)                  │
│  └─ SQLite Metadata (file versions, snapshots)      │
└─────────────────────────────────────────────────────┘
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for detailed concurrency model and design decisions.

## Quick Start

### Installation

Download pre-built binaries from [Releases](../../releases) or build from source:

```bash
# Clone repository
git clone https://github.com/yourusername/p2p-file-sync.git
cd p2p-file-sync

# Build (Git Bash/Linux/macOS)
make build

# Or build all platforms
make build-all

# Windows PowerShell
.\build-all.ps1
```

### Basic Usage

```bash
# Initialize a directory
p2p-file-sync init ~/Documents/sync-folder

# Start the sync daemon
p2p-file-sync serve --metrics :9090

# Sync with a peer
p2p-file-sync sync 192.168.1.100:4433

# Watch directory for changes
p2p-file-sync watch ~/Documents/sync-folder

# Check status
p2p-file-sync status

# View version
p2p-file-sync version
```

### Configuration

Global flags available for all commands:

| Flag | Description | Default |
|------|-------------|---------|
| `-d, --dir` | Directory to sync | Current directory |
| `-v, --verbose` | Enable debug logging | false |
| `--json` | Output logs in JSON format | false |
| `--metrics` | Prometheus metrics endpoint | disabled |

## Commands

### `init [directory]`
Initialize a directory for P2P sync. Creates `.storage/chunks/` for content-addressable storage.

```bash
p2p-file-sync init ~/Documents/shared
```

### `serve`
Start the sync daemon. Listens for incoming peer connections and serves chunk requests.

```bash
# Basic
p2p-file-sync serve

# With metrics
p2p-file-sync serve --metrics :9090
```

Metrics available at `http://localhost:9090/metrics`:
- `p2p_sync_bytes_uploaded` - Total bytes uploaded
- `p2p_sync_bytes_downloaded` - Total bytes downloaded
- `p2p_sync_chunks_deduplicated` - Chunks skipped due to deduplication
- `p2p_sync_merkle_depth` - Current Merkle tree depth

### `sync <peer-address>`
Synchronize with a remote peer.

```bash
# Direct connection (LAN or known IP)
p2p-file-sync sync 192.168.1.50:4433
```

**For peers behind NAT**, use the signaling server for peer discovery and hole punching:

```bash
# Start the signaling server (on a VPS with public IP)
p2p-file-sync signal --addr :9000

# Peer A: Register and wait for Peer B
p2p-file-sync sync --signal wss://signal.example.com:9000 --peer-id alice

# Peer B: Connect to Peer A via signaling
p2p-file-sync sync --signal wss://signal.example.com:9000 --peer-id bob --target alice
```

**How NAT Traversal Works:**

```
┌─────────┐         ┌─────────────────┐         ┌─────────┐
│ Peer A  │◄───────►│ Signaling Server│◄───────►│ Peer B  │
│ (NAT)   │         │ (Public VPS)    │         │ (NAT)   │
└────┬────┘         └────────┬────────┘         └────┬────┘
     │                       │                       │
     │  1. Register          │         1. Register   │
     │──────────────────────►│◄──────────────────────│
     │                       │                       │
     │  2. Exchange public IP:port via STUN          │
     │◄─────────────────────►│◄─────────────────────►│
     │                       │                       │
     │  3. UDP Hole Punch (SYN/SYN-ACK/ACK)          │
     │◄─────────────────────────────────────────────►│
     │                       │                       │
     │  4. Direct QUIC connection established        │
     │◄═════════════════════════════════════════════►│
```

If UDP hole punching fails after 5 attempts (common with symmetric NAT), the system automatically falls back to a TCP relay.

### `watch [directory]`
Watch directory for file changes and automatically trigger re-chunking and Merkle tree updates.

```bash
p2p-file-sync watch ~/Documents/shared
```

### `status`
Display current sync status, Merkle root hash, file count, and storage size.

```bash
p2p-file-sync status
```

## Building

### Prerequisites
- Go 1.21+
- protoc (Protocol Buffers compiler)
- Make (optional, for Unix-like systems)

### Build Commands

```bash
# Download dependencies
go mod download

# Run tests
go test ./...

# Build for current platform
go build -o build/p2p-file-sync ./cmd/p2p-file-sync

# Cross-platform builds
make build-all        # Unix-like systems
.\build-all.ps1       # Windows PowerShell
```

### Generate Protocol Buffers

If you modify `internal/proto/sync.proto`:

```bash
make proto
# or
protoc --go_out=. --go_opt=paths=source_relative internal/proto/sync.proto
```

## Development

### Project Structure

```
p2p-file-sync/
├── cmd/p2p-file-sync/    # CLI entry point
├── internal/
│   ├── cas/              # Content-addressable storage (chunker, blob store)
│   ├── db/               # SQLite metadata layer
│   ├── delta/            # Rsync-style delta sync (rolling hash)
│   ├── logging/          # Structured logging with slog
│   ├── merkle/           # Merkle tree construction and diffing
│   ├── metrics/          # Prometheus metrics
│   ├── nat/              # NAT traversal (STUN, hole punch, relay)
│   ├── net/              # QUIC transport and sync manager
│   ├── proto/            # Protocol Buffers definitions
│   └── watcher/          # File system watcher (fsnotify)
├── build-all.ps1         # Windows build script
├── Makefile              # Unix build automation
└── go.mod
```

### Running Tests

```bash
# All tests
go test ./...

# With coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Specific package
go test ./internal/cas/

# Verbose output
go test -v ./...
```

### Code Quality

```bash
# Format code
go fmt ./...

# Vet code
go vet ./...

# Run linter (requires golangci-lint)
golangci-lint run ./...
```

## Design Trade-offs

Key architectural decisions and their rationale:

### Fixed vs. Variable Chunking
Chose **fixed 4KB chunks** for simplicity in the initial CAS implementation. Variable-size chunking (Content-Defined Chunking with Rabin fingerprints) handles "byte-shift" scenarios better—where inserting bytes at the start of a file doesn't invalidate all subsequent chunks—but adds ~15-20% CPU overhead and implementation complexity. The 4KB size aligns with OS page size for efficient I/O.

### QUIC vs. TCP
**QUIC** was chosen over TCP for three reasons:
1. **No Head-of-Line Blocking** - Multiple streams can be multiplexed; a lost packet only blocks its stream, not the entire connection
2. **Faster Connection Migration** - As laptops/phones switch networks (WiFi → cellular), QUIC connections survive IP changes
3. **Built-in TLS 1.3** - Zero-RTT connection establishment and mandatory encryption

### SHA-256 vs. Faster Hashes
**SHA-256** was chosen despite being slower than xxHash/Blake3 because:
1. Collision resistance matters for content-addressing (birthday paradox: 2^128 operations needed)
2. Widely trusted in production systems (Git, Bitcoin, TLS)
3. Hardware acceleration (SHA-NI) available on modern CPUs

### Merkle Trees vs. Bloom Filters
**Merkle Trees** enable precise O(log N) identification of differing files. Bloom filters would be faster for "do we need to sync?" checks but provide only probabilistic answers and can't pinpoint *which* files differ.

### SQLite vs. Custom Index
**SQLite** (CGO-free via modernc.org/sqlite) provides ACID transactions for metadata without reinventing indexing. Trade-off: ~5MB binary size increase, but gains battle-tested reliability.

## Performance Characteristics

| Operation | Time Complexity | Notes |
|-----------|----------------|-------|
| Chunk deduplication | O(1) | Hash table lookup |
| Merkle tree diff | O(log N) | Only descends into changed subtrees |
| File chunking | O(n) | Streaming, constant memory |
| Delta generation | O(n) | Rolling hash with backpressure |

### Memory Usage
- **Chunker**: 4KB per chunk + sync.Pool buffers
- **Merkle Tree**: O(N) nodes, where N = file count
- **Delta Sync**: Constant memory (streaming)
- **Network**: Bounded channels prevent memory explosion

## Security

- **TLS 1.3** for all QUIC connections
- **Peer Authentication** via SHA-256 certificate fingerprints
- **No plaintext** - all data encrypted in transit
- **Content Addressing** ensures data integrity (tampered chunks fail hash verification)

## Known Limitations

1. **No conflict resolution** - Last-write-wins model (no CRDTs)
2. **Single writer** - Concurrent writes to same file may cause issues
3. **NAT traversal** - Symmetric NAT requires relay fallback
4. **Large files** - Very large files (>1GB) are chunked but may take time for initial hash

## Roadmap

- [ ] Conflict resolution with vector clocks
- [ ] Distributed hash table (DHT) for peer discovery
- [ ] Selective sync (ignore patterns)
- [ ] Compression (zstd) for chunk storage
- [ ] Web UI for monitoring

## Contributing

This project was built as a technical demonstration of:
- Content-addressable storage systems
- Merkle tree-based state reconciliation
- Efficient delta encoding algorithms
- NAT traversal techniques
- Modern Go concurrency patterns

See [ARCHITECTURE.md](ARCHITECTURE.md) for implementation details.

## License

MIT License - see LICENSE file for details.

## Acknowledgments

Built with:
- [quic-go](https://github.com/quic-go/quic-go) - QUIC implementation
- [cobra](https://github.com/spf13/cobra) - CLI framework
- [mpb](https://github.com/vbauerster/mpb) - Progress bars
- [fsnotify](https://github.com/fsnotify/fsnotify) - File system watcher
- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) - CGO-free SQLite
- Protocol Buffers - Efficient serialization

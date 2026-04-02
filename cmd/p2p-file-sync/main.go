// Package main provides the CLI for the P2P file sync engine.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"p2p-file-sync/internal/cas"
	"p2p-file-sync/internal/logging"
	"p2p-file-sync/internal/merkle"
	"p2p-file-sync/internal/metrics"
	"p2p-file-sync/internal/net"
	"p2p-file-sync/internal/proto"
)

var (
	// Global flags
	verbose     bool
	jsonLogs    bool
	metricsAddr string
	dataDir     string

	// Version info (set by build)
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "p2p-file-sync",
	Short: "P2P File Synchronization Engine",
	Long: `A high-performance, decentralized file synchronization engine using
content-addressable storage, Merkle trees, and delta encoding.

Features:
  • Content-addressable storage with SHA-256 chunk hashing
  • O(log N) Merkle tree diffing for efficient sync
  • Rsync-style delta encoding for bandwidth optimization
  • NAT traversal with UDP hole punching
  • QUIC transport with TLS encryption`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Initialize logging
		level := slog.LevelInfo
		if verbose {
			level = slog.LevelDebug
		}

		format := "text"
		if jsonLogs {
			format = "json"
		}

		logging.Init(&logging.Config{
			Level:  level,
			Format: format,
			Output: os.Stderr,
		})
	},
}

var initCmd = &cobra.Command{
	Use:   "init [directory]",
	Short: "Initialize a directory for P2P sync",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := "."
		if len(args) > 0 {
			dir = args[0]
		}

		absDir, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("invalid directory: %w", err)
		}

		// Create storage directory
		storageDir := filepath.Join(absDir, ".storage", "chunks")
		if err := os.MkdirAll(storageDir, 0755); err != nil {
			return fmt.Errorf("creating storage directory: %w", err)
		}

		logging.Info("initialized sync directory",
			"path", absDir,
			"storage", storageDir)

		fmt.Printf("✓ Initialized P2P sync in %s\n", absDir)
		return nil
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show sync status",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, _ := cmd.Flags().GetString("dir")
		if dir == "" {
			dir = "."
		}

		absDir, _ := filepath.Abs(dir)
		storageDir := filepath.Join(absDir, ".storage", "chunks")

		// Check if initialized
		if _, err := os.Stat(storageDir); os.IsNotExist(err) {
			return fmt.Errorf("directory not initialized - run 'p2p-file-sync init' first")
		}

		// Build Merkle tree and show stats
		fmt.Println("Building file index...")

		tree, err := merkle.BuildFromFS(absDir)
		if err != nil {
			return fmt.Errorf("building tree: %w", err)
		}

		// Count chunks in storage
		chunkCount := 0
		var totalSize int64
		filepath.Walk(storageDir, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				chunkCount++
				totalSize += info.Size()
			}
			return nil
		})

		// Calculate stats
		depth := calculateTreeDepth(tree.Root)
		nodeCount := countNodes(tree.Root)
		fileCount := countFiles(tree.Root)

		rootHash := "(empty)"
		if tree.Root != nil && len(tree.Root.Hash) >= 8 {
			rootHash = fmt.Sprintf("%x...", tree.Root.Hash[:8])
		}

		fmt.Printf("\n📁 Sync Status: %s\n", absDir)
		fmt.Printf("   Root Hash:    %s\n", rootHash)
		fmt.Printf("   Files:        %d\n", fileCount)
		fmt.Printf("   Tree Nodes:   %d\n", nodeCount)
		fmt.Printf("   Tree Depth:   %d\n", depth)
		fmt.Printf("   Chunks:       %d (%.2f MB)\n", chunkCount, float64(totalSize)/(1024*1024))

		// Update metrics
		metrics.SetMerkleTreeStats(depth, nodeCount)

		return nil
	},
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the sync daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		port, _ := cmd.Flags().GetInt("port")
		dir, _ := cmd.Flags().GetString("dir")
		if dir == "" {
			dir = "."
		}

		absDir, _ := filepath.Abs(dir)
		storageDir := filepath.Join(absDir, ".storage", "chunks")

		if _, err := os.Stat(storageDir); os.IsNotExist(err) {
			return fmt.Errorf("directory not initialized - run 'p2p-file-sync init' first")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Handle interrupt
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

		// Initialize blob store
		store, err := cas.NewBlobStore(storageDir)
		if err != nil {
			return fmt.Errorf("creating blob store: %w", err)
		}

		// Chunk all files in the directory
		fmt.Println("📦 Indexing files...")
		fileCount, chunkCount, err := indexDirectory(absDir, store)
		if err != nil {
			return fmt.Errorf("indexing directory: %w", err)
		}
		fmt.Printf("   Indexed %d files, %d chunks\n", fileCount, chunkCount)

		// Build Merkle tree
		tree, err := merkle.BuildFromFS(absDir)
		if err != nil {
			return fmt.Errorf("building merkle tree: %w", err)
		}

		var merkleRoot []byte
		if tree.Root != nil {
			merkleRoot = tree.Root.Hash
		}

		// Create transport with callbacks
		listenAddr := fmt.Sprintf(":%d", port)
		transport, err := net.NewTransport(&net.Config{
			ListenAddr: listenAddr,
			PeerName:   fmt.Sprintf("server-%d", port),
			MerkleRoot: merkleRoot,
			FileCount:  uint64(fileCount),
			TotalSize:  0,
			OnPeerConnected: func(peer *net.Peer) {
				fmt.Printf("✓ Peer connected: %s (%s)\n", peer.Name, peer.ID.Short())
				logging.Info("peer connected",
					"name", peer.Name,
					"id", peer.ID.Short(),
					"addr", peer.Addr)
			},
			OnPeerDisconnected: func(peer *net.Peer) {
				fmt.Printf("✗ Peer disconnected: %s\n", peer.Name)
				logging.Info("peer disconnected", "name", peer.Name)
			},
			OnChunkRequest: func(peer *net.Peer, req *proto.ChunkRequest) *proto.ChunkResponse {
				logging.Info("chunk request received",
					"peer", peer.Name,
					"chunks", len(req.ChunkHashes))

				resp := &proto.ChunkResponse{
					Chunks:   make([]*proto.Chunk, 0, len(req.ChunkHashes)),
					NotFound: make([][]byte, 0),
				}

				for _, hash := range req.ChunkHashes {
					chunk, err := store.Get(hash)
					if err != nil {
						resp.NotFound = append(resp.NotFound, hash)
						continue
					}
					resp.Chunks = append(resp.Chunks, &proto.Chunk{
						Hash: hash,
						Data: chunk.Data,
						Size: uint32(len(chunk.Data)),
					})
				}

				logging.Info("sending chunks",
					"found", len(resp.Chunks),
					"not_found", len(resp.NotFound))

				return resp
			},
			OnManifestRequest: func(peer *net.Peer, req *proto.ManifestRequest) *proto.ManifestResponse {
				logging.Info("manifest request received", "peer", peer.Name)

				// Convert merkle tree to manifest
				root := convertTreeToManifest(tree.Root)
				return &proto.ManifestResponse{
					Root:      root,
					NodeCount: uint64(countNodes(tree.Root)),
				}
			},
		})
		if err != nil {
			return fmt.Errorf("creating transport: %w", err)
		}

		// Start listening
		if err := transport.Listen(); err != nil {
			return fmt.Errorf("starting listener: %w", err)
		}

		// Start metrics server if enabled
		if metricsAddr != "" {
			metricsServer := metrics.NewServer(metricsAddr)
			go func() {
				logging.Info("starting metrics server", "addr", metricsAddr)
				if err := metricsServer.Start(); err != nil && err != http.ErrServerClosed {
					logging.Error("metrics server error", "error", err)
				}
			}()
			defer metricsServer.Stop()
		}

		fmt.Printf("\n🚀 P2P Sync daemon started\n")
		fmt.Printf("   Directory: %s\n", absDir)
		fmt.Printf("   Address:   %s\n", listenAddr)
		fmt.Printf("   Peer ID:   %s\n", transport.LocalID().Short())
		if merkleRoot != nil {
			fmt.Printf("   Root Hash: %x...\n", merkleRoot[:8])
		}
		if metricsAddr != "" {
			fmt.Printf("   Metrics:   http://%s/metrics\n", metricsAddr)
		}
		fmt.Println("\nWaiting for connections... (Ctrl+C to stop)")

		logging.Info("daemon started",
			"dir", absDir,
			"addr", listenAddr,
			"metrics", metricsAddr)

		select {
		case <-sigCh:
			fmt.Println("\n⏹ Shutting down...")
			cancel()
		case <-ctx.Done():
		}

		transport.Close()
		return nil
	},
}

var syncCmd = &cobra.Command{
	Use:   "sync [peer-address]",
	Short: "Sync with a peer",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		peerAddr := args[0]
		dir, _ := cmd.Flags().GetString("dir")
		if dir == "" {
			dir = "."
		}

		absDir, _ := filepath.Abs(dir)
		storageDir := filepath.Join(absDir, ".storage", "chunks")

		if _, err := os.Stat(storageDir); os.IsNotExist(err) {
			return fmt.Errorf("directory not initialized - run 'p2p-file-sync init' first")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Handle interrupt
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Println("\n⏹ Sync cancelled")
			cancel()
		}()

		// Initialize blob store
		store, err := cas.NewBlobStore(storageDir)
		if err != nil {
			return fmt.Errorf("creating blob store: %w", err)
		}

		// Build local Merkle tree
		fmt.Println("📊 Building local tree...")
		localTree, err := merkle.BuildFromFS(absDir)
		if err != nil {
			return fmt.Errorf("building merkle tree: %w", err)
		}

		var localRoot []byte
		if localTree.Root != nil {
			localRoot = localTree.Root.Hash
		}

		// Create transport (client mode - no listen address)
		transport, err := net.NewTransport(&net.Config{
			PeerName:   "sync-client",
			MerkleRoot: localRoot,
		})
		if err != nil {
			return fmt.Errorf("creating transport: %w", err)
		}
		defer transport.Close()

		// Connect to peer
		fmt.Printf("🔄 Connecting to %s...\n", peerAddr)
		peer, err := transport.Connect(ctx, peerAddr)
		if err != nil {
			return fmt.Errorf("connecting to peer: %w", err)
		}

		fmt.Printf("✓ Connected to %s (%s)\n", peer.Name, peer.ID.Short())

		// Compare Merkle roots
		if localRoot != nil && peer.MerkleRoot != nil {
			localRootHex := fmt.Sprintf("%x", localRoot[:8])
			remoteRootHex := fmt.Sprintf("%x", peer.MerkleRoot[:8])
			fmt.Printf("   Local root:  %s...\n", localRootHex)
			fmt.Printf("   Remote root: %s...\n", remoteRootHex)

			if string(localRoot) == string(peer.MerkleRoot) {
				fmt.Println("\n✓ Already in sync! No changes needed.")
				return nil
			}
		}

		// Request manifest from peer
		fmt.Println("\n📥 Requesting file manifest...")
		manifest, err := requestManifest(ctx, transport, peer)
		if err != nil {
			return fmt.Errorf("requesting manifest: %w", err)
		}

		if manifest.Root == nil {
			fmt.Println("Remote has no files.")
			return nil
		}

		// Collect all chunk hashes we need
		fmt.Println("🔍 Computing diff...")
		neededChunks := collectMissingChunks(manifest.Root, store)

		if len(neededChunks) == 0 {
			fmt.Println("\n✓ All chunks already present locally!")
			// Still need to reconstruct files
		} else {
			fmt.Printf("   Need %d chunks\n", len(neededChunks))

			// Request chunks in batches
			fmt.Println("\n📦 Downloading chunks...")
			batchSize := 10
			downloaded := 0
			for i := 0; i < len(neededChunks); i += batchSize {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}

				end := i + batchSize
				if end > len(neededChunks) {
					end = len(neededChunks)
				}

				batch := neededChunks[i:end]
				resp, err := transport.RequestChunks(ctx, peer, batch)
				if err != nil {
					logging.Error("chunk request failed", "error", err)
					continue
				}

				// Store received chunks
				for _, chunk := range resp.Chunks {
					// Create a cas.Chunk from the proto.Chunk
					casChunk := &cas.Chunk{
						Data: chunk.Data,
					}
					// Compute hash
					hash := sha256.Sum256(chunk.Data)
					casChunk.Hash = hash[:]

					if _, err := store.Put(casChunk); err != nil {
						logging.Error("storing chunk failed", "error", err)
					} else {
						downloaded++
					}
				}

				fmt.Printf("   Downloaded %d/%d chunks\r", downloaded, len(neededChunks))
			}

			fmt.Printf("   Downloaded %d/%d chunks\n", downloaded, len(neededChunks))
		}

		// Reconstruct files from manifest
		fmt.Println("\n📝 Reconstructing files...")
		filesWritten, err := reconstructFiles(absDir, manifest.Root, store)
		if err != nil {
			return fmt.Errorf("reconstructing files: %w", err)
		}

		fmt.Printf("\n✓ Sync complete! %d files synchronized\n", filesWritten)

		return nil
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("p2p-file-sync %s\n", version)
		fmt.Printf("  commit: %s\n", commit)
		fmt.Printf("  built:  %s\n", date)
	},
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose logging")
	rootCmd.PersistentFlags().BoolVar(&jsonLogs, "json", false, "Output logs in JSON format")
	rootCmd.PersistentFlags().StringVar(&metricsAddr, "metrics", "", "Prometheus metrics address (e.g., :9090)")
	rootCmd.PersistentFlags().StringVarP(&dataDir, "dir", "d", "", "Directory to sync (default: current)")

	// Serve command flags
	serveCmd.Flags().IntP("port", "p", 4433, "Port to listen on")

	// Add commands
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(versionCmd)
}

// Helper functions

func indexDirectory(dir string, store *cas.BlobStore) (int, int, error) {
	fileCount := 0
	chunkCount := 0

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		// Skip hidden files/dirs and .storage
		base := filepath.Base(path)
		if len(base) > 0 && base[0] == '.' {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if info.IsDir() {
			return nil
		}

		// Chunk the file
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		reader := bufio.NewReaderSize(f, 64*1024)
		chunker := cas.NewChunker(reader)

		for {
			chunk, err := chunker.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil
			}

			_, err = store.Put(chunk)
			if err != nil {
				return nil
			}
			chunkCount++
		}

		fileCount++
		return nil
	})

	return fileCount, chunkCount, err
}

func convertTreeToManifest(node *merkle.Node) *proto.ManifestNode {
	if node == nil {
		return nil
	}

	mn := &proto.ManifestNode{
		Name:  node.Name,
		IsDir: node.Type == merkle.NodeTypeDir,
		Hash:  node.Hash,
		Mode:  node.Mode,
		Size:  node.Size,
	}

	if node.Type == merkle.NodeTypeFile {
		mn.Chunks = node.Chunks
	}

	for _, child := range node.Children {
		mn.Children = append(mn.Children, convertTreeToManifest(child))
	}

	return mn
}

func requestManifest(ctx context.Context, t *net.Transport, peer *net.Peer) (*proto.ManifestResponse, error) {
	stream, err := peer.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	req := &proto.ManifestRequest{
		MaxDepth:      -1,
		IncludeChunks: true,
	}

	if err := proto.SendManifestRequest(stream, 1, req); err != nil {
		return nil, err
	}

	env, err := proto.ReadEnvelope(stream)
	if err != nil {
		return nil, err
	}

	resp := &proto.ManifestResponse{}
	if err := proto.UnmarshalPayload(env, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

func collectMissingChunks(node *proto.ManifestNode, store *cas.BlobStore) [][]byte {
	var missing [][]byte

	if node == nil {
		return missing
	}

	// Check chunks for files
	for _, hash := range node.Chunks {
		if !store.Has(hash) {
			missing = append(missing, hash)
		}
	}

	// Recurse into children
	for _, child := range node.Children {
		missing = append(missing, collectMissingChunks(child, store)...)
	}

	return missing
}

func reconstructFiles(baseDir string, node *proto.ManifestNode, store *cas.BlobStore) (int, error) {
	// The root node represents the sync directory itself - skip its name
	// and process its children directly into baseDir
	if node == nil {
		return 0, nil
	}

	filesWritten := 0
	for _, child := range node.Children {
		n, err := reconstructFilesRecursive(baseDir, "", child, store)
		if err != nil {
			return filesWritten, err
		}
		filesWritten += n
	}
	return filesWritten, nil
}

func reconstructFilesRecursive(baseDir, relPath string, node *proto.ManifestNode, store *cas.BlobStore) (int, error) {
	if node == nil {
		return 0, nil
	}

	currentPath := filepath.Join(baseDir, relPath, node.Name)
	filesWritten := 0

	if node.IsDir {
		// Create directory
		if err := os.MkdirAll(currentPath, os.FileMode(node.Mode)|0755); err != nil {
			return 0, err
		}

		// Process children
		childPath := filepath.Join(relPath, node.Name)

		for _, child := range node.Children {
			n, err := reconstructFilesRecursive(baseDir, childPath, child, store)
			if err != nil {
				return filesWritten, err
			}
			filesWritten += n
		}
	} else {
		// Reconstruct file from chunks
		if len(node.Chunks) == 0 {
			return 0, nil
		}

		// Ensure parent directory exists
		parentDir := filepath.Dir(currentPath)
		if err := os.MkdirAll(parentDir, 0755); err != nil {
			return 0, err
		}

		// Create file
		f, err := os.Create(currentPath)
		if err != nil {
			return 0, err
		}

		for _, hash := range node.Chunks {
			chunk, err := store.Get(hash)
			if err != nil {
				f.Close()
				return filesWritten, fmt.Errorf("missing chunk %x: %w", hash[:8], err)
			}
			if _, err := f.Write(chunk.Data); err != nil {
				f.Close()
				return filesWritten, err
			}
		}

		f.Close()

		// Set file mode
		if node.Mode != 0 {
			os.Chmod(currentPath, os.FileMode(node.Mode))
		}

		filesWritten++
		logging.Debug("reconstructed file", "path", currentPath)
	}

	return filesWritten, nil
}

func calculateTreeDepth(node *merkle.Node) int {
	if node == nil || len(node.Children) == 0 {
		return 1
	}
	maxChildDepth := 0
	for _, child := range node.Children {
		childDepth := calculateTreeDepth(child)
		if childDepth > maxChildDepth {
			maxChildDepth = childDepth
		}
	}
	return maxChildDepth + 1
}

func countNodes(node *merkle.Node) int {
	if node == nil {
		return 0
	}
	count := 1
	for _, child := range node.Children {
		count += countNodes(child)
	}
	return count
}

func countFiles(node *merkle.Node) int {
	if node == nil {
		return 0
	}
	if node.Type == merkle.NodeTypeFile {
		return 1
	}
	count := 0
	for _, child := range node.Children {
		count += countFiles(child)
	}
	return count
}

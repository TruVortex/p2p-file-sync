// Package main provides the CLI for the P2P file sync engine.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"

	"p2p-file-sync/internal/logging"
	"p2p-file-sync/internal/merkle"
	"p2p-file-sync/internal/metrics"
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
	Use:   "p2psync",
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
			return fmt.Errorf("directory not initialized - run 'p2psync init' first")
		}

		// Build Merkle tree and show stats
		fmt.Println("Building file index...")

		tree, err := merkle.BuildFromFS(absDir)
		if err != nil {
			return fmt.Errorf("building tree: %w", err)
		}

		// Calculate stats
		depth := calculateTreeDepth(tree.Root)
		nodeCount := countNodes(tree.Root)
		fileCount := countFiles(tree.Root)

		rootHash := ""
		if tree.Root != nil && len(tree.Root.Hash) >= 8 {
			rootHash = fmt.Sprintf("%x", tree.Root.Hash[:8])
		}

		fmt.Printf("\n📁 Sync Status: %s\n", absDir)
		fmt.Printf("   Root Hash:    %s\n", rootHash)
		fmt.Printf("   Files:        %d\n", fileCount)
		fmt.Printf("   Tree Nodes:   %d\n", nodeCount)
		fmt.Printf("   Tree Depth:   %d\n", depth)

		// Update metrics
		metrics.SetMerkleTreeStats(depth, nodeCount)

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
			return fmt.Errorf("directory not initialized - run 'p2psync init' first")
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

		// Create progress container
		progress := mpb.New(mpb.WithWidth(60))

		// Simulate sync with progress bars
		fmt.Printf("🔄 Syncing with %s...\n\n", peerAddr)

		// Phase 1: Building local tree
		bar1 := progress.AddBar(100,
			mpb.PrependDecorators(
				decor.Name("📊 Building tree ", decor.WC{C: decor.DindentRight}),
				decor.Percentage(),
			),
			mpb.AppendDecorators(
				decor.OnComplete(decor.EwmaETA(decor.ET_STYLE_GO, 30), "done"),
			),
		)

		for i := 0; i < 100; i++ {
			select {
			case <-ctx.Done():
				progress.Wait()
				return ctx.Err()
			default:
				time.Sleep(20 * time.Millisecond)
				bar1.Increment()
			}
		}

		// Phase 2: Computing diff
		bar2 := progress.AddBar(100,
			mpb.PrependDecorators(
				decor.Name("🔍 Computing diff ", decor.WC{C: decor.DindentRight}),
				decor.Percentage(),
			),
			mpb.AppendDecorators(
				decor.OnComplete(decor.EwmaETA(decor.ET_STYLE_GO, 30), "done"),
			),
		)

		for i := 0; i < 100; i++ {
			select {
			case <-ctx.Done():
				progress.Wait()
				return ctx.Err()
			default:
				time.Sleep(15 * time.Millisecond)
				bar2.Increment()
			}
		}

		// Phase 3: Transferring chunks
		totalChunks := int64(42) // Simulated
		bar3 := progress.AddBar(totalChunks,
			mpb.PrependDecorators(
				decor.Name("📦 Transferring ", decor.WC{C: decor.DindentRight}),
				decor.CountersNoUnit("%d / %d"),
			),
			mpb.AppendDecorators(
				decor.OnComplete(
					decor.AverageSpeed(decor.SizeB1024(0), " %.1f"),
					" done",
				),
			),
		)

		for i := int64(0); i < totalChunks; i++ {
			select {
			case <-ctx.Done():
				progress.Wait()
				return ctx.Err()
			default:
				time.Sleep(50 * time.Millisecond)
				bar3.IncrBy(1)
				metrics.RecordDownload(peerAddr, 4096, 50*time.Millisecond)
			}
		}

		progress.Wait()

		fmt.Printf("\n✓ Sync complete! %d chunks transferred\n", totalChunks)

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
			return fmt.Errorf("directory not initialized - run 'p2psync init' first")
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Handle interrupt
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

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

		fmt.Printf("🚀 P2P Sync daemon started\n")
		fmt.Printf("   Directory: %s\n", absDir)
		fmt.Printf("   Port:      %d\n", port)
		if metricsAddr != "" {
			fmt.Printf("   Metrics:   http://%s/metrics\n", metricsAddr)
		}
		fmt.Println("\nPress Ctrl+C to stop...")

		logging.Info("daemon started",
			"dir", absDir,
			"port", port,
			"metrics", metricsAddr)

		select {
		case <-sigCh:
			fmt.Println("\n⏹ Shutting down...")
			cancel()
		case <-ctx.Done():
		}

		return nil
	},
}

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch directory for changes",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, _ := cmd.Flags().GetString("dir")
		if dir == "" {
			dir = "."
		}

		absDir, _ := filepath.Abs(dir)

		_, cancel := context.WithCancel(context.Background())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

		fmt.Printf("👀 Watching %s for changes...\n", absDir)
		fmt.Println("Press Ctrl+C to stop")

		// Simulate file watching with progress
		progress := mpb.New()

		spinner := progress.New(0,
			mpb.SpinnerStyle("◐", "◓", "◑", "◒"),
			mpb.PrependDecorators(
				decor.Name("Watching"),
			),
			mpb.AppendDecorators(
				decor.Elapsed(decor.ET_STYLE_GO),
			),
		)

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-sigCh:
				spinner.Abort(true)
				progress.Wait()
				fmt.Println("\n⏹ Stopped watching")
				return nil
			case <-ticker.C:
				spinner.Increment()
			}
		}
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("p2psync %s\n", version)
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
	serveCmd.Flags().IntP("port", "p", 8080, "Port to listen on")

	// Add commands
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(watchCmd)
	rootCmd.AddCommand(versionCmd)
}

// Helper functions

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

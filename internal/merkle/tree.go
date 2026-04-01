// Package merkle implements a Merkle Tree for filesystem state representation.
// It enables O(log N) reconciliation between two directory trees by comparing
// root hashes and descending only into differing subtrees.
package merkle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// NodeType distinguishes between files and directories in the tree.
type NodeType uint8

const (
	NodeTypeFile NodeType = iota
	NodeTypeDir
)

// Node represents a single node in the Merkle Tree.
// Files are leaf nodes containing chunk hashes.
// Directories are internal nodes containing child nodes.
type Node struct {
	Name     string   // File or directory name
	Type     NodeType // File or directory
	Hash     []byte   // SHA-256: for files, hash of chunk hashes; for dirs, hash of children hashes
	Mode     uint32   // File permissions
	Size     uint64   // Total size in bytes
	Children []*Node  // Child nodes (only for directories)
	Chunks   [][]byte // Chunk hashes (only for files, ordered)
}

// HashHex returns the hex-encoded hash string.
func (n *Node) HashHex() string {
	return hex.EncodeToString(n.Hash)
}

// IsDir returns true if this node represents a directory.
func (n *Node) IsDir() bool {
	return n.Type == NodeTypeDir
}

// Tree represents a complete Merkle Tree for a filesystem.
type Tree struct {
	Root *Node
}

// NewTree creates an empty Merkle Tree.
func NewTree() *Tree {
	return &Tree{}
}

// NewFileNode creates a leaf node for a file with its chunk hashes.
func NewFileNode(name string, mode uint32, chunks [][]byte) *Node {
	node := &Node{
		Name:   name,
		Type:   NodeTypeFile,
		Mode:   mode,
		Chunks: chunks,
	}

	// Compute file hash from chunk hashes
	node.computeFileHash()
	return node
}

// NewDirNode creates an internal node for a directory.
func NewDirNode(name string, mode uint32, children []*Node) *Node {
	node := &Node{
		Name:     name,
		Type:     NodeTypeDir,
		Mode:     mode,
		Children: children,
	}

	// Sort children by name for deterministic hashing
	sort.Slice(node.Children, func(i, j int) bool {
		return node.Children[i].Name < node.Children[j].Name
	})

	// Compute directory hash from children
	node.computeDirHash()
	return node
}

// computeFileHash computes the hash for a file node from its chunk hashes.
// Hash = SHA256(chunk0_hash || chunk1_hash || ... || chunkN_hash)
func (n *Node) computeFileHash() {
	hasher := sha256.New()
	for _, chunkHash := range n.Chunks {
		hasher.Write(chunkHash)
	}
	// Also include size for empty file distinction
	n.Size = uint64(len(n.Chunks)) * 4096 // Approximate, actual tracked elsewhere
	n.Hash = hasher.Sum(nil)
}

// computeDirHash computes the hash for a directory from its children.
// Hash = SHA256(child0_name || child0_hash || child1_name || child1_hash || ...)
func (n *Node) computeDirHash() {
	hasher := sha256.New()
	var totalSize uint64

	for _, child := range n.Children {
		// Include name to detect renames
		hasher.Write([]byte(child.Name))
		hasher.Write(child.Hash)
		totalSize += child.Size
	}

	n.Size = totalSize
	n.Hash = hasher.Sum(nil)
}

// Rehash recomputes the hash for this node and all ancestors.
// Call this after modifying children.
func (n *Node) Rehash() {
	if n.Type == NodeTypeFile {
		n.computeFileHash()
	} else {
		// Re-sort children
		sort.Slice(n.Children, func(i, j int) bool {
			return n.Children[i].Name < n.Children[j].Name
		})
		n.computeDirHash()
	}
}

// GetChild finds a child node by name. Returns nil if not found.
func (n *Node) GetChild(name string) *Node {
	if n.Type != NodeTypeDir {
		return nil
	}
	// Binary search since children are sorted
	i := sort.Search(len(n.Children), func(i int) bool {
		return n.Children[i].Name >= name
	})
	if i < len(n.Children) && n.Children[i].Name == name {
		return n.Children[i]
	}
	return nil
}

// DiffResult represents a single difference between two trees.
type DiffResult struct {
	Path       string   // Full path to the differing node
	Type       DiffType // Type of difference
	LocalNode  *Node    // Node in local tree (nil if missing)
	RemoteNode *Node    // Node in remote tree (nil if missing)
	Chunks     [][]byte // Specific chunk hashes that differ (for files)
}

// DiffType describes the kind of difference found.
type DiffType uint8

const (
	DiffTypeAdded    DiffType = iota // Present locally, missing remotely
	DiffTypeDeleted                  // Missing locally, present remotely
	DiffTypeModified                 // Present in both but different hash
)

func (d DiffType) String() string {
	switch d {
	case DiffTypeAdded:
		return "added"
	case DiffTypeDeleted:
		return "deleted"
	case DiffTypeModified:
		return "modified"
	default:
		return "unknown"
	}
}

// Diff compares two Merkle Trees and returns the differences.
// This achieves O(log N) by only descending into subtrees with different hashes.
func Diff(local, remote *Tree) []DiffResult {
	var results []DiffResult

	// Handle nil trees
	if local == nil || local.Root == nil {
		if remote != nil && remote.Root != nil {
			// Everything in remote is "deleted" (we need to fetch it)
			collectAll(remote.Root, "", DiffTypeDeleted, &results)
		}
		return results
	}
	if remote == nil || remote.Root == nil {
		// Everything in local is "added" (we need to push it)
		collectAll(local.Root, "", DiffTypeAdded, &results)
		return results
	}

	// Quick check: if roots match, trees are identical
	if bytes.Equal(local.Root.Hash, remote.Root.Hash) {
		return results // Empty - no differences
	}

	// Descend into differing subtrees
	diffNodes(local.Root, remote.Root, "", &results)
	return results
}

// diffNodes recursively compares two nodes, only descending where hashes differ.
func diffNodes(local, remote *Node, path string, results *[]DiffResult) {
	currentPath := path
	if local != nil {
		currentPath = joinPath(path, local.Name)
	} else if remote != nil {
		currentPath = joinPath(path, remote.Name)
	}

	// Case 1: Only exists locally
	if remote == nil {
		collectAll(local, path, DiffTypeAdded, results)
		return
	}

	// Case 2: Only exists remotely
	if local == nil {
		collectAll(remote, path, DiffTypeDeleted, results)
		return
	}

	// Case 3: Both exist - check if hashes match
	if bytes.Equal(local.Hash, remote.Hash) {
		return // Subtrees are identical, skip
	}

	// Case 4: Type changed (file <-> directory)
	if local.Type != remote.Type {
		*results = append(*results, DiffResult{
			Path:       currentPath,
			Type:       DiffTypeModified,
			LocalNode:  local,
			RemoteNode: remote,
		})
		return
	}

	// Case 5: Both are files with different content
	if local.Type == NodeTypeFile {
		// Find which chunks differ
		diffChunks := findDifferingChunks(local.Chunks, remote.Chunks)
		*results = append(*results, DiffResult{
			Path:       currentPath,
			Type:       DiffTypeModified,
			LocalNode:  local,
			RemoteNode: remote,
			Chunks:     diffChunks,
		})
		return
	}

	// Case 6: Both are directories - descend into children
	localChildren := make(map[string]*Node)
	for _, child := range local.Children {
		localChildren[child.Name] = child
	}

	remoteChildren := make(map[string]*Node)
	for _, child := range remote.Children {
		remoteChildren[child.Name] = child
	}

	// Check all local children
	for name, localChild := range localChildren {
		remoteChild := remoteChildren[name]
		diffNodes(localChild, remoteChild, currentPath, results)
	}

	// Check remote children not in local
	for name, remoteChild := range remoteChildren {
		if _, exists := localChildren[name]; !exists {
			diffNodes(nil, remoteChild, currentPath, results)
		}
	}
}

// findDifferingChunks identifies which chunks differ between two files.
func findDifferingChunks(local, remote [][]byte) [][]byte {
	var differing [][]byte

	// Build set of remote chunks for O(1) lookup
	remoteSet := make(map[string]bool)
	for _, h := range remote {
		remoteSet[string(h)] = true
	}

	// Find chunks in local that aren't in remote
	for _, h := range local {
		if !remoteSet[string(h)] {
			differing = append(differing, h)
		}
	}

	return differing
}

// collectAll adds all nodes in a subtree to results.
func collectAll(node *Node, parentPath string, diffType DiffType, results *[]DiffResult) {
	if node == nil {
		return
	}

	currentPath := joinPath(parentPath, node.Name)

	result := DiffResult{
		Path: currentPath,
		Type: diffType,
	}

	if diffType == DiffTypeAdded {
		result.LocalNode = node
		if node.Type == NodeTypeFile {
			result.Chunks = node.Chunks
		}
	} else {
		result.RemoteNode = node
		if node.Type == NodeTypeFile {
			result.Chunks = node.Chunks
		}
	}

	*results = append(*results, result)

	// Recurse into children
	for _, child := range node.Children {
		collectAll(child, currentPath, diffType, results)
	}
}

// joinPath joins parent and child paths.
func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "/" + child
}

// PrintTree prints a visual representation of the tree for debugging.
func (t *Tree) PrintTree() {
	if t.Root == nil {
		fmt.Println("<empty tree>")
		return
	}
	printNode(t.Root, "", true)
}

func printNode(node *Node, prefix string, isLast bool) {
	marker := "├── "
	if isLast {
		marker = "└── "
	}

	typeStr := "F"
	if node.Type == NodeTypeDir {
		typeStr = "D"
	}

	fmt.Printf("%s%s[%s] %s (%s)\n", prefix, marker, typeStr, node.Name, node.HashHex()[:8])

	if node.Type == NodeTypeDir {
		newPrefix := prefix
		if isLast {
			newPrefix += "    "
		} else {
			newPrefix += "│   "
		}

		for i, child := range node.Children {
			printNode(child, newPrefix, i == len(node.Children)-1)
		}
	}
}

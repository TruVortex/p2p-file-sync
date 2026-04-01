package merkle

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"p2p-file-sync/internal/cas"
)

// BuildFromFS builds a Merkle Tree from a filesystem directory.
// It chunks all files using the CAS chunker and computes hashes bottom-up.
func BuildFromFS(rootPath string) (*Tree, error) {
	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("stat root path: %w", err)
	}

	if !info.IsDir() {
		return nil, fmt.Errorf("root path must be a directory: %s", rootPath)
	}

	root, err := buildNode(rootPath, info)
	if err != nil {
		return nil, err
	}

	return &Tree{Root: root}, nil
}

// buildNode recursively builds a node for a file or directory.
func buildNode(path string, info fs.FileInfo) (*Node, error) {
	if info.IsDir() {
		return buildDirNode(path, info)
	}
	return buildFileNode(path, info)
}

// buildFileNode creates a leaf node by chunking the file.
func buildFileNode(path string, info fs.FileInfo) (*Node, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening file %s: %w", path, err)
	}
	defer f.Close()

	// Use buffered reader for performance
	reader := bufio.NewReaderSize(f, 64*1024)
	chunker := cas.NewChunker(reader)

	chunks, err := chunker.ChunkAll()
	if err != nil {
		return nil, fmt.Errorf("chunking file %s: %w", path, err)
	}

	// Extract just the hashes
	chunkHashes := make([][]byte, len(chunks))
	for i, chunk := range chunks {
		chunkHashes[i] = chunk.Hash
	}

	node := NewFileNode(info.Name(), uint32(info.Mode()), chunkHashes)
	node.Size = uint64(info.Size())

	return node, nil
}

// buildDirNode creates an internal node for a directory.
func buildDirNode(path string, info fs.FileInfo) (*Node, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("reading directory %s: %w", path, err)
	}

	var children []*Node

	for _, entry := range entries {
		// Skip hidden files and special directories
		if entry.Name()[0] == '.' {
			continue
		}

		childPath := filepath.Join(path, entry.Name())
		childInfo, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", childPath, err)
		}

		child, err := buildNode(childPath, childInfo)
		if err != nil {
			return nil, err
		}

		children = append(children, child)
	}

	return NewDirNode(info.Name(), uint32(info.Mode()), children), nil
}

// BuildFromManifest builds a tree from a serialized manifest.
// This is used when receiving a remote tree over the network.
func BuildFromManifest(manifest *Manifest) (*Tree, error) {
	if manifest == nil || manifest.Root == nil {
		return &Tree{}, nil
	}

	root := manifestNodeToNode(manifest.Root)
	return &Tree{Root: root}, nil
}

// manifestNodeToNode recursively converts manifest nodes to tree nodes.
func manifestNodeToNode(mn *ManifestNode) *Node {
	node := &Node{
		Name: mn.Name,
		Type: mn.Type,
		Hash: mn.Hash,
		Mode: mn.Mode,
		Size: mn.Size,
	}

	if mn.Type == NodeTypeFile {
		node.Chunks = mn.Chunks
	} else {
		for _, child := range mn.Children {
			node.Children = append(node.Children, manifestNodeToNode(child))
		}
	}

	return node
}

// Manifest is a serializable representation of a Merkle Tree.
type Manifest struct {
	Root *ManifestNode
}

// ManifestNode is a serializable tree node.
type ManifestNode struct {
	Name     string          `json:"name"`
	Type     NodeType        `json:"type"`
	Hash     []byte          `json:"hash"`
	Mode     uint32          `json:"mode"`
	Size     uint64          `json:"size"`
	Children []*ManifestNode `json:"children,omitempty"`
	Chunks   [][]byte        `json:"chunks,omitempty"`
}

// ToManifest converts a tree to a serializable manifest.
func (t *Tree) ToManifest() *Manifest {
	if t.Root == nil {
		return &Manifest{}
	}
	return &Manifest{Root: nodeToManifestNode(t.Root)}
}

func nodeToManifestNode(n *Node) *ManifestNode {
	mn := &ManifestNode{
		Name: n.Name,
		Type: n.Type,
		Hash: n.Hash,
		Mode: n.Mode,
		Size: n.Size,
	}

	if n.Type == NodeTypeFile {
		mn.Chunks = n.Chunks
	} else {
		for _, child := range n.Children {
			mn.Children = append(mn.Children, nodeToManifestNode(child))
		}
	}

	return mn
}

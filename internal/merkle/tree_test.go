package merkle

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestNewFileNode_HashFromChunks(t *testing.T) {
	chunk1 := sha256.Sum256([]byte("chunk1"))
	chunk2 := sha256.Sum256([]byte("chunk2"))
	chunks := [][]byte{chunk1[:], chunk2[:]}

	node := NewFileNode("test.txt", 0644, chunks)

	if node.Name != "test.txt" {
		t.Errorf("expected name 'test.txt', got '%s'", node.Name)
	}
	if node.Type != NodeTypeFile {
		t.Error("expected file type")
	}
	if len(node.Hash) != 32 {
		t.Errorf("expected 32-byte hash, got %d bytes", len(node.Hash))
	}

	// Verify hash is deterministic
	node2 := NewFileNode("test.txt", 0644, chunks)
	if !bytes.Equal(node.Hash, node2.Hash) {
		t.Error("file hash should be deterministic")
	}
}

func TestNewDirNode_HashFromChildren(t *testing.T) {
	chunk1 := sha256.Sum256([]byte("data1"))
	chunk2 := sha256.Sum256([]byte("data2"))

	file1 := NewFileNode("a.txt", 0644, [][]byte{chunk1[:]})
	file2 := NewFileNode("b.txt", 0644, [][]byte{chunk2[:]})

	dir := NewDirNode("mydir", 0755, []*Node{file1, file2})

	if dir.Name != "mydir" {
		t.Errorf("expected name 'mydir', got '%s'", dir.Name)
	}
	if dir.Type != NodeTypeDir {
		t.Error("expected dir type")
	}
	if len(dir.Children) != 2 {
		t.Errorf("expected 2 children, got %d", len(dir.Children))
	}

	// Verify children are sorted
	if dir.Children[0].Name != "a.txt" || dir.Children[1].Name != "b.txt" {
		t.Error("children should be sorted by name")
	}
}

func TestDirNode_SortedChildren(t *testing.T) {
	chunk := sha256.Sum256([]byte("data"))

	// Create children in reverse order
	fileZ := NewFileNode("z.txt", 0644, [][]byte{chunk[:]})
	fileA := NewFileNode("a.txt", 0644, [][]byte{chunk[:]})
	fileM := NewFileNode("m.txt", 0644, [][]byte{chunk[:]})

	dir := NewDirNode("root", 0755, []*Node{fileZ, fileA, fileM})

	// Verify sorted
	if dir.Children[0].Name != "a.txt" {
		t.Errorf("first child should be 'a.txt', got '%s'", dir.Children[0].Name)
	}
	if dir.Children[1].Name != "m.txt" {
		t.Errorf("second child should be 'm.txt', got '%s'", dir.Children[1].Name)
	}
	if dir.Children[2].Name != "z.txt" {
		t.Errorf("third child should be 'z.txt', got '%s'", dir.Children[2].Name)
	}
}

func TestDiff_IdenticalTrees(t *testing.T) {
	chunk := sha256.Sum256([]byte("same data"))
	file := NewFileNode("file.txt", 0644, [][]byte{chunk[:]})

	tree1 := &Tree{Root: NewDirNode("root", 0755, []*Node{file})}

	// Create identical tree
	file2 := NewFileNode("file.txt", 0644, [][]byte{chunk[:]})
	tree2 := &Tree{Root: NewDirNode("root", 0755, []*Node{file2})}

	diffs := Diff(tree1, tree2)

	if len(diffs) != 0 {
		t.Errorf("expected no diffs for identical trees, got %d", len(diffs))
	}
}

func TestDiff_AddedFile(t *testing.T) {
	chunk1 := sha256.Sum256([]byte("data1"))
	chunk2 := sha256.Sum256([]byte("data2"))

	// Local has two files
	file1 := NewFileNode("a.txt", 0644, [][]byte{chunk1[:]})
	file2 := NewFileNode("b.txt", 0644, [][]byte{chunk2[:]})
	local := &Tree{Root: NewDirNode("root", 0755, []*Node{file1, file2})}

	// Remote has only one file
	remoteFile := NewFileNode("a.txt", 0644, [][]byte{chunk1[:]})
	remote := &Tree{Root: NewDirNode("root", 0755, []*Node{remoteFile})}

	diffs := Diff(local, remote)

	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}

	if diffs[0].Type != DiffTypeAdded {
		t.Errorf("expected DiffTypeAdded, got %v", diffs[0].Type)
	}
	if diffs[0].Path != "root/b.txt" {
		t.Errorf("expected path 'root/b.txt', got '%s'", diffs[0].Path)
	}
}

func TestDiff_DeletedFile(t *testing.T) {
	chunk1 := sha256.Sum256([]byte("data1"))
	chunk2 := sha256.Sum256([]byte("data2"))

	// Local has one file
	file1 := NewFileNode("a.txt", 0644, [][]byte{chunk1[:]})
	local := &Tree{Root: NewDirNode("root", 0755, []*Node{file1})}

	// Remote has two files
	remoteFile1 := NewFileNode("a.txt", 0644, [][]byte{chunk1[:]})
	remoteFile2 := NewFileNode("b.txt", 0644, [][]byte{chunk2[:]})
	remote := &Tree{Root: NewDirNode("root", 0755, []*Node{remoteFile1, remoteFile2})}

	diffs := Diff(local, remote)

	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}

	if diffs[0].Type != DiffTypeDeleted {
		t.Errorf("expected DiffTypeDeleted, got %v", diffs[0].Type)
	}
	if diffs[0].Path != "root/b.txt" {
		t.Errorf("expected path 'root/b.txt', got '%s'", diffs[0].Path)
	}
}

func TestDiff_ModifiedFile(t *testing.T) {
	chunk1 := sha256.Sum256([]byte("original"))
	chunk2 := sha256.Sum256([]byte("modified"))

	// Local has modified version
	localFile := NewFileNode("file.txt", 0644, [][]byte{chunk2[:]})
	local := &Tree{Root: NewDirNode("root", 0755, []*Node{localFile})}

	// Remote has original version
	remoteFile := NewFileNode("file.txt", 0644, [][]byte{chunk1[:]})
	remote := &Tree{Root: NewDirNode("root", 0755, []*Node{remoteFile})}

	diffs := Diff(local, remote)

	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}

	if diffs[0].Type != DiffTypeModified {
		t.Errorf("expected DiffTypeModified, got %v", diffs[0].Type)
	}
	if len(diffs[0].Chunks) != 1 {
		t.Errorf("expected 1 differing chunk, got %d", len(diffs[0].Chunks))
	}
}

func TestDiff_NestedDirectories(t *testing.T) {
	chunk := sha256.Sum256([]byte("data"))
	modifiedChunk := sha256.Sum256([]byte("modified"))

	// Build local tree: root/subdir/deep/file.txt (modified)
	localDeepFile := NewFileNode("file.txt", 0644, [][]byte{modifiedChunk[:]})
	localDeep := NewDirNode("deep", 0755, []*Node{localDeepFile})
	localSubdir := NewDirNode("subdir", 0755, []*Node{localDeep})
	local := &Tree{Root: NewDirNode("root", 0755, []*Node{localSubdir})}

	// Build remote tree: root/subdir/deep/file.txt (original)
	remoteDeepFile := NewFileNode("file.txt", 0644, [][]byte{chunk[:]})
	remoteDeep := NewDirNode("deep", 0755, []*Node{remoteDeepFile})
	remoteSubdir := NewDirNode("subdir", 0755, []*Node{remoteDeep})
	remote := &Tree{Root: NewDirNode("root", 0755, []*Node{remoteSubdir})}

	diffs := Diff(local, remote)

	// Should only find the one modified file, not intermediate directories
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}

	if diffs[0].Path != "root/subdir/deep/file.txt" {
		t.Errorf("expected path 'root/subdir/deep/file.txt', got '%s'", diffs[0].Path)
	}
}

func TestDiff_EmptyTrees(t *testing.T) {
	local := &Tree{}
	remote := &Tree{}

	diffs := Diff(local, remote)

	if len(diffs) != 0 {
		t.Errorf("expected no diffs for empty trees, got %d", len(diffs))
	}
}

func TestDiff_LocalEmptyRemotePopulated(t *testing.T) {
	chunk := sha256.Sum256([]byte("data"))
	remoteFile := NewFileNode("file.txt", 0644, [][]byte{chunk[:]})
	remote := &Tree{Root: NewDirNode("root", 0755, []*Node{remoteFile})}

	local := &Tree{}

	diffs := Diff(local, remote)

	// Should see deleted entries for root dir and file
	if len(diffs) != 2 {
		t.Errorf("expected 2 diffs (dir + file), got %d", len(diffs))
	}

	for _, d := range diffs {
		if d.Type != DiffTypeDeleted {
			t.Errorf("expected all diffs to be DiffTypeDeleted, got %v", d.Type)
		}
	}
}

func TestNode_GetChild(t *testing.T) {
	chunk := sha256.Sum256([]byte("data"))
	file1 := NewFileNode("alpha.txt", 0644, [][]byte{chunk[:]})
	file2 := NewFileNode("beta.txt", 0644, [][]byte{chunk[:]})
	file3 := NewFileNode("gamma.txt", 0644, [][]byte{chunk[:]})

	dir := NewDirNode("root", 0755, []*Node{file3, file1, file2}) // unsorted input

	// Should find by name using binary search
	found := dir.GetChild("beta.txt")
	if found == nil {
		t.Fatal("expected to find beta.txt")
	}
	if found.Name != "beta.txt" {
		t.Errorf("expected name 'beta.txt', got '%s'", found.Name)
	}

	// Should return nil for non-existent
	notFound := dir.GetChild("delta.txt")
	if notFound != nil {
		t.Error("expected nil for non-existent child")
	}
}

// Benchmark to verify O(log N) diff performance
func BenchmarkDiff_LargeTree(b *testing.B) {
	// Create a tree with 10000 files
	chunk := sha256.Sum256([]byte("data"))
	children := make([]*Node, 10000)
	for i := range children {
		children[i] = NewFileNode(
			string(rune('a'+i%26))+string(rune('0'+i%10))+".txt",
			0644,
			[][]byte{chunk[:]},
		)
	}
	tree1 := &Tree{Root: NewDirNode("root", 0755, children)}

	// Clone and modify one file
	children2 := make([]*Node, 10000)
	modifiedChunk := sha256.Sum256([]byte("modified"))
	for i := range children2 {
		if i == 5000 {
			children2[i] = NewFileNode(children[i].Name, 0644, [][]byte{modifiedChunk[:]})
		} else {
			children2[i] = NewFileNode(children[i].Name, 0644, [][]byte{chunk[:]})
		}
	}
	tree2 := &Tree{Root: NewDirNode("root", 0755, children2)}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Diff(tree1, tree2)
	}
}

package net

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"

	"p2p-file-sync/internal/cas"
	"p2p-file-sync/internal/merkle"
	"p2p-file-sync/internal/proto"
)

// SyncManager coordinates synchronization with peers.
type SyncManager struct {
	transport *Transport
	store     *cas.BlobStore
	tree      *merkle.Tree

	// Pending chunk requests
	pendingMu sync.RWMutex
	pending   map[string]*chunkRequest // hash -> request

	// Stats
	chunksRequested atomic.Int64
	chunksReceived  atomic.Int64
	bytesReceived   atomic.Int64

	// Concurrent request handling
	requestCh chan *syncRequest
	resultCh  chan *syncResult
	workers   int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type chunkRequest struct {
	hash     []byte
	resultCh chan *proto.Chunk
	err      error
}

type syncRequest struct {
	peer   *Peer
	hashes [][]byte
}

type syncResult struct {
	chunks   []*proto.Chunk
	notFound [][]byte
	err      error
}

// SyncConfig holds sync manager configuration.
type SyncConfig struct {
	Transport *Transport
	Store     *cas.BlobStore
	Tree      *merkle.Tree
	Workers   int // Number of concurrent chunk fetchers
}

// NewSyncManager creates a new sync manager.
func NewSyncManager(cfg *SyncConfig) *SyncManager {
	workers := cfg.Workers
	if workers <= 0 {
		workers = 4
	}

	ctx, cancel := context.WithCancel(context.Background())

	sm := &SyncManager{
		transport: cfg.Transport,
		store:     cfg.Store,
		tree:      cfg.Tree,
		pending:   make(map[string]*chunkRequest),
		requestCh: make(chan *syncRequest, 100),
		resultCh:  make(chan *syncResult, 100),
		workers:   workers,
		ctx:       ctx,
		cancel:    cancel,
	}

	// Start worker pool
	for i := 0; i < workers; i++ {
		sm.wg.Add(1)
		go sm.worker()
	}

	return sm
}

// Close shuts down the sync manager.
func (sm *SyncManager) Close() {
	sm.cancel()
	close(sm.requestCh)
	sm.wg.Wait()
}

// SyncWithPeer initiates synchronization with a peer.
func (sm *SyncManager) SyncWithPeer(ctx context.Context, peer *Peer) (*SyncResult, error) {
	result := &SyncResult{
		PeerID: peer.ID,
	}

	// Check if we're already in sync
	if sm.tree != nil && sm.tree.Root != nil && peer.MerkleRoot != nil {
		if bytes.Equal(sm.tree.Root.Hash, peer.MerkleRoot) {
			result.AlreadyInSync = true
			return result, nil
		}
	}

	// Request manifest from peer
	manifest, err := sm.requestManifest(ctx, peer)
	if err != nil {
		return nil, err
	}

	// Build remote tree from manifest
	remoteTree := &merkle.Tree{Root: manifestToNode(manifest.Root)}

	// Compute diff
	diffs := merkle.Diff(sm.tree, remoteTree)
	result.Diffs = len(diffs)

	// Collect all chunks we need to fetch
	var chunksToFetch [][]byte
	for _, diff := range diffs {
		if diff.Type == merkle.DiffTypeDeleted || diff.Type == merkle.DiffTypeModified {
			// We need to fetch these chunks
			chunksToFetch = append(chunksToFetch, diff.Chunks...)
		}
	}

	result.ChunksNeeded = len(chunksToFetch)

	if len(chunksToFetch) == 0 {
		return result, nil
	}

	// Fetch chunks in batches
	const batchSize = 16
	for i := 0; i < len(chunksToFetch); i += batchSize {
		end := i + batchSize
		if end > len(chunksToFetch) {
			end = len(chunksToFetch)
		}
		batch := chunksToFetch[i:end]

		resp, err := sm.transport.RequestChunks(ctx, peer, batch)
		if err != nil {
			return result, err
		}

		// Store received chunks
		for _, chunk := range resp.Chunks {
			casChunk := &cas.Chunk{
				Hash: chunk.Hash,
				Size: chunk.Size,
				Data: chunk.Data,
			}
			if _, err := sm.store.Put(casChunk); err != nil {
				return result, err
			}
			result.ChunksFetched++
			result.BytesFetched += int64(len(chunk.Data))
		}

		result.ChunksNotFound += len(resp.NotFound)
	}

	return result, nil
}

// SyncResult contains the outcome of a sync operation.
type SyncResult struct {
	PeerID         PeerID
	AlreadyInSync  bool
	Diffs          int
	ChunksNeeded   int
	ChunksFetched  int
	ChunksNotFound int
	BytesFetched   int64
}

// requestManifest fetches the manifest from a peer.
func (sm *SyncManager) requestManifest(ctx context.Context, peer *Peer) (*proto.ManifestResponse, error) {
	stream, err := peer.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	// Send request
	req := &proto.ManifestRequest{
		MaxDepth:      -1,
		IncludeChunks: true,
	}

	if err := proto.SendManifestRequest(stream, 1, req); err != nil {
		return nil, err
	}

	// Read response
	env, err := proto.ReadEnvelope(stream)
	if err != nil {
		return nil, err
	}

	if env.Type != proto.MessageType_MESSAGE_TYPE_MANIFEST_RESPONSE {
		return nil, err
	}

	// Decode manifest
	resp := &proto.ManifestResponse{}
	if err := proto.UnmarshalPayload(env, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// worker processes chunk requests from the channel.
func (sm *SyncManager) worker() {
	defer sm.wg.Done()

	for req := range sm.requestCh {
		if req == nil {
			return
		}

		resp, err := sm.transport.RequestChunks(sm.ctx, req.peer, req.hashes)

		result := &syncResult{err: err}
		if resp != nil {
			result.chunks = resp.Chunks
			result.notFound = resp.NotFound
		}

		select {
		case sm.resultCh <- result:
		case <-sm.ctx.Done():
			return
		}
	}
}

// manifestToNode converts a proto manifest node to a merkle node.
func manifestToNode(mn *proto.ManifestNode) *merkle.Node {
	if mn == nil {
		return nil
	}

	nodeType := merkle.NodeTypeFile
	if mn.IsDir {
		nodeType = merkle.NodeTypeDir
	}

	node := &merkle.Node{
		Name:   mn.Name,
		Type:   nodeType,
		Hash:   mn.Hash,
		Mode:   mn.Mode,
		Size:   mn.Size,
		Chunks: mn.Chunks,
	}

	for _, child := range mn.Children {
		node.Children = append(node.Children, manifestToNode(child))
	}

	return node
}

// UpdateMerkleRoot updates the local Merkle root and notifies peers.
func (sm *SyncManager) UpdateMerkleRoot(tree *merkle.Tree) {
	sm.tree = tree
}

// HandleChunkRequest processes incoming chunk requests from peers.
func (sm *SyncManager) HandleChunkRequest(peer *Peer, req *proto.ChunkRequest) *proto.ChunkResponse {
	resp := &proto.ChunkResponse{
		Chunks:   make([]*proto.Chunk, 0, len(req.ChunkHashes)),
		NotFound: make([][]byte, 0),
	}

	for _, hash := range req.ChunkHashes {
		chunk, err := sm.store.Get(hash)
		if err != nil {
			resp.NotFound = append(resp.NotFound, hash)
			continue
		}

		resp.Chunks = append(resp.Chunks, &proto.Chunk{
			Hash: chunk.Hash,
			Data: chunk.Data,
			Size: chunk.Size,
		})
	}

	return resp
}

// HandleManifestRequest processes incoming manifest requests.
func (sm *SyncManager) HandleManifestRequest(peer *Peer, req *proto.ManifestRequest) *proto.ManifestResponse {
	if sm.tree == nil || sm.tree.Root == nil {
		return &proto.ManifestResponse{}
	}

	return &proto.ManifestResponse{
		Root: nodeToManifest(sm.tree.Root),
	}
}

// nodeToManifest converts a merkle node to a proto manifest node.
func nodeToManifest(n *merkle.Node) *proto.ManifestNode {
	if n == nil {
		return nil
	}

	mn := &proto.ManifestNode{
		Name:   n.Name,
		IsDir:  n.Type == merkle.NodeTypeDir,
		Hash:   n.Hash,
		Mode:   n.Mode,
		Size:   n.Size,
		Chunks: n.Chunks,
	}

	for _, child := range n.Children {
		mn.Children = append(mn.Children, nodeToManifest(child))
	}

	return mn
}

// Stats returns current sync statistics.
func (sm *SyncManager) Stats() (requested, received int64, bytes int64) {
	return sm.chunksRequested.Load(), sm.chunksReceived.Load(), sm.bytesReceived.Load()
}

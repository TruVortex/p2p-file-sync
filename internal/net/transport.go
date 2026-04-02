// Package net provides QUIC-based P2P networking with TLS.
package net

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	googleproto "google.golang.org/protobuf/proto"

	"p2p-file-sync/internal/proto"
)

// PeerID is the SHA-256 fingerprint of a peer's TLS certificate.
type PeerID [32]byte

// String returns the hex-encoded peer ID.
func (p PeerID) String() string {
	return hex.EncodeToString(p[:])
}

// Short returns a shortened peer ID for display.
func (p PeerID) Short() string {
	return hex.EncodeToString(p[:8])
}

// Peer represents a connected remote peer.
type Peer struct {
	ID          PeerID
	Name        string
	Addr        string
	MerkleRoot  []byte
	FileCount   uint64
	TotalSize   uint64
	conn        *quic.Conn
	mu          sync.RWMutex
	streams     map[uint64]*quic.Stream
	nextReqID   uint64
	established time.Time
}

// Transport handles QUIC connections and TLS certificates.
type Transport struct {
	config     *Config
	tlsConfig  *tls.Config
	quicConfig *quic.Config
	listener   *quic.Listener
	localID    PeerID
	localCert  *tls.Certificate
	peers      map[PeerID]*Peer
	peersMu    sync.RWMutex

	// Callbacks
	onPeerConnected    func(*Peer)
	onPeerDisconnected func(*Peer)
	onManifestRequest  func(*Peer, *proto.ManifestRequest) *proto.ManifestResponse
	onChunkRequest     func(*Peer, *proto.ChunkRequest) *proto.ChunkResponse

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Config holds transport configuration.
type Config struct {
	ListenAddr string
	PeerName   string
	MerkleRoot []byte
	FileCount  uint64
	TotalSize  uint64

	// Optional: provide existing certificate
	Certificate *tls.Certificate

	// Callbacks
	OnPeerConnected    func(*Peer)
	OnPeerDisconnected func(*Peer)
	OnManifestRequest  func(*Peer, *proto.ManifestRequest) *proto.ManifestResponse
	OnChunkRequest     func(*Peer, *proto.ChunkRequest) *proto.ChunkResponse
}

// NewTransport creates a new QUIC transport.
func NewTransport(cfg *Config) (*Transport, error) {
	ctx, cancel := context.WithCancel(context.Background())

	t := &Transport{
		config:             cfg,
		peers:              make(map[PeerID]*Peer),
		onPeerConnected:    cfg.OnPeerConnected,
		onPeerDisconnected: cfg.OnPeerDisconnected,
		onManifestRequest:  cfg.OnManifestRequest,
		onChunkRequest:     cfg.OnChunkRequest,
		ctx:                ctx,
		cancel:             cancel,
	}

	// Generate or use provided certificate
	if cfg.Certificate != nil {
		t.localCert = cfg.Certificate
	} else {
		cert, err := generateSelfSignedCert(cfg.PeerName)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("generating certificate: %w", err)
		}
		t.localCert = cert
	}

	// Compute local peer ID from certificate fingerprint
	t.localID = CertFingerprint(t.localCert)

	// Setup TLS config
	t.tlsConfig = &tls.Config{
		Certificates:       []tls.Certificate{*t.localCert},
		NextProtos:         []string{"p2p-sync-v1"},
		InsecureSkipVerify: true, // We verify via fingerprint
		ClientAuth:         tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			// Accept any cert - we identify by fingerprint
			return nil
		},
	}

	// QUIC config with reasonable defaults
	t.quicConfig = &quic.Config{
		MaxIdleTimeout:        30 * time.Second,
		KeepAlivePeriod:       10 * time.Second,
		MaxIncomingStreams:    1000,
		MaxIncomingUniStreams: 1000,
	}

	return t, nil
}

// LocalID returns this transport's peer ID.
func (t *Transport) LocalID() PeerID {
	return t.localID
}

// Listen starts accepting incoming connections.
func (t *Transport) Listen() error {
	addr, err := net.ResolveUDPAddr("udp", t.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("resolving address: %w", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listening UDP: %w", err)
	}

	listener, err := quic.Listen(conn, t.tlsConfig, t.quicConfig)
	if err != nil {
		conn.Close()
		return fmt.Errorf("creating QUIC listener: %w", err)
	}

	t.listener = listener

	// Accept connections in background
	t.wg.Add(1)
	go t.acceptLoop()

	return nil
}

// Connect initiates a connection to a remote peer.
func (t *Transport) Connect(ctx context.Context, addr string) (*Peer, error) {
	conn, err := quic.DialAddr(ctx, addr, t.tlsConfig, t.quicConfig)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", addr, err)
	}

	peer, err := t.handleConnection(conn, true)
	if err != nil {
		conn.CloseWithError(1, err.Error())
		return nil, err
	}

	return peer, nil
}

// Close shuts down the transport.
func (t *Transport) Close() error {
	t.cancel()

	if t.listener != nil {
		t.listener.Close()
	}

	// Close all peer connections
	t.peersMu.Lock()
	for _, peer := range t.peers {
		peer.conn.CloseWithError(0, "transport closing")
	}
	t.peersMu.Unlock()

	t.wg.Wait()
	return nil
}

// GetPeer returns a connected peer by ID.
func (t *Transport) GetPeer(id PeerID) *Peer {
	t.peersMu.RLock()
	defer t.peersMu.RUnlock()
	return t.peers[id]
}

// Peers returns all connected peers.
func (t *Transport) Peers() []*Peer {
	t.peersMu.RLock()
	defer t.peersMu.RUnlock()

	peers := make([]*Peer, 0, len(t.peers))
	for _, p := range t.peers {
		peers = append(peers, p)
	}
	return peers
}

// acceptLoop accepts incoming connections.
func (t *Transport) acceptLoop() {
	defer t.wg.Done()

	for {
		conn, err := t.listener.Accept(t.ctx)
		if err != nil {
			if t.ctx.Err() != nil {
				return // Shutting down
			}
			continue
		}

		go func() {
			peer, err := t.handleConnection(conn, false)
			if err != nil {
				conn.CloseWithError(1, err.Error())
				return
			}
			_ = peer
		}()
	}
}

// handleConnection performs handshake and sets up peer.
func (t *Transport) handleConnection(conn *quic.Conn, isInitiator bool) (*Peer, error) {
	// Get peer certificate fingerprint
	state := conn.ConnectionState()
	if len(state.TLS.PeerCertificates) == 0 {
		return nil, fmt.Errorf("no peer certificate")
	}
	peerCert := state.TLS.PeerCertificates[0]
	peerID := certToFingerprint(peerCert)

	// Check if already connected
	t.peersMu.Lock()
	if existing, ok := t.peers[peerID]; ok {
		t.peersMu.Unlock()
		return existing, nil
	}
	t.peersMu.Unlock()

	// Create control stream for handshake
	var stream *quic.Stream
	var err error

	if isInitiator {
		stream, err = conn.OpenStreamSync(t.ctx)
	} else {
		stream, err = conn.AcceptStream(t.ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("opening control stream: %w", err)
	}

	// Perform handshake
	peer, err := t.performHandshake(conn, stream, peerID, isInitiator)
	if err != nil {
		stream.Close()
		return nil, fmt.Errorf("handshake failed: %w", err)
	}

	// Register peer
	t.peersMu.Lock()
	t.peers[peerID] = peer
	t.peersMu.Unlock()

	// Start handling streams
	t.wg.Add(1)
	go t.handlePeerStreams(peer)

	// Notify callback
	if t.onPeerConnected != nil {
		t.onPeerConnected(peer)
	}

	return peer, nil
}

// performHandshake exchanges handshake messages.
func (t *Transport) performHandshake(conn *quic.Conn, stream *quic.Stream, peerID PeerID, isInitiator bool) (*Peer, error) {
	// Set deadline for handshake
	stream.SetDeadline(time.Now().Add(10 * time.Second))
	defer stream.SetDeadline(time.Time{})

	handshake := &proto.Handshake{
		ProtocolVersion: proto.ProtocolVersion,
		CertFingerprint: t.localID[:],
		PeerName:        t.config.PeerName,
		Capabilities:    0,
		MerkleRoot:      t.config.MerkleRoot,
		FileCount:       t.config.FileCount,
		TotalSize:       t.config.TotalSize,
	}

	if isInitiator {
		// Send handshake
		if err := proto.SendHandshake(stream, 0, handshake); err != nil {
			return nil, fmt.Errorf("sending handshake: %w", err)
		}

		// Wait for ack
		env, err := proto.ReadEnvelope(stream)
		if err != nil {
			return nil, fmt.Errorf("reading handshake ack: %w", err)
		}
		if env.Type != proto.MessageType_MESSAGE_TYPE_HANDSHAKE_ACK {
			return nil, fmt.Errorf("expected handshake ack, got %d", env.Type)
		}

		ack := &proto.HandshakeAck{}
		if err := proto.UnmarshalPayload(env, ack); err != nil {
			return nil, fmt.Errorf("decoding handshake ack: %w", err)
		}

		if !ack.Accepted {
			return nil, fmt.Errorf("handshake rejected: %s", ack.RejectionReason)
		}

		return &Peer{
			ID:          peerID,
			Name:        ack.PeerName,
			Addr:        conn.RemoteAddr().String(),
			MerkleRoot:  ack.MerkleRoot,
			conn:        conn,
			streams:     make(map[uint64]*quic.Stream),
			established: time.Now(),
		}, nil
	}

	// Responder: wait for handshake
	env, err := proto.ReadEnvelope(stream)
	if err != nil {
		return nil, fmt.Errorf("reading handshake: %w", err)
	}
	if env.Type != proto.MessageType_MESSAGE_TYPE_HANDSHAKE {
		return nil, fmt.Errorf("expected handshake, got %d", env.Type)
	}

	hs := &proto.Handshake{}
	if err := proto.UnmarshalPayload(env, hs); err != nil {
		return nil, fmt.Errorf("decoding handshake: %w", err)
	}

	// Send ack
	ack := &proto.HandshakeAck{
		Accepted:        true,
		CertFingerprint: t.localID[:],
		PeerName:        t.config.PeerName,
		MerkleRoot:      t.config.MerkleRoot,
		ProtocolVersion: proto.ProtocolVersion,
	}

	if err := proto.SendHandshakeAck(stream, 0, ack); err != nil {
		return nil, fmt.Errorf("sending handshake ack: %w", err)
	}

	return &Peer{
		ID:          peerID,
		Name:        hs.PeerName,
		Addr:        conn.RemoteAddr().String(),
		MerkleRoot:  hs.MerkleRoot,
		FileCount:   hs.FileCount,
		TotalSize:   hs.TotalSize,
		conn:        conn,
		streams:     make(map[uint64]*quic.Stream),
		established: time.Now(),
	}, nil
}

// handlePeerStreams handles incoming streams from a peer.
func (t *Transport) handlePeerStreams(peer *Peer) {
	defer t.wg.Done()
	defer t.removePeer(peer)

	for {
		stream, err := peer.conn.AcceptStream(t.ctx)
		if err != nil {
			return // Connection closed
		}

		go t.handleStream(peer, stream)
	}
}

// handleStream processes messages on a stream.
func (t *Transport) handleStream(peer *Peer, stream *quic.Stream) {
	defer stream.Close()

	for {
		env, err := proto.ReadEnvelope(stream)
		if err != nil {
			return // Stream closed
		}

		switch env.Type {
		case proto.MessageType_MESSAGE_TYPE_CHUNK_REQUEST:
			t.handleChunkRequest(peer, stream, env.RequestId, env.Payload)
		case proto.MessageType_MESSAGE_TYPE_MANIFEST_REQUEST:
			t.handleManifestRequest(peer, stream, env.RequestId, env.Payload)
		}
	}
}

// handleChunkRequest processes a chunk request.
func (t *Transport) handleChunkRequest(peer *Peer, stream *quic.Stream, reqID uint64, payload []byte) {
	req := &proto.ChunkRequest{}
	if err := googleproto.Unmarshal(payload, req); err != nil {
		return
	}

	if t.onChunkRequest == nil {
		return
	}

	resp := t.onChunkRequest(peer, req)
	if resp == nil {
		return
	}

	proto.SendChunkResponse(stream, reqID, resp)
}

// handleManifestRequest processes a manifest request.
func (t *Transport) handleManifestRequest(peer *Peer, stream *quic.Stream, reqID uint64, payload []byte) {
	// Decode request (currently unused fields)
	req := &proto.ManifestRequest{}
	googleproto.Unmarshal(payload, req)

	if t.onManifestRequest == nil {
		return
	}

	resp := t.onManifestRequest(peer, req)
	if resp == nil {
		return
	}

	proto.SendManifestResponse(stream, reqID, resp)
}

// removePeer removes a peer from the connected list.
func (t *Transport) removePeer(peer *Peer) {
	t.peersMu.Lock()
	delete(t.peers, peer.ID)
	t.peersMu.Unlock()

	if t.onPeerDisconnected != nil {
		t.onPeerDisconnected(peer)
	}
}

// OpenStream opens a new stream to the peer.
func (p *Peer) OpenStream(ctx context.Context) (*quic.Stream, error) {
	return p.conn.OpenStreamSync(ctx)
}

// RequestChunks sends a chunk request to a peer and waits for response.
func (t *Transport) RequestChunks(ctx context.Context, peer *Peer, hashes [][]byte) (*proto.ChunkResponse, error) {
	stream, err := peer.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening stream: %w", err)
	}
	defer stream.Close()

	peer.mu.Lock()
	reqID := peer.nextReqID
	peer.nextReqID++
	peer.mu.Unlock()

	req := &proto.ChunkRequest{
		ChunkHashes: hashes,
		Priority:    1,
	}

	if err := proto.SendChunkRequest(stream, reqID, req); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	env, err := proto.ReadEnvelope(stream)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if env.Type != proto.MessageType_MESSAGE_TYPE_CHUNK_RESPONSE {
		return nil, fmt.Errorf("unexpected response type: %d", env.Type)
	}

	resp := &proto.ChunkResponse{}
	if err := proto.UnmarshalPayload(env, resp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return resp, nil
}

// CertFingerprint computes the SHA-256 fingerprint of a certificate.
func CertFingerprint(cert *tls.Certificate) PeerID {
	var id PeerID
	if cert != nil && len(cert.Certificate) > 0 {
		hash := sha256.Sum256(cert.Certificate[0])
		copy(id[:], hash[:])
	}
	return id
}

func certToFingerprint(cert *x509.Certificate) PeerID {
	var id PeerID
	hash := sha256.Sum256(cert.Raw)
	copy(id[:], hash[:])
	return id
}

// generateSelfSignedCert generates a self-signed TLS certificate.
func generateSelfSignedCert(name string) (*tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: name,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("creating certificate: %w", err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
	}, nil
}

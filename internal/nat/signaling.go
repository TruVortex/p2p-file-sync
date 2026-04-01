// Package nat - signaling.go implements a lightweight signaling server
// for P2P connection establishment and peer discovery.
package nat

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Signaling message types
const (
	MsgTypeRegister     = 0x01 // Client registers with server
	MsgTypeRegisterAck  = 0x02 // Server acknowledges registration
	MsgTypePeerRequest  = 0x03 // Request peer's address by ID
	MsgTypePeerResponse = 0x04 // Response with peer's address
	MsgTypePeerNotify   = 0x05 // Notify peer of connection attempt
	MsgTypeError        = 0xFF // Error response

	// Timeouts
	SignalingTimeout  = 10 * time.Second
	PeerEntryExpiry   = 5 * time.Minute
	HeartbeatInterval = 30 * time.Second
)

var (
	ErrPeerNotFound    = errors.New("peer not found")
	ErrRegistrationErr = errors.New("registration failed")
	ErrServerClosed    = errors.New("signaling server closed")
)

// PeerInfo contains information about a registered peer.
type PeerInfo struct {
	ID         string    // Unique peer identifier
	PublicAddr string    // Public IP:port (from STUN)
	LocalAddr  string    // Local IP:port
	LastSeen   time.Time // Last heartbeat time
}

// SignalingServer helps peers discover each other for hole punching.
type SignalingServer struct {
	listener net.Listener
	peers    map[string]*PeerInfo
	mu       sync.RWMutex
	done     chan struct{}
	wg       sync.WaitGroup
}

// NewSignalingServer creates a new signaling server.
func NewSignalingServer() *SignalingServer {
	return &SignalingServer{
		peers: make(map[string]*PeerInfo),
		done:  make(chan struct{}),
	}
}

// Listen starts the signaling server on the specified address.
func (s *SignalingServer) Listen(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("starting listener: %w", err)
	}
	s.listener = listener

	// Start cleanup goroutine
	s.wg.Add(1)
	go s.cleanupExpiredPeers()

	// Accept connections
	s.wg.Add(1)
	go s.acceptLoop()

	return nil
}

// Addr returns the server's listen address.
func (s *SignalingServer) Addr() net.Addr {
	if s.listener != nil {
		return s.listener.Addr()
	}
	return nil
}

// Close shuts down the signaling server.
func (s *SignalingServer) Close() error {
	close(s.done)
	if s.listener != nil {
		s.listener.Close()
	}
	s.wg.Wait()
	return nil
}

// RegisteredPeers returns the current number of registered peers.
func (s *SignalingServer) RegisteredPeers() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peers)
}

func (s *SignalingServer) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				continue
			}
		}

		go s.handleConnection(conn)
	}
}

func (s *SignalingServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	// Set read deadline
	conn.SetDeadline(time.Now().Add(SignalingTimeout))

	// Read message header (type + length)
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}

	msgType := header[0]
	msgLen := binary.BigEndian.Uint32(header[1:5])

	if msgLen > 1024 { // Sanity check
		s.sendError(conn, "message too large")
		return
	}

	// Read message body
	body := make([]byte, msgLen)
	if msgLen > 0 {
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
	}

	switch msgType {
	case MsgTypeRegister:
		s.handleRegister(conn, body)
	case MsgTypePeerRequest:
		s.handlePeerRequest(conn, body)
	default:
		s.sendError(conn, "unknown message type")
	}
}

func (s *SignalingServer) handleRegister(conn net.Conn, body []byte) {
	// Body format: peerID(len:2 + data) + publicAddr(len:2 + data) + localAddr(len:2 + data)
	if len(body) < 6 {
		s.sendError(conn, "invalid register message")
		return
	}

	pos := 0

	// Read peer ID
	idLen := binary.BigEndian.Uint16(body[pos : pos+2])
	pos += 2
	if pos+int(idLen) > len(body) {
		s.sendError(conn, "invalid peer ID")
		return
	}
	peerID := string(body[pos : pos+int(idLen)])
	pos += int(idLen)

	// Read public address
	if pos+2 > len(body) {
		s.sendError(conn, "invalid public address")
		return
	}
	pubLen := binary.BigEndian.Uint16(body[pos : pos+2])
	pos += 2
	if pos+int(pubLen) > len(body) {
		s.sendError(conn, "invalid public address")
		return
	}
	publicAddr := string(body[pos : pos+int(pubLen)])
	pos += int(pubLen)

	// Read local address
	if pos+2 > len(body) {
		s.sendError(conn, "invalid local address")
		return
	}
	localLen := binary.BigEndian.Uint16(body[pos : pos+2])
	pos += 2
	if pos+int(localLen) > len(body) {
		s.sendError(conn, "invalid local address")
		return
	}
	localAddr := string(body[pos : pos+int(localLen)])

	// Register peer
	s.mu.Lock()
	s.peers[peerID] = &PeerInfo{
		ID:         peerID,
		PublicAddr: publicAddr,
		LocalAddr:  localAddr,
		LastSeen:   time.Now(),
	}
	s.mu.Unlock()

	// Send acknowledgment
	s.sendRegisterAck(conn, peerID)
}

func (s *SignalingServer) handlePeerRequest(conn net.Conn, body []byte) {
	if len(body) < 2 {
		s.sendError(conn, "invalid peer request")
		return
	}

	idLen := binary.BigEndian.Uint16(body[0:2])
	if len(body) < 2+int(idLen) {
		s.sendError(conn, "invalid peer ID")
		return
	}
	peerID := string(body[2 : 2+int(idLen)])

	s.mu.RLock()
	peer, exists := s.peers[peerID]
	s.mu.RUnlock()

	if !exists {
		s.sendError(conn, "peer not found")
		return
	}

	s.sendPeerResponse(conn, peer)
}

func (s *SignalingServer) sendRegisterAck(conn net.Conn, peerID string) {
	// Response: type(1) + len(4) + peerID
	idBytes := []byte(peerID)
	msg := make([]byte, 5+len(idBytes))
	msg[0] = MsgTypeRegisterAck
	binary.BigEndian.PutUint32(msg[1:5], uint32(len(idBytes)))
	copy(msg[5:], idBytes)
	conn.Write(msg)
}

func (s *SignalingServer) sendPeerResponse(conn net.Conn, peer *PeerInfo) {
	// Response: type(1) + len(4) + publicAddr(2+data) + localAddr(2+data)
	pubBytes := []byte(peer.PublicAddr)
	localBytes := []byte(peer.LocalAddr)

	bodyLen := 2 + len(pubBytes) + 2 + len(localBytes)
	msg := make([]byte, 5+bodyLen)

	msg[0] = MsgTypePeerResponse
	binary.BigEndian.PutUint32(msg[1:5], uint32(bodyLen))

	pos := 5
	binary.BigEndian.PutUint16(msg[pos:pos+2], uint16(len(pubBytes)))
	pos += 2
	copy(msg[pos:], pubBytes)
	pos += len(pubBytes)

	binary.BigEndian.PutUint16(msg[pos:pos+2], uint16(len(localBytes)))
	pos += 2
	copy(msg[pos:], localBytes)

	conn.Write(msg)
}

func (s *SignalingServer) sendError(conn net.Conn, errMsg string) {
	errBytes := []byte(errMsg)
	msg := make([]byte, 5+len(errBytes))
	msg[0] = MsgTypeError
	binary.BigEndian.PutUint32(msg[1:5], uint32(len(errBytes)))
	copy(msg[5:], errBytes)
	conn.Write(msg)
}

func (s *SignalingServer) cleanupExpiredPeers() {
	defer s.wg.Done()

	ticker := time.NewTicker(PeerEntryExpiry / 2)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for id, peer := range s.peers {
				if now.Sub(peer.LastSeen) > PeerEntryExpiry {
					delete(s.peers, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

// SignalingClient connects to a signaling server for peer discovery.
type SignalingClient struct {
	serverAddr string
	timeout    time.Duration
}

// NewSignalingClient creates a new signaling client.
func NewSignalingClient(serverAddr string) *SignalingClient {
	return &SignalingClient{
		serverAddr: serverAddr,
		timeout:    SignalingTimeout,
	}
}

// Register announces this peer to the signaling server.
func (c *SignalingClient) Register(ctx context.Context, peerID, publicAddr, localAddr string) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Build register message
	idBytes := []byte(peerID)
	pubBytes := []byte(publicAddr)
	localBytes := []byte(localAddr)

	bodyLen := 2 + len(idBytes) + 2 + len(pubBytes) + 2 + len(localBytes)
	msg := make([]byte, 5+bodyLen)

	msg[0] = MsgTypeRegister
	binary.BigEndian.PutUint32(msg[1:5], uint32(bodyLen))

	pos := 5
	binary.BigEndian.PutUint16(msg[pos:pos+2], uint16(len(idBytes)))
	pos += 2
	copy(msg[pos:], idBytes)
	pos += len(idBytes)

	binary.BigEndian.PutUint16(msg[pos:pos+2], uint16(len(pubBytes)))
	pos += 2
	copy(msg[pos:], pubBytes)
	pos += len(pubBytes)

	binary.BigEndian.PutUint16(msg[pos:pos+2], uint16(len(localBytes)))
	pos += 2
	copy(msg[pos:], localBytes)

	if _, err := conn.Write(msg); err != nil {
		return fmt.Errorf("sending register message: %w", err)
	}

	// Read response
	resp, err := c.readResponse(conn)
	if err != nil {
		return err
	}

	if resp.msgType == MsgTypeError {
		return fmt.Errorf("registration error: %s", string(resp.body))
	}

	if resp.msgType != MsgTypeRegisterAck {
		return fmt.Errorf("unexpected response type: %d", resp.msgType)
	}

	return nil
}

// LookupPeer queries the signaling server for a peer's address.
func (c *SignalingClient) LookupPeer(ctx context.Context, peerID string) (*PeerInfo, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Build peer request message
	idBytes := []byte(peerID)
	msg := make([]byte, 5+2+len(idBytes))
	msg[0] = MsgTypePeerRequest
	binary.BigEndian.PutUint32(msg[1:5], uint32(2+len(idBytes)))
	binary.BigEndian.PutUint16(msg[5:7], uint16(len(idBytes)))
	copy(msg[7:], idBytes)

	if _, err := conn.Write(msg); err != nil {
		return nil, fmt.Errorf("sending peer request: %w", err)
	}

	// Read response
	resp, err := c.readResponse(conn)
	if err != nil {
		return nil, err
	}

	if resp.msgType == MsgTypeError {
		if string(resp.body) == "peer not found" {
			return nil, ErrPeerNotFound
		}
		return nil, fmt.Errorf("lookup error: %s", string(resp.body))
	}

	if resp.msgType != MsgTypePeerResponse {
		return nil, fmt.Errorf("unexpected response type: %d", resp.msgType)
	}

	// Parse response
	return c.parsePeerResponse(resp.body)
}

func (c *SignalingClient) connect(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	d.Timeout = c.timeout

	conn, err := d.DialContext(ctx, "tcp", c.serverAddr)
	if err != nil {
		return nil, fmt.Errorf("connecting to signaling server: %w", err)
	}

	conn.SetDeadline(time.Now().Add(c.timeout))
	return conn, nil
}

type signalingResponse struct {
	msgType byte
	body    []byte
}

func (c *SignalingClient) readResponse(conn net.Conn) (*signalingResponse, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, fmt.Errorf("reading response header: %w", err)
	}

	msgType := header[0]
	msgLen := binary.BigEndian.Uint32(header[1:5])

	if msgLen > 1024 {
		return nil, errors.New("response too large")
	}

	body := make([]byte, msgLen)
	if msgLen > 0 {
		if _, err := io.ReadFull(conn, body); err != nil {
			return nil, fmt.Errorf("reading response body: %w", err)
		}
	}

	return &signalingResponse{msgType: msgType, body: body}, nil
}

func (c *SignalingClient) parsePeerResponse(body []byte) (*PeerInfo, error) {
	if len(body) < 4 {
		return nil, errors.New("invalid peer response")
	}

	pos := 0

	// Read public address
	pubLen := binary.BigEndian.Uint16(body[pos : pos+2])
	pos += 2
	if pos+int(pubLen) > len(body) {
		return nil, errors.New("invalid public address in response")
	}
	publicAddr := string(body[pos : pos+int(pubLen)])
	pos += int(pubLen)

	// Read local address
	if pos+2 > len(body) {
		return nil, errors.New("invalid local address in response")
	}
	localLen := binary.BigEndian.Uint16(body[pos : pos+2])
	pos += 2
	if pos+int(localLen) > len(body) {
		return nil, errors.New("invalid local address in response")
	}
	localAddr := string(body[pos : pos+int(localLen)])

	return &PeerInfo{
		PublicAddr: publicAddr,
		LocalAddr:  localAddr,
	}, nil
}

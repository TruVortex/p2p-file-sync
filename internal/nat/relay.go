// Package nat - relay.go implements a TCP relay server and client for fallback
// when UDP hole punching fails (e.g., symmetric NAT).
//
// TURN RELAY ARCHITECTURE:
//
// When UDP hole punching fails (typically with symmetric NAT), peers need a relay server
// to forward traffic. This can be implemented in several ways:
//
// 1. TURN Protocol (RFC 5766):
//    - Industry standard for relay
//    - Allocates relay addresses, handles permissions
//    - Libraries: pion/turn (Go), coturn (server)
//
// 2. Simple TCP Proxy (implemented here):
//    - Lightweight alternative for controlled deployments
//    - Peers connect to relay, relay bridges the connections
//    - Lower complexity, good for private networks
//
// INTEGRATION GUIDE:
//
// To integrate TURN into this P2P system:
//
// 1. Deploy a TURN server (e.g., coturn) on a public VPS:
//    ```
//    apt-get install coturn
//    # Configure /etc/turnserver.conf:
//    # listening-port=3478
//    # realm=yourdomain.com
//    # user=username:password
//    # lt-cred-mech
//    ```
//
// 2. In the client code, after hole punch failure:
//    ```go
//    import "github.com/pion/turn/v2"
//
//    cfg := &turn.ClientConfig{
//        STUNServerAddr: "turn.example.com:3478",
//        TURNServerAddr: "turn.example.com:3478",
//        Username:       "user",
//        Password:       "pass",
//    }
//    client, _ := turn.NewClient(cfg)
//    relayConn, _ := client.Allocate()
//    // Use relayConn for P2P communication
//    ```
//
// 3. The relayed connection can be used with the existing QUIC transport:
//    ```go
//    // Wrap relay connection for QUIC
//    quicConfig := &quic.Config{...}
//    quicConn, _ := quic.Dial(relayConn, remoteAddr, tlsConfig, quicConfig)
//    ```

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

// Relay message types
const (
	RelayMsgConnect    = 0x01 // Client requests connection to peer
	RelayMsgConnectAck = 0x02 // Relay acknowledges and provides session
	RelayMsgData       = 0x03 // Data packet
	RelayMsgClose      = 0x04 // Close session
	RelayMsgError      = 0xFF // Error

	// Configuration
	RelayBufferSize = 32 * 1024
	RelayTimeout    = 30 * time.Second
)

var (
	ErrRelaySessionNotFound = errors.New("relay session not found")
	ErrRelayPeerNotReady    = errors.New("peer not connected to relay")
	ErrRelayClosed          = errors.New("relay connection closed")
)

// RelaySession represents a relayed connection between two peers.
type RelaySession struct {
	ID      string
	PeerA   net.Conn
	PeerB   net.Conn
	Created time.Time
	mu      sync.Mutex
	closed  bool
}

// RelayServer provides TCP relay for peers that cannot establish direct connections.
type RelayServer struct {
	listener net.Listener
	sessions map[string]*RelaySession
	waiting  map[string]net.Conn // Peers waiting for their counterpart
	mu       sync.RWMutex
	done     chan struct{}
	wg       sync.WaitGroup
}

// NewRelayServer creates a new TCP relay server.
func NewRelayServer() *RelayServer {
	return &RelayServer{
		sessions: make(map[string]*RelaySession),
		waiting:  make(map[string]net.Conn),
		done:     make(chan struct{}),
	}
}

// Listen starts the relay server on the specified address.
func (rs *RelayServer) Listen(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("starting relay listener: %w", err)
	}
	rs.listener = listener

	rs.wg.Add(1)
	go rs.acceptLoop()

	return nil
}

// Addr returns the server's listen address.
func (rs *RelayServer) Addr() net.Addr {
	if rs.listener != nil {
		return rs.listener.Addr()
	}
	return nil
}

// Close shuts down the relay server.
func (rs *RelayServer) Close() error {
	close(rs.done)
	if rs.listener != nil {
		rs.listener.Close()
	}
	rs.wg.Wait()

	// Close all sessions
	rs.mu.Lock()
	for _, session := range rs.sessions {
		session.Close()
	}
	for _, conn := range rs.waiting {
		conn.Close()
	}
	rs.mu.Unlock()

	return nil
}

func (rs *RelayServer) acceptLoop() {
	defer rs.wg.Done()

	for {
		conn, err := rs.listener.Accept()
		if err != nil {
			select {
			case <-rs.done:
				return
			default:
				continue
			}
		}

		go rs.handleConnection(conn)
	}
}

func (rs *RelayServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(RelayTimeout))

	// Read connect request
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}

	if header[0] != RelayMsgConnect {
		rs.sendError(conn, "expected connect message")
		return
	}

	sessionIDLen := binary.BigEndian.Uint32(header[1:5])
	if sessionIDLen > 256 {
		rs.sendError(conn, "session ID too long")
		return
	}

	sessionIDBytes := make([]byte, sessionIDLen)
	if _, err := io.ReadFull(conn, sessionIDBytes); err != nil {
		return
	}
	sessionID := string(sessionIDBytes)

	// Check if peer is already waiting
	rs.mu.Lock()
	if waitingConn, exists := rs.waiting[sessionID]; exists {
		// Pair found - create session and start relaying
		delete(rs.waiting, sessionID)

		session := &RelaySession{
			ID:      sessionID,
			PeerA:   waitingConn,
			PeerB:   conn,
			Created: time.Now(),
		}
		rs.sessions[sessionID] = session
		rs.mu.Unlock()

		// Send ack to both peers
		rs.sendConnectAck(waitingConn, sessionID)
		rs.sendConnectAck(conn, sessionID)

		// Clear deadlines for relay
		waitingConn.SetDeadline(time.Time{})
		conn.SetDeadline(time.Time{})

		// Start bidirectional relay
		rs.relay(session)
	} else {
		// First peer - wait for counterpart
		rs.waiting[sessionID] = conn
		rs.mu.Unlock()

		// Wait for pairing or timeout
		select {
		case <-rs.done:
			return
		case <-time.After(RelayTimeout):
			rs.mu.Lock()
			delete(rs.waiting, sessionID)
			rs.mu.Unlock()
			rs.sendError(conn, "timeout waiting for peer")
		}
	}
}

func (rs *RelayServer) relay(session *RelaySession) {
	defer session.Close()

	// Remove session when done
	defer func() {
		rs.mu.Lock()
		delete(rs.sessions, session.ID)
		rs.mu.Unlock()
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// A -> B
	go func() {
		defer wg.Done()
		rs.copyWithHeader(session.PeerB, session.PeerA)
	}()

	// B -> A
	go func() {
		defer wg.Done()
		rs.copyWithHeader(session.PeerA, session.PeerB)
	}()

	wg.Wait()
}

func (rs *RelayServer) copyWithHeader(dst, src net.Conn) {
	buf := make([]byte, RelayBufferSize)

	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}

		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (rs *RelayServer) sendConnectAck(conn net.Conn, sessionID string) {
	msg := make([]byte, 5+len(sessionID))
	msg[0] = RelayMsgConnectAck
	binary.BigEndian.PutUint32(msg[1:5], uint32(len(sessionID)))
	copy(msg[5:], sessionID)
	conn.Write(msg)
}

func (rs *RelayServer) sendError(conn net.Conn, errMsg string) {
	msg := make([]byte, 5+len(errMsg))
	msg[0] = RelayMsgError
	binary.BigEndian.PutUint32(msg[1:5], uint32(len(errMsg)))
	copy(msg[5:], errMsg)
	conn.Write(msg)
}

// Close closes the relay session.
func (s *RelaySession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true

	if s.PeerA != nil {
		s.PeerA.Close()
	}
	if s.PeerB != nil {
		s.PeerB.Close()
	}
}

// RelayClient connects to a relay server for fallback P2P communication.
type RelayClient struct {
	serverAddr string
	conn       net.Conn
	sessionID  string
	mu         sync.Mutex
}

// NewRelayClient creates a new relay client.
func NewRelayClient(serverAddr string) *RelayClient {
	return &RelayClient{
		serverAddr: serverAddr,
	}
}

// Connect establishes a relayed connection with a peer using the session ID.
// Both peers must use the same session ID to be paired.
func (rc *RelayClient) Connect(ctx context.Context, sessionID string) (net.Conn, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	// Connect to relay server
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", rc.serverAddr)
	if err != nil {
		return nil, fmt.Errorf("connecting to relay: %w", err)
	}

	// Set deadline
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	} else {
		conn.SetDeadline(time.Now().Add(RelayTimeout))
	}

	// Send connect request
	msg := make([]byte, 5+len(sessionID))
	msg[0] = RelayMsgConnect
	binary.BigEndian.PutUint32(msg[1:5], uint32(len(sessionID)))
	copy(msg[5:], sessionID)

	if _, err := conn.Write(msg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending connect request: %w", err)
	}

	// Wait for acknowledgment
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading response: %w", err)
	}

	msgType := header[0]
	msgLen := binary.BigEndian.Uint32(header[1:5])

	if msgType == RelayMsgError {
		errMsg := make([]byte, msgLen)
		io.ReadFull(conn, errMsg)
		conn.Close()
		return nil, fmt.Errorf("relay error: %s", string(errMsg))
	}

	if msgType != RelayMsgConnectAck {
		conn.Close()
		return nil, fmt.Errorf("unexpected response: %d", msgType)
	}

	// Read session ID from ack
	ackSessionID := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, ackSessionID); err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading session ID: %w", err)
	}

	// Clear deadline - connection is established
	conn.SetDeadline(time.Time{})

	rc.conn = conn
	rc.sessionID = string(ackSessionID)

	return conn, nil
}

// Close closes the relay connection.
func (rc *RelayClient) Close() error {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	if rc.conn != nil {
		rc.conn.Close()
		rc.conn = nil
	}
	return nil
}

// GetConn returns the underlying relay connection.
func (rc *RelayClient) GetConn() net.Conn {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.conn
}

// RelayedConn wraps a relay connection with session management.
type RelayedConn struct {
	net.Conn
	sessionID string
}

// SessionID returns the relay session identifier.
func (rc *RelayedConn) SessionID() string {
	return rc.sessionID
}

// FallbackStrategy coordinates hole punching with relay fallback.
type FallbackStrategy struct {
	holePuncher *HolePuncher
	relayAddr   string
}

// NewFallbackStrategy creates a strategy that tries hole punching then falls back to relay.
func NewFallbackStrategy(signalingAddr, relayAddr, peerID string) *FallbackStrategy {
	return &FallbackStrategy{
		holePuncher: NewHolePuncher(signalingAddr, peerID),
		relayAddr:   relayAddr,
	}
}

// Connect tries to establish a connection, first via hole punching, then relay.
func (fs *FallbackStrategy) Connect(ctx context.Context, remotePeerID, sessionID string) (*ConnectionResult, error) {
	result := &ConnectionResult{}

	// Initialize hole puncher
	if err := fs.holePuncher.Initialize(ctx); err != nil {
		// If STUN fails, go straight to relay
		result.Method = "relay"
		return fs.connectViaRelay(ctx, sessionID, result)
	}

	// Attempt hole punching
	hpResult, err := fs.holePuncher.Connect(ctx, remotePeerID)
	if err == nil && hpResult.Success {
		result.Method = "hole_punch"
		result.UDPConn = hpResult.Conn
		result.RemoteAddr = hpResult.RemoteAddr
		result.Attempts = hpResult.Attempts
		return result, nil
	}

	// Hole punching failed - try relay
	result.Method = "relay"
	result.HolePunchAttempts = hpResult.Attempts
	return fs.connectViaRelay(ctx, sessionID, result)
}

func (fs *FallbackStrategy) connectViaRelay(ctx context.Context, sessionID string, result *ConnectionResult) (*ConnectionResult, error) {
	if fs.relayAddr == "" {
		return result, ErrHolePunchFailed
	}

	client := NewRelayClient(fs.relayAddr)
	conn, err := client.Connect(ctx, sessionID)
	if err != nil {
		return result, fmt.Errorf("relay connection failed: %w", err)
	}

	result.RelayConn = conn
	return result, nil
}

// Close releases resources.
func (fs *FallbackStrategy) Close() error {
	return fs.holePuncher.Close()
}

// ConnectionResult contains the outcome of a connection attempt.
type ConnectionResult struct {
	Method            string // "hole_punch" or "relay"
	UDPConn           *net.UDPConn
	RelayConn         net.Conn
	RemoteAddr        *net.UDPAddr
	Attempts          int
	HolePunchAttempts int
}

// GetConn returns the appropriate connection based on the method used.
func (cr *ConnectionResult) GetConn() net.Conn {
	if cr.RelayConn != nil {
		return cr.RelayConn
	}
	if cr.UDPConn != nil {
		return cr.UDPConn
	}
	return nil
}

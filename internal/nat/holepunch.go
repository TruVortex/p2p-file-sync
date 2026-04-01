// Package nat - holepunch.go implements UDP hole punching for NAT traversal.
package nat

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Hole punch message types
const (
	HolePunchSyn    = 0x01 // Initial punch attempt
	HolePunchSynAck = 0x02 // Acknowledgment
	HolePunchAck    = 0x03 // Final acknowledgment

	// Configuration
	MaxPunchAttempts = 5
	PunchInterval    = 500 * time.Millisecond
	PunchTimeout     = 5 * time.Second
)

var (
	ErrHolePunchFailed = errors.New("UDP hole punching failed after max attempts")
	ErrPunchTimeout    = errors.New("hole punch timed out")
	ErrSymmetricNAT    = errors.New("symmetric NAT detected - hole punching may not work")
)

// HolePunchResult contains the outcome of a hole punch attempt.
type HolePunchResult struct {
	Success    bool
	Conn       *net.UDPConn
	RemoteAddr *net.UDPAddr
	Attempts   int
	Method     string // "direct", "hole_punch", "relay"
}

// HolePuncher manages UDP hole punching for P2P connections.
type HolePuncher struct {
	stunClient      *StunClient
	signalingClient *SignalingClient
	peerID          string
	localConn       *net.UDPConn
	publicAddr      *MappedAddress
	mu              sync.Mutex
}

// NewHolePuncher creates a new hole puncher with the given signaling server.
func NewHolePuncher(signalingServerAddr, peerID string) *HolePuncher {
	return &HolePuncher{
		stunClient:      NewStunClient(),
		signalingClient: NewSignalingClient(signalingServerAddr),
		peerID:          peerID,
	}
}

// Initialize discovers our public address and registers with the signaling server.
func (hp *HolePuncher) Initialize(ctx context.Context) error {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	// Create UDP socket and discover public address
	publicAddr, conn, err := hp.stunClient.DiscoverWithNewConn(ctx)
	if err != nil {
		return fmt.Errorf("STUN discovery failed: %w", err)
	}

	hp.localConn = conn
	hp.publicAddr = publicAddr

	// Get local address
	localAddr := conn.LocalAddr().String()

	// Register with signaling server
	err = hp.signalingClient.Register(ctx, hp.peerID, publicAddr.String(), localAddr)
	if err != nil {
		conn.Close()
		return fmt.Errorf("signaling registration failed: %w", err)
	}

	return nil
}

// Connect attempts to establish a UDP connection to a peer using hole punching.
func (hp *HolePuncher) Connect(ctx context.Context, remotePeerID string) (*HolePunchResult, error) {
	hp.mu.Lock()
	conn := hp.localConn
	hp.mu.Unlock()

	if conn == nil {
		return nil, errors.New("not initialized - call Initialize() first")
	}

	// Look up peer's address from signaling server
	peerInfo, err := hp.signalingClient.LookupPeer(ctx, remotePeerID)
	if err != nil {
		return nil, fmt.Errorf("peer lookup failed: %w", err)
	}

	// Parse peer's addresses
	publicAddr, err := net.ResolveUDPAddr("udp4", peerInfo.PublicAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid peer public address: %w", err)
	}

	localAddr, err := net.ResolveUDPAddr("udp4", peerInfo.LocalAddr)
	if err != nil {
		// Local address might not be reachable, continue with public only
		localAddr = nil
	}

	// Generate session ID for this punch attempt
	sessionID := make([]byte, 8)
	rand.Read(sessionID)

	// Try hole punching
	result := hp.attemptHolePunch(ctx, conn, publicAddr, localAddr, sessionID)

	return result, nil
}

// attemptHolePunch tries to punch through NAT using simultaneous open.
func (hp *HolePuncher) attemptHolePunch(
	ctx context.Context,
	conn *net.UDPConn,
	publicAddr, localAddr *net.UDPAddr,
	sessionID []byte,
) *HolePunchResult {
	result := &HolePunchResult{
		Conn:     conn,
		Attempts: 0,
	}

	// Create deadline context
	ctx, cancel := context.WithTimeout(ctx, PunchTimeout)
	defer cancel()

	// Channel to receive successful connection
	successCh := make(chan *net.UDPAddr, 1)
	errCh := make(chan error, 1)

	// Start listener for incoming punch packets
	go hp.listenForPunch(ctx, conn, sessionID, successCh, errCh)

	// Send punch packets to both addresses
	targets := []*net.UDPAddr{publicAddr}
	if localAddr != nil && !localAddr.IP.Equal(publicAddr.IP) {
		targets = append(targets, localAddr)
	}

	// Attempt hole punching
	ticker := time.NewTicker(PunchInterval)
	defer ticker.Stop()

	for result.Attempts < MaxPunchAttempts {
		select {
		case <-ctx.Done():
			result.Success = false
			return result

		case addr := <-successCh:
			result.Success = true
			result.RemoteAddr = addr
			result.Method = "hole_punch"
			return result

		case err := <-errCh:
			if err != nil {
				// Log error but continue trying
				_ = err
			}

		case <-ticker.C:
			result.Attempts++

			// Send SYN to all targets
			for _, target := range targets {
				hp.sendPunchPacket(conn, target, HolePunchSyn, sessionID)
			}
		}
	}

	result.Success = false
	return result
}

// listenForPunch listens for incoming hole punch packets.
func (hp *HolePuncher) listenForPunch(
	ctx context.Context,
	conn *net.UDPConn,
	expectedSessionID []byte,
	successCh chan<- *net.UDPAddr,
	errCh chan<- error,
) {
	buf := make([]byte, 64)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Set short read deadline to check context periodically
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case errCh <- err:
			default:
			}
			continue
		}

		if n < 13 { // 1 (type) + 4 (magic) + 8 (session)
			continue
		}

		// Parse punch packet
		msgType := buf[0]
		magic := binary.BigEndian.Uint32(buf[1:5])
		sessionID := buf[5:13]

		// Verify magic number
		if magic != 0x50554E43 { // "PUNC"
			continue
		}

		// Verify session ID
		match := true
		for i := 0; i < 8; i++ {
			if sessionID[i] != expectedSessionID[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}

		switch msgType {
		case HolePunchSyn:
			// Received SYN, send SYN-ACK
			hp.sendPunchPacket(conn, remoteAddr, HolePunchSynAck, expectedSessionID)

		case HolePunchSynAck:
			// Received SYN-ACK, send ACK and report success
			hp.sendPunchPacket(conn, remoteAddr, HolePunchAck, expectedSessionID)
			select {
			case successCh <- remoteAddr:
			default:
			}
			return

		case HolePunchAck:
			// Received final ACK, connection established
			select {
			case successCh <- remoteAddr:
			default:
			}
			return
		}
	}
}

// sendPunchPacket sends a hole punch packet.
func (hp *HolePuncher) sendPunchPacket(conn *net.UDPConn, addr *net.UDPAddr, msgType byte, sessionID []byte) {
	// Packet format: type(1) + magic(4) + sessionID(8) = 13 bytes
	packet := make([]byte, 13)
	packet[0] = msgType
	binary.BigEndian.PutUint32(packet[1:5], 0x50554E43) // "PUNC"
	copy(packet[5:13], sessionID)

	conn.WriteToUDP(packet, addr)
}

// GetPublicAddress returns our discovered public address.
func (hp *HolePuncher) GetPublicAddress() *MappedAddress {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	return hp.publicAddr
}

// GetLocalConn returns the local UDP connection.
func (hp *HolePuncher) GetLocalConn() *net.UDPConn {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	return hp.localConn
}

// Close releases resources.
func (hp *HolePuncher) Close() error {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	if hp.localConn != nil {
		hp.localConn.Close()
		hp.localConn = nil
	}
	return nil
}

// ConnectWithFallback attempts hole punching with relay fallback.
// If hole punching fails, returns information about using a relay.
func (hp *HolePuncher) ConnectWithFallback(ctx context.Context, remotePeerID string, relayAddr string) (*HolePunchResult, error) {
	// First try direct hole punching
	result, err := hp.Connect(ctx, remotePeerID)
	if err != nil {
		return nil, err
	}

	if result.Success {
		return result, nil
	}

	// Hole punching failed - would need to use relay
	// Return result indicating fallback is needed
	result.Method = "relay_needed"
	return result, ErrHolePunchFailed
}

// SimultaneousOpen performs hole punching where both peers punch at the same time.
// This is coordinated through the signaling server.
type SimultaneousOpen struct {
	hp        *HolePuncher
	startTime time.Time
	syncDelay time.Duration
}

// NewSimultaneousOpen creates a coordinator for simultaneous hole punching.
func NewSimultaneousOpen(hp *HolePuncher, syncDelay time.Duration) *SimultaneousOpen {
	return &SimultaneousOpen{
		hp:        hp,
		syncDelay: syncDelay,
	}
}

// ScheduleAndPunch coordinates timing with peer for simultaneous punching.
// Both peers should call this at approximately the same time (coordinated via signaling).
func (so *SimultaneousOpen) ScheduleAndPunch(ctx context.Context, targetAddr *net.UDPAddr) (*HolePunchResult, error) {
	conn := so.hp.GetLocalConn()
	if conn == nil {
		return nil, errors.New("not initialized")
	}

	// Generate session ID
	sessionID := make([]byte, 8)
	rand.Read(sessionID)

	result := &HolePunchResult{
		Conn:     conn,
		Attempts: 0,
	}

	// Wait for sync delay (allows both peers to start around the same time)
	time.Sleep(so.syncDelay)

	// Create deadline context
	ctx, cancel := context.WithTimeout(ctx, PunchTimeout)
	defer cancel()

	successCh := make(chan *net.UDPAddr, 1)
	errCh := make(chan error, 1)

	// Listen for responses
	go so.hp.listenForPunch(ctx, conn, sessionID, successCh, errCh)

	// Send simultaneous punches
	ticker := time.NewTicker(50 * time.Millisecond) // Faster interval for simultaneous open
	defer ticker.Stop()

	for result.Attempts < MaxPunchAttempts*2 { // More attempts for simultaneous
		select {
		case <-ctx.Done():
			result.Success = false
			return result, nil

		case addr := <-successCh:
			result.Success = true
			result.RemoteAddr = addr
			result.Method = "simultaneous_open"
			return result, nil

		case <-ticker.C:
			result.Attempts++
			so.hp.sendPunchPacket(conn, targetAddr, HolePunchSyn, sessionID)
		}
	}

	result.Success = false
	return result, nil
}

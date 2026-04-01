// Package nat implements NAT traversal techniques including STUN, UDP hole punching,
// and signaling for P2P connections behind firewalls.
package nat

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// STUN message types (RFC 5389)
const (
	StunBindingRequest  = 0x0001
	StunBindingResponse = 0x0101
	StunBindingError    = 0x0111

	// STUN attributes
	StunAttrMappedAddress    = 0x0001
	StunAttrXorMappedAddress = 0x0020

	// STUN magic cookie (RFC 5389)
	StunMagicCookie = 0x2112A442

	// Default STUN servers
	DefaultStunServer1 = "stun.l.google.com:19302"
	DefaultStunServer2 = "stun1.l.google.com:19302"
)

var (
	ErrStunTimeout     = errors.New("STUN request timed out")
	ErrStunInvalidResp = errors.New("invalid STUN response")
	ErrNoMappedAddress = errors.New("no mapped address in response")
)

// MappedAddress represents a public IP:port as seen by the STUN server.
type MappedAddress struct {
	IP   net.IP
	Port int
}

func (m MappedAddress) String() string {
	return fmt.Sprintf("%s:%d", m.IP, m.Port)
}

// StunClient discovers public IP/port using the STUN protocol.
type StunClient struct {
	servers []string
	timeout time.Duration
}

// NewStunClient creates a STUN client with default servers.
func NewStunClient() *StunClient {
	return &StunClient{
		servers: []string{DefaultStunServer1, DefaultStunServer2},
		timeout: 3 * time.Second,
	}
}

// NewStunClientWithServers creates a STUN client with custom servers.
func NewStunClientWithServers(servers []string, timeout time.Duration) *StunClient {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &StunClient{
		servers: servers,
		timeout: timeout,
	}
}

// Discover finds the public IP and port by querying STUN servers.
// Uses the provided UDP connection to maintain port consistency.
func (c *StunClient) Discover(ctx context.Context, conn *net.UDPConn) (*MappedAddress, error) {
	var lastErr error

	for _, server := range c.servers {
		addr, err := c.queryServer(ctx, conn, server)
		if err == nil {
			return addr, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all STUN servers failed: %w", lastErr)
	}
	return nil, errors.New("no STUN servers configured")
}

// DiscoverWithNewConn creates a new UDP connection and discovers the public address.
func (c *StunClient) DiscoverWithNewConn(ctx context.Context) (*MappedAddress, *net.UDPConn, error) {
	// Bind to any available port
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, nil, fmt.Errorf("creating UDP socket: %w", err)
	}

	addr, err := c.Discover(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}

	return addr, conn, nil
}

// queryServer sends a STUN binding request and parses the response.
func (c *StunClient) queryServer(ctx context.Context, conn *net.UDPConn, server string) (*MappedAddress, error) {
	serverAddr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, fmt.Errorf("resolving STUN server %s: %w", server, err)
	}

	// Build STUN binding request
	transactionID := make([]byte, 12)
	if _, err := rand.Read(transactionID); err != nil {
		return nil, fmt.Errorf("generating transaction ID: %w", err)
	}

	request := buildStunRequest(transactionID)

	// Set deadline based on context or timeout
	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("setting deadline: %w", err)
	}

	// Send request
	if _, err := conn.WriteToUDP(request, serverAddr); err != nil {
		return nil, fmt.Errorf("sending STUN request: %w", err)
	}

	// Read response
	buf := make([]byte, 576) // Max STUN message size
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return nil, ErrStunTimeout
		}
		return nil, fmt.Errorf("reading STUN response: %w", err)
	}

	// Reset deadline
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("resetting deadline: %w", err)
	}

	// Parse response
	return parseStunResponse(buf[:n], transactionID)
}

// buildStunRequest creates a STUN binding request message.
func buildStunRequest(transactionID []byte) []byte {
	msg := make([]byte, 20)

	// Message Type: Binding Request (0x0001)
	binary.BigEndian.PutUint16(msg[0:2], StunBindingRequest)

	// Message Length: 0 (no attributes)
	binary.BigEndian.PutUint16(msg[2:4], 0)

	// Magic Cookie
	binary.BigEndian.PutUint32(msg[4:8], StunMagicCookie)

	// Transaction ID (12 bytes)
	copy(msg[8:20], transactionID)

	return msg
}

// parseStunResponse extracts the mapped address from a STUN response.
func parseStunResponse(data []byte, expectedTxID []byte) (*MappedAddress, error) {
	if len(data) < 20 {
		return nil, ErrStunInvalidResp
	}

	// Verify message type is Binding Response
	msgType := binary.BigEndian.Uint16(data[0:2])
	if msgType != StunBindingResponse {
		if msgType == StunBindingError {
			return nil, errors.New("STUN binding error response")
		}
		return nil, fmt.Errorf("unexpected STUN message type: 0x%04x", msgType)
	}

	// Verify magic cookie
	cookie := binary.BigEndian.Uint32(data[4:8])
	if cookie != StunMagicCookie {
		return nil, fmt.Errorf("invalid magic cookie: 0x%08x", cookie)
	}

	// Verify transaction ID
	if len(expectedTxID) == 12 {
		for i := 0; i < 12; i++ {
			if data[8+i] != expectedTxID[i] {
				return nil, errors.New("transaction ID mismatch")
			}
		}
	}

	// Parse message length
	msgLen := binary.BigEndian.Uint16(data[2:4])
	if len(data) < 20+int(msgLen) {
		return nil, ErrStunInvalidResp
	}

	// Parse attributes
	pos := 20
	end := 20 + int(msgLen)

	for pos < end {
		if pos+4 > end {
			break
		}

		attrType := binary.BigEndian.Uint16(data[pos : pos+2])
		attrLen := binary.BigEndian.Uint16(data[pos+2 : pos+4])
		pos += 4

		if pos+int(attrLen) > end {
			break
		}

		attrData := data[pos : pos+int(attrLen)]

		switch attrType {
		case StunAttrXorMappedAddress:
			return parseXorMappedAddress(attrData, data[4:8])
		case StunAttrMappedAddress:
			return parseMappedAddress(attrData)
		}

		// Align to 4-byte boundary
		pos += int(attrLen)
		if attrLen%4 != 0 {
			pos += 4 - int(attrLen%4)
		}
	}

	return nil, ErrNoMappedAddress
}

// parseXorMappedAddress decodes XOR-MAPPED-ADDRESS attribute (RFC 5389).
func parseXorMappedAddress(data []byte, magicCookie []byte) (*MappedAddress, error) {
	if len(data) < 8 {
		return nil, errors.New("XOR-MAPPED-ADDRESS too short")
	}

	family := data[1]
	xorPort := binary.BigEndian.Uint16(data[2:4])

	// XOR with magic cookie's first 2 bytes
	port := xorPort ^ uint16(binary.BigEndian.Uint32(magicCookie)>>16)

	var ip net.IP

	if family == 0x01 { // IPv4
		if len(data) < 8 {
			return nil, errors.New("IPv4 XOR-MAPPED-ADDRESS too short")
		}
		xorIP := binary.BigEndian.Uint32(data[4:8])
		realIP := xorIP ^ StunMagicCookie
		ip = make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, realIP)
	} else if family == 0x02 { // IPv6
		if len(data) < 20 {
			return nil, errors.New("IPv6 XOR-MAPPED-ADDRESS too short")
		}
		// XOR with magic cookie + transaction ID
		// For simplicity, we only support IPv4 for now
		return nil, errors.New("IPv6 not supported")
	} else {
		return nil, fmt.Errorf("unknown address family: %d", family)
	}

	return &MappedAddress{IP: ip, Port: int(port)}, nil
}

// parseMappedAddress decodes MAPPED-ADDRESS attribute (legacy).
func parseMappedAddress(data []byte) (*MappedAddress, error) {
	if len(data) < 8 {
		return nil, errors.New("MAPPED-ADDRESS too short")
	}

	family := data[1]
	port := binary.BigEndian.Uint16(data[2:4])

	var ip net.IP

	if family == 0x01 { // IPv4
		ip = net.IP(data[4:8])
	} else if family == 0x02 { // IPv6
		if len(data) < 20 {
			return nil, errors.New("IPv6 MAPPED-ADDRESS too short")
		}
		ip = net.IP(data[4:20])
	} else {
		return nil, fmt.Errorf("unknown address family: %d", family)
	}

	return &MappedAddress{IP: ip, Port: int(port)}, nil
}

// DetectNATType attempts to classify the NAT type.
// Returns one of: "Full Cone", "Restricted Cone", "Port Restricted Cone", "Symmetric", "Unknown"
func (c *StunClient) DetectNATType(ctx context.Context) (string, error) {
	// Query two different servers to see if we get the same mapped address
	if len(c.servers) < 2 {
		return "Unknown", errors.New("need at least 2 STUN servers for NAT detection")
	}

	// Create connection and query first server
	addr1, conn1, err := c.DiscoverWithNewConn(ctx)
	if err != nil {
		return "Unknown", fmt.Errorf("first STUN query failed: %w", err)
	}
	defer conn1.Close()

	// Query second server with SAME local port
	addr2, err := c.queryServer(ctx, conn1, c.servers[1])
	if err != nil {
		return "Unknown", fmt.Errorf("second STUN query failed: %w", err)
	}

	// Compare mapped addresses
	if addr1.IP.Equal(addr2.IP) && addr1.Port == addr2.Port {
		// Same mapping for different destinations = Cone NAT (Full, Restricted, or Port-Restricted)
		// To distinguish, we'd need a STUN server that supports CHANGE-REQUEST
		return "Cone NAT", nil
	}

	// Different mapping for different destinations = Symmetric NAT
	// UDP hole punching often fails with symmetric NAT
	return "Symmetric NAT", nil
}

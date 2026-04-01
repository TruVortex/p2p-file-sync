package nat

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func TestStunRequest(t *testing.T) {
	// Test building a STUN request
	txID := make([]byte, 12)
	for i := range txID {
		txID[i] = byte(i)
	}

	request := buildStunRequest(txID)

	if len(request) != 20 {
		t.Errorf("expected request length 20, got %d", len(request))
	}

	// Verify message type
	msgType := uint16(request[0])<<8 | uint16(request[1])
	if msgType != StunBindingRequest {
		t.Errorf("expected message type 0x0001, got 0x%04x", msgType)
	}

	// Verify magic cookie
	cookie := uint32(request[4])<<24 | uint32(request[5])<<16 | uint32(request[6])<<8 | uint32(request[7])
	if cookie != StunMagicCookie {
		t.Errorf("expected magic cookie 0x%08x, got 0x%08x", StunMagicCookie, cookie)
	}

	// Verify transaction ID
	for i := 0; i < 12; i++ {
		if request[8+i] != txID[i] {
			t.Errorf("transaction ID mismatch at byte %d", i)
		}
	}
}

func TestStunResponseParsing(t *testing.T) {
	// Build a mock STUN response with XOR-MAPPED-ADDRESS
	txID := make([]byte, 12)
	for i := range txID {
		txID[i] = byte(i)
	}

	// Response with XOR-MAPPED-ADDRESS for 192.168.1.100:12345
	response := make([]byte, 32)

	// Message type: Binding Response
	response[0] = 0x01
	response[1] = 0x01

	// Message length: 12 bytes (for XOR-MAPPED-ADDRESS attribute)
	response[2] = 0x00
	response[3] = 0x0C

	// Magic cookie
	response[4] = 0x21
	response[5] = 0x12
	response[6] = 0xA4
	response[7] = 0x42

	// Transaction ID
	copy(response[8:20], txID)

	// XOR-MAPPED-ADDRESS attribute
	response[20] = 0x00 // Attribute type: XOR-MAPPED-ADDRESS (0x0020)
	response[21] = 0x20
	response[22] = 0x00 // Attribute length: 8
	response[23] = 0x08
	response[24] = 0x00 // Reserved
	response[25] = 0x01 // IPv4
	// Port XOR'd with magic cookie high bits: 12345 ^ (0x2112) = 0x301F ^ 0x2112 = 0x110D
	// 12345 = 0x3039, 0x3039 ^ 0x2112 = 0x112B
	response[26] = 0x11
	response[27] = 0x2B
	// IP XOR'd with magic cookie: 192.168.1.100 = 0xC0A80164, XOR 0x2112A442 = 0xE1BAA526
	response[28] = 0xE1
	response[29] = 0xBA
	response[30] = 0xA5
	response[31] = 0x26

	mapped, err := parseStunResponse(response, txID)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	expectedIP := net.IPv4(192, 168, 1, 100)
	if !mapped.IP.Equal(expectedIP) {
		t.Errorf("expected IP %v, got %v", expectedIP, mapped.IP)
	}

	if mapped.Port != 12345 {
		t.Errorf("expected port 12345, got %d", mapped.Port)
	}
}

func TestStunInvalidResponse(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"too short", make([]byte, 10)},
		{"wrong type", func() []byte {
			d := make([]byte, 20)
			d[0] = 0x00
			d[1] = 0x00
			d[4] = 0x21
			d[5] = 0x12
			d[6] = 0xA4
			d[7] = 0x42
			return d
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseStunResponse(tt.data, nil)
			if err == nil {
				t.Error("expected error for invalid response")
			}
		})
	}
}

func TestMappedAddressString(t *testing.T) {
	addr := MappedAddress{
		IP:   net.IPv4(1, 2, 3, 4),
		Port: 5678,
	}

	expected := "1.2.3.4:5678"
	if addr.String() != expected {
		t.Errorf("expected %s, got %s", expected, addr.String())
	}
}

func TestSignalingServer(t *testing.T) {
	server := NewSignalingServer()
	if err := server.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Close()

	addr := server.Addr().String()
	client := NewSignalingClient(addr)
	ctx := context.Background()

	// Register a peer
	err := client.Register(ctx, "peer-1", "1.2.3.4:5000", "192.168.1.10:5000")
	if err != nil {
		t.Fatalf("registration failed: %v", err)
	}

	// Wait for registration to be processed
	time.Sleep(50 * time.Millisecond)

	if server.RegisteredPeers() != 1 {
		t.Errorf("expected 1 registered peer, got %d", server.RegisteredPeers())
	}

	// Lookup the peer
	info, err := client.LookupPeer(ctx, "peer-1")
	if err != nil {
		t.Fatalf("lookup failed: %v", err)
	}

	if info.PublicAddr != "1.2.3.4:5000" {
		t.Errorf("expected public addr 1.2.3.4:5000, got %s", info.PublicAddr)
	}

	if info.LocalAddr != "192.168.1.10:5000" {
		t.Errorf("expected local addr 192.168.1.10:5000, got %s", info.LocalAddr)
	}

	// Lookup non-existent peer
	_, err = client.LookupPeer(ctx, "peer-nonexistent")
	if err != ErrPeerNotFound {
		t.Errorf("expected ErrPeerNotFound, got %v", err)
	}
}

func TestSignalingMultiplePeers(t *testing.T) {
	server := NewSignalingServer()
	if err := server.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Close()

	addr := server.Addr().String()
	ctx := context.Background()

	// Register multiple peers
	peers := []struct {
		id     string
		public string
		local  string
	}{
		{"alice", "1.1.1.1:1000", "10.0.0.1:1000"},
		{"bob", "2.2.2.2:2000", "10.0.0.2:2000"},
		{"charlie", "3.3.3.3:3000", "10.0.0.3:3000"},
	}

	for _, peer := range peers {
		client := NewSignalingClient(addr)
		if err := client.Register(ctx, peer.id, peer.public, peer.local); err != nil {
			t.Fatalf("registration failed for %s: %v", peer.id, err)
		}
	}

	time.Sleep(50 * time.Millisecond)

	if server.RegisteredPeers() != 3 {
		t.Errorf("expected 3 registered peers, got %d", server.RegisteredPeers())
	}

	// Lookup each peer
	client := NewSignalingClient(addr)
	for _, peer := range peers {
		info, err := client.LookupPeer(ctx, peer.id)
		if err != nil {
			t.Errorf("lookup failed for %s: %v", peer.id, err)
			continue
		}
		if info.PublicAddr != peer.public {
			t.Errorf("peer %s: expected public %s, got %s", peer.id, peer.public, info.PublicAddr)
		}
	}
}

func TestHolePunchPacket(t *testing.T) {
	hp := &HolePuncher{}

	// Create a UDP connection for testing
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("failed to create UDP socket: %v", err)
	}
	defer conn.Close()

	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
	sessionID := []byte{1, 2, 3, 4, 5, 6, 7, 8}

	// This won't actually reach anything, just test packet construction
	hp.sendPunchPacket(conn, target, HolePunchSyn, sessionID)
	hp.sendPunchPacket(conn, target, HolePunchSynAck, sessionID)
	hp.sendPunchPacket(conn, target, HolePunchAck, sessionID)
}

func TestRelayServer(t *testing.T) {
	server := NewRelayServer()
	if err := server.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("failed to start relay server: %v", err)
	}
	defer server.Close()

	addr := server.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sessionID := "test-session-123"

	// Connect two clients with the same session ID
	var conn1, conn2 net.Conn
	var err1, err2 error
	done := make(chan struct{})

	go func() {
		client1 := NewRelayClient(addr)
		conn1, err1 = client1.Connect(ctx, sessionID)
		done <- struct{}{}
	}()

	go func() {
		time.Sleep(100 * time.Millisecond) // Slight delay to ensure first is waiting
		client2 := NewRelayClient(addr)
		conn2, err2 = client2.Connect(ctx, sessionID)
		done <- struct{}{}
	}()

	<-done
	<-done

	if err1 != nil {
		t.Fatalf("client1 connect failed: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("client2 connect failed: %v", err2)
	}

	defer conn1.Close()
	defer conn2.Close()

	// Test bidirectional communication
	testData := []byte("Hello from peer 1!")

	go func() {
		conn1.Write(testData)
	}()

	buf := make([]byte, 100)
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn2.Read(buf)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if !bytes.Equal(buf[:n], testData) {
		t.Errorf("expected %q, got %q", testData, buf[:n])
	}

	// Test reverse direction
	testData2 := []byte("Hello back from peer 2!")
	go func() {
		conn2.Write(testData2)
	}()

	conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = conn1.Read(buf)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if !bytes.Equal(buf[:n], testData2) {
		t.Errorf("expected %q, got %q", testData2, buf[:n])
	}
}

func TestConnectionResult(t *testing.T) {
	// Test with UDP connection
	udpConn, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	defer udpConn.Close()

	result := &ConnectionResult{
		Method:  "hole_punch",
		UDPConn: udpConn,
	}

	if result.GetConn() == nil {
		t.Error("expected non-nil connection")
	}

	// Test with relay connection (using a pipe as mock)
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	result2 := &ConnectionResult{
		Method:    "relay",
		RelayConn: client,
	}

	if result2.GetConn() == nil {
		t.Error("expected non-nil relay connection")
	}
}

func TestStunClientCreation(t *testing.T) {
	// Default client
	client := NewStunClient()
	if len(client.servers) != 2 {
		t.Errorf("expected 2 default servers, got %d", len(client.servers))
	}

	// Custom client
	customServers := []string{"stun.example.com:3478"}
	customClient := NewStunClientWithServers(customServers, 5*time.Second)
	if len(customClient.servers) != 1 {
		t.Errorf("expected 1 custom server, got %d", len(customClient.servers))
	}
	if customClient.timeout != 5*time.Second {
		t.Errorf("expected 5s timeout, got %v", customClient.timeout)
	}
}

func TestHolePunchResultMethods(t *testing.T) {
	result := &HolePunchResult{
		Success:  false,
		Attempts: 5,
		Method:   "hole_punch",
	}

	if result.Success {
		t.Error("expected failure")
	}

	if result.Attempts != 5 {
		t.Errorf("expected 5 attempts, got %d", result.Attempts)
	}
}

func TestPeerInfoStructure(t *testing.T) {
	info := &PeerInfo{
		ID:         "test-peer",
		PublicAddr: "1.2.3.4:5000",
		LocalAddr:  "192.168.1.10:5000",
		LastSeen:   time.Now(),
	}

	if info.ID != "test-peer" {
		t.Errorf("expected ID 'test-peer', got %s", info.ID)
	}
}

func TestRelayClientClose(t *testing.T) {
	client := NewRelayClient("127.0.0.1:9999")

	// Close should not panic even without connection
	if err := client.Close(); err != nil {
		t.Errorf("close failed: %v", err)
	}

	if client.GetConn() != nil {
		t.Error("expected nil connection after close")
	}
}

func TestFallbackStrategy(t *testing.T) {
	// Just test creation - actual connection requires network setup
	strategy := NewFallbackStrategy("signal.example.com:8000", "relay.example.com:8001", "test-peer")
	defer strategy.Close()

	if strategy.holePuncher == nil {
		t.Error("expected non-nil hole puncher")
	}

	if strategy.relayAddr != "relay.example.com:8001" {
		t.Errorf("expected relay addr 'relay.example.com:8001', got %s", strategy.relayAddr)
	}
}

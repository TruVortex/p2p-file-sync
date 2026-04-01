package net

import (
	"context"
	"testing"
	"time"
)

func TestTransport_GenerateCert(t *testing.T) {
	cert, err := generateSelfSignedCert("test-peer")
	if err != nil {
		t.Fatalf("generating cert: %v", err)
	}

	if cert == nil {
		t.Fatal("expected certificate")
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("expected certificate data")
	}
	if cert.PrivateKey == nil {
		t.Fatal("expected private key")
	}
}

func TestTransport_CertFingerprint(t *testing.T) {
	cert1, _ := generateSelfSignedCert("peer1")
	cert2, _ := generateSelfSignedCert("peer2")

	fp1 := CertFingerprint(cert1)
	fp2 := CertFingerprint(cert2)

	if fp1 == fp2 {
		t.Error("different certs should have different fingerprints")
	}

	// Same cert should have same fingerprint
	fp1Again := CertFingerprint(cert1)
	if fp1 != fp1Again {
		t.Error("same cert should have same fingerprint")
	}
}

func TestPeerID_String(t *testing.T) {
	var id PeerID
	for i := range id {
		id[i] = byte(i)
	}

	fullStr := id.String()
	if len(fullStr) != 64 { // 32 bytes * 2 hex chars
		t.Errorf("expected 64 char string, got %d", len(fullStr))
	}

	shortStr := id.Short()
	if len(shortStr) != 16 { // 8 bytes * 2 hex chars
		t.Errorf("expected 16 char short string, got %d", len(shortStr))
	}
}

func TestTransport_CreateAndClose(t *testing.T) {
	cfg := &Config{
		ListenAddr: "127.0.0.1:0",
		PeerName:   "test-node",
	}

	transport, err := NewTransport(cfg)
	if err != nil {
		t.Fatalf("creating transport: %v", err)
	}

	if transport.LocalID() == (PeerID{}) {
		t.Error("expected non-zero local ID")
	}

	if err := transport.Close(); err != nil {
		t.Fatalf("closing transport: %v", err)
	}
}

func TestTransport_ListenAndConnect(t *testing.T) {
	// Create server transport
	serverCfg := &Config{
		ListenAddr: "127.0.0.1:0",
		PeerName:   "server",
		MerkleRoot: make([]byte, 32),
	}
	server, err := NewTransport(serverCfg)
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	defer server.Close()

	if err := server.Listen(); err != nil {
		t.Fatalf("server listen: %v", err)
	}

	// Get actual listen address
	addr := server.listener.Addr().String()

	// Create client transport
	clientCfg := &Config{
		ListenAddr: "127.0.0.1:0",
		PeerName:   "client",
		MerkleRoot: make([]byte, 32),
	}
	client, err := NewTransport(clientCfg)
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	// Connect
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peer, err := client.Connect(ctx, addr)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}

	if peer == nil {
		t.Fatal("expected peer")
	}
	if peer.Name != "server" {
		t.Errorf("expected peer name 'server', got '%s'", peer.Name)
	}
	if peer.ID == client.LocalID() {
		t.Error("peer ID should differ from client ID")
	}

	// Verify peer is registered
	time.Sleep(100 * time.Millisecond)
	if len(client.Peers()) != 1 {
		t.Errorf("expected 1 peer, got %d", len(client.Peers()))
	}
}

func TestTransport_PeerCallbacks(t *testing.T) {
	connectedCh := make(chan *Peer, 1)
	disconnectedCh := make(chan *Peer, 1)

	serverCfg := &Config{
		ListenAddr: "127.0.0.1:0",
		PeerName:   "server",
		MerkleRoot: make([]byte, 32),
		OnPeerConnected: func(p *Peer) {
			connectedCh <- p
		},
		OnPeerDisconnected: func(p *Peer) {
			disconnectedCh <- p
		},
	}
	server, err := NewTransport(serverCfg)
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	defer server.Close()

	if err := server.Listen(); err != nil {
		t.Fatalf("server listen: %v", err)
	}

	addr := server.listener.Addr().String()

	clientCfg := &Config{
		ListenAddr: "127.0.0.1:0",
		PeerName:   "client",
		MerkleRoot: make([]byte, 32),
	}
	client, err := NewTransport(clientCfg)
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = client.Connect(ctx, addr)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}

	// Wait for connected callback
	select {
	case p := <-connectedCh:
		if p.Name != "client" {
			t.Errorf("expected peer name 'client', got '%s'", p.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for connected callback")
	}

	// Close client to trigger disconnect
	client.Close()

	// Wait for disconnected callback
	select {
	case <-disconnectedCh:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for disconnected callback")
	}
}

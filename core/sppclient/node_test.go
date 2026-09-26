package sppclient

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestDialNetworkForProto(t *testing.T) {
	cases := []struct {
		proto string
		want  string
	}{
		{"", "tcp"},
		{"tcp", "tcp"},
		{"TCP", "tcp"},
		{"udp", "udp"},
		{"rudp", "udp"},
		{"RUDP", "udp"},
		{"kcp", "udp"},
		{"quic", "udp"},
		{"  quic  ", "udp"},
	}
	for _, tc := range cases {
		if got := dialNetworkForProto(tc.proto); got != tc.want {
			t.Errorf("dialNetworkForProto(%q)=%q, want %q", tc.proto, got, tc.want)
		}
	}
}

func TestNodeJSONSerialization(t *testing.T) {
	node := &Node{
		Name:        "HongKong-01",
		Server:      "1.2.3.4:8888",
		ServerProto: "kcp",
		Key:         "pass123",
		Encrypt:     "default",
		Compress:    128,
		Latency:     45 * time.Millisecond,
		Alive:       true,
	}

	data, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("failed to marshal node: %v", err)
	}

	var parsed Node
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal node: %v", err)
	}

	if parsed.Name != node.Name || parsed.Server != node.Server || parsed.ServerProto != node.ServerProto {
		t.Errorf("mismatch parsed node: %+v", parsed)
	}
	if parsed.Compress != node.Compress || parsed.Key != node.Key {
		t.Errorf("mismatch properties: %+v", parsed)
	}
}

func TestManagerHealthCheckUsesProtoNetwork(t *testing.T) {
	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpL.Close()
	go func() {
		for {
			c, err := tcpL.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	udpAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpL, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer udpL.Close()

	nodes := []*Node{
		{Name: "tcp-node", Server: tcpL.Addr().String(), ServerProto: "tcp"},
		{Name: "rudp-node", Server: udpL.LocalAddr().String(), ServerProto: "rudp"},
	}
	m := &Manager{
		nodes:       nodes,
		localSocks5: "127.0.0.1:0",
		checkStopCh: make(chan struct{}),
		activeIndex: 0,
	}
	m.checkAllNodes()
	if !nodes[0].Alive {
		t.Fatal("tcp node should be alive after TCP health check")
	}
	if !nodes[1].Alive {
		t.Fatal("rudp node should be alive after UDP health check")
	}
	if nodes[0].Latency <= 0 || nodes[1].Latency <= 0 {
		t.Fatalf("expected positive latency, got %v %v", nodes[0].Latency, nodes[1].Latency)
	}
	if got := m.Socks5Addr(); got != "127.0.0.1:0" {
		t.Fatalf("Socks5Addr=%q", got)
	}
	if m.ActiveNode().Name != "tcp-node" {
		t.Fatalf("ActiveNode=%s", m.ActiveNode().Name)
	}
	if len(m.GetAllNodes()) != 2 {
		t.Fatal("GetAllNodes size")
	}
	m.Close()
}

func TestNewClientRejectsEmptyServer(t *testing.T) {
	_, err := NewClient(&Config{})
	if err == nil {
		t.Fatal("expected error for empty server")
	}
}

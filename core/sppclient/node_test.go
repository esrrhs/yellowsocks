package sppclient

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestNodeJSONSerialization(t *testing.T) {
	node := &Node{
		Name:        "HongKong-01",
		Server:      "1.2.3.4:8888",
		ServerProto: "tcp",
		Key:         "secret",
		Encrypt:     "",
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
	if parsed.Key != node.Key || parsed.Compress != node.Compress {
		t.Errorf("mismatch spp fields: %+v", parsed)
	}
}

func TestDialNetworkForProto(t *testing.T) {
	if dialNetworkForProto("tcp") != "tcp" || dialNetworkForProto("") != "tcp" {
		t.Fatal("tcp proto should dial tcp")
	}
	for _, proto := range []string{"udp", "kcp", "quic", "rudp"} {
		if dialNetworkForProto(proto) != "udp" {
			t.Fatalf("%s should dial udp", proto)
		}
	}
}

func TestManagerHealthCheckDialsTCP(t *testing.T) {
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

	nodes := []*Node{
		{Name: "spp-a", Server: tcpL.Addr().String(), ServerProto: "tcp"},
		{Name: "spp-b", Server: "127.0.0.1:1", ServerProto: "tcp"},
	}
	m := &Manager{
		nodes:       nodes,
		localSocks5: "127.0.0.1:10808",
		checkStopCh: make(chan struct{}),
		activeIndex: 0,
	}
	m.checkAllNodes()
	if !nodes[0].Alive {
		t.Fatal("spp node should be alive after TCP health check")
	}
	if nodes[1].Alive {
		t.Fatal("closed port should not be alive")
	}
	if nodes[0].Latency <= 0 {
		t.Fatalf("expected positive latency, got %v", nodes[0].Latency)
	}
	if got := m.Socks5Addr(); got != "127.0.0.1:10808" {
		t.Fatalf("Socks5Addr=%q", got)
	}
	user, pass := m.Socks5Auth()
	if user != "" || pass != "" {
		t.Fatalf("local spp socks5 auth = %q %q", user, pass)
	}
	if len(m.GetAllNodes()) != 2 {
		t.Fatal("GetAllNodes size")
	}
	m.Close()
}

package sppclient

import (
	"encoding/json"
	"testing"
)

func TestNodeJSONSerialization(t *testing.T) {
	node := &Node{
		Name:          "HongKong-01",
		Server:        "1.2.3.4:8888",
		ServerProto:   "rdns",
		Key:           "secret",
		Encrypt:       "",
		Compress:      128,
		Alive:         true,
		RdnsDomain:    "tunnel.example.com",
		RdnsUpstream:  "8.8.8.8:53",
		RdnsPsk:       "psk-secret",
		RdnsQueryType: "NULL",
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
	if parsed.RdnsDomain != node.RdnsDomain || parsed.RdnsUpstream != node.RdnsUpstream || parsed.RdnsPsk != node.RdnsPsk || parsed.RdnsQueryType != node.RdnsQueryType {
		t.Errorf("mismatch rdns fields: %+v", parsed)
	}
}

func TestDialNetworkForProto(t *testing.T) {
	for _, proto := range []string{"tcp", "rhttp", ""} {
		if dialNetworkForProto(proto) != "tcp" {
			t.Fatalf("%s should dial tcp", proto)
		}
	}
	for _, proto := range []string{"udp", "kcp", "quic", "rudp", "rdns"} {
		if dialNetworkForProto(proto) != "udp" {
			t.Fatalf("%s should dial udp", proto)
		}
	}
}

func TestProtoRobustnessOrdering(t *testing.T) {
	if protoRobustnessBase("rudp") <= protoRobustnessBase("ricmp") {
		t.Fatal("rudp should rank above ricmp")
	}
	if protoRobustnessBase("ricmp") <= protoRobustnessBase("kcp") {
		t.Fatal("ricmp should rank above kcp")
	}
	if protoRobustnessBase("kcp") <= protoRobustnessBase("pingtunnel") {
		t.Fatal("kcp should rank above pingtunnel")
	}
	if protoRobustnessBase("pingtunnel") <= protoRobustnessBase("tcp") {
		t.Fatal("pingtunnel should rank above tcp")
	}
	if protoRobustnessBase("tcp") <= protoRobustnessBase("rhttp") {
		t.Fatal("tcp should rank above rhttp")
	}
	if protoRobustnessBase("rhttp") <= protoRobustnessBase("rdns") {
		t.Fatal("rhttp should rank above rdns")
	}
	if protoRobustnessBase("rdns") <= protoRobustnessBase("quic") {
		t.Fatal("rdns should rank above quic")
	}
}

// A socks5 node dials its own server with its own credentials and needs no
// local client; the pick must hand the real dial address and auth to the
// proxy caller even while the node is unreachable (TEST-NET).
func TestManagerPicksSocks5NodeWithCreds(t *testing.T) {
	m, err := NewManager([]*Node{{
		Name:     "socks5-backup",
		Type:     "socks5",
		Server:   "203.0.113.10",
		Port:     1080,
		Username: "esrrhs",
		Password: "secret",
		Priority: 1,
	}}, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	addr, user, pass, name, release := m.PickNode()
	if addr != "203.0.113.10:1080" {
		t.Fatalf("PickNode addr = %q, want dial address 203.0.113.10:1080", addr)
	}
	if user != "esrrhs" || pass != "secret" {
		t.Fatalf("PickNode creds = %q/%q, want node credentials", user, pass)
	}
	if name != "socks5-backup" {
		t.Fatalf("PickNode name = %q", name)
	}
	if release == nil {
		t.Fatal("PickNode release is nil")
	}
	release()
}

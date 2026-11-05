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
	if protoRobustnessBase("ricmp") <= protoRobustnessBase("kcp") {
		t.Fatal("ricmp should rank above kcp")
	}
	if protoRobustnessBase("kcp") <= protoRobustnessBase("rudp") {
		t.Fatal("kcp should rank above rudp")
	}
	if protoRobustnessBase("rudp") <= protoRobustnessBase("pingtunnel") {
		t.Fatal("rudp should rank above pingtunnel")
	}
	if protoRobustnessBase("pingtunnel") <= protoRobustnessBase("tcp") {
		t.Fatal("pingtunnel should rank above tcp")
	}
	if protoRobustnessBase("tcp") <= protoRobustnessBase("rdns") {
		t.Fatal("tcp should rank above rdns")
	}
	if protoRobustnessBase("rdns") <= protoRobustnessBase("quic") {
		t.Fatal("rdns should rank above quic")
	}
	if protoRobustnessBase("quic") <= protoRobustnessBase("rhttp") {
		t.Fatal("quic should rank above rhttp")
	}
}

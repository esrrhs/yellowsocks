package sppclient

import (
	"encoding/json"
	"testing"
)

func TestNodeJSONSerialization(t *testing.T) {
	node := &Node{
		Name:        "HongKong-01",
		Server:      "1.2.3.4:8888",
		ServerProto: "tcp",
		Key:         "secret",
		Encrypt:     "",
		Compress:    128,
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

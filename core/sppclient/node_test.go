package sppclient

import (
	"encoding/json"
	"testing"
	"time"
)

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

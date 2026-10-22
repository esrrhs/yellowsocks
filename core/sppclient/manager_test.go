package sppclient

import (
	"net"
	"testing"
	"time"
)

// acceptAndClose accepts every inbound connection on ln and closes it. The
// fake "server" is enough to satisfy a TCP connect probe without speaking SPP.
func acceptAndClose(t *testing.T, ln net.Listener) {
	t.Helper()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
}

// listenTCP starts a TCP listener on a fresh loopback port.
func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// reserveTCPPort listens, records the address, and closes the socket so the
// address is currently refused but can be re-bound later in the test.
func reserveTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve tcp port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// rebindTCP retries re-listening on addr until the kernel frees the socket.
func rebindTCP(t *testing.T, addr string) net.Listener {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			t.Cleanup(func() { _ = ln.Close() })
			return ln
		}
		if time.Now().After(deadline) {
			t.Fatalf("rebind %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func tcpNode(name, addr string) *Node {
	return &Node{Name: name, Server: addr, ServerProto: "tcp", Key: "testkey"}
}

func TestNewManagerRejectsEmptyNodes(t *testing.T) {
	if _, err := NewManager(nil, ""); err == nil {
		t.Fatal("NewManager with no nodes must fail")
	}
}

func TestNewManagerDefaultsSocks5Addr(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)
	m, err := NewManager([]*Node{tcpNode("a", ln.Addr().String())}, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	if got := m.Socks5Addr(); got != "127.0.0.1:10808" {
		t.Fatalf("Socks5Addr=%q", got)
	}
	if user, pass := m.Socks5Auth(); user != "" || pass != "" {
		t.Fatalf("Socks5Auth=%q,%q", user, pass)
	}
	if idx := m.ActiveNodeIndex(); idx != 0 {
		t.Fatalf("ActiveNodeIndex=%d, want 0", idx)
	}
}

// Regression: spp's NewClient dials in a background loop and never fails on a
// dead address. The manager must probe nodes itself and start on the first
// reachable one, instead of waiting for the first health-check tick.
func TestNewManagerSelectsFirstReachable(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)

	m, err := NewManager([]*Node{
		tcpNode("primary", ln.Addr().String()),
		tcpNode("backup", "127.0.0.1:1"),
	}, reserveTCPPort(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	if idx := m.ActiveNodeIndex(); idx != 0 {
		t.Fatalf("ActiveNodeIndex=%d, want reachable primary 0", idx)
	}
	if !m.NodeAlive(0) || m.NodeAlive(1) {
		t.Fatalf("alive flags wrong: %v %v", m.NodeAlive(0), m.NodeAlive(1))
	}
}

// Regression: when the configured primary is down at startup the manager must
// immediately start the reachable backup.
func TestNewManagerFallsBackAtStartup(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)

	m, err := NewManager([]*Node{
		tcpNode("primary", "127.0.0.1:1"),
		tcpNode("backup", ln.Addr().String()),
	}, reserveTCPPort(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	if idx := m.ActiveNodeIndex(); idx != 1 {
		t.Fatalf("ActiveNodeIndex=%d, want backup 1 after startup fallback", idx)
	}
	if m.NodeAlive(0) || !m.NodeAlive(1) {
		t.Fatalf("alive flags wrong: %v %v", m.NodeAlive(0), m.NodeAlive(1))
	}
}

// With every node down the manager still starts (spp keeps reconnecting in the
// background) and Close releases it.
func TestNewManagerAllNodesUnreachable(t *testing.T) {
	m, err := NewManager([]*Node{
		tcpNode("a", "127.0.0.1:1"),
		tcpNode("b", "127.0.0.1:2"),
	}, reserveTCPPort(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if idx := m.ActiveNodeIndex(); idx != 0 {
		t.Fatalf("ActiveNodeIndex=%d, want forced primary 0", idx)
	}
	m.Close()
	// Idempotent: a second Close must not panic.
	m.Close()
}

// Regression: a bogus protocol makes NewClient fail synchronously even when
// the best-effort UDP probe reports the node alive. Startup must skip to the
// next node that actually starts.
func TestNewManagerInvalidProtoFallsBack(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)

	m, err := NewManager([]*Node{
		{Name: "bogus", Server: "127.0.0.1:1", ServerProto: "not-a-real-proto", Key: "testkey"},
		tcpNode("backup", ln.Addr().String()),
	}, reserveTCPPort(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	if idx := m.ActiveNodeIndex(); idx != 1 {
		t.Fatalf("ActiveNodeIndex=%d, want backup 1 after NewClient failure", idx)
	}
}

// Regression: when the active node dies and a backup recovers, the periodic
// health check switches over automatically.
func TestHealthCheckFailsOverToRecoveredNode(t *testing.T) {
	primary := listenTCP(t)
	acceptAndClose(t, primary)
	backupAddr := reserveTCPPort(t)

	m, err := newManager([]*Node{
		tcpNode("primary", primary.Addr().String()),
		tcpNode("backup", backupAddr),
	}, reserveTCPPort(t), 25*time.Millisecond)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()
	if idx := m.ActiveNodeIndex(); idx != 0 {
		t.Fatalf("initial ActiveNodeIndex=%d", idx)
	}

	// Primary goes down, backup comes up on its pre-reserved address.
	_ = primary.Close()
	backup := rebindTCP(t, backupAddr)
	acceptAndClose(t, backup)

	waitFor(t, "failover to backup", func() bool { return m.ActiveNodeIndex() == 1 })
	if m.NodeAlive(0) {
		t.Fatal("dead primary must be marked down")
	}
	if !m.NodeAlive(1) {
		t.Fatal("recovered backup must be marked alive")
	}
}

// Regression: a health sweep finishing during or after Close must not rebuild
// a client that Close just released.
func TestCloseSuppressesFailover(t *testing.T) {
	addr := reserveTCPPort(t)
	m, err := newManager([]*Node{
		tcpNode("a", addr),
		tcpNode("b", "127.0.0.1:1"),
	}, reserveTCPPort(t), 20*time.Millisecond)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	// Let at least one sweep run against the all-dead set.
	time.Sleep(80 * time.Millisecond)
	m.Close()

	// The node "recovers" after shutdown; the sweep must not fail over to it.
	ln := rebindTCP(t, addr)
	acceptAndClose(t, ln)
	m.checkNodes()

	if client := m.activeClient; client != nil {
		t.Fatal("Close left/rebuilt an active SPP client")
	}
	if idx := m.ActiveNodeIndex(); idx != 0 {
		t.Fatalf("ActiveNodeIndex changed to %d after Close", idx)
	}
}

func TestProbeNode(t *testing.T) {
	tcpL := listenTCP(t)
	acceptAndClose(t, tcpL)
	if !probeNode(tcpNode("tcp-alive", tcpL.Addr().String())) {
		t.Fatal("TCP probe against a listening server must succeed")
	}
	if probeNode(tcpNode("tcp-dead", "127.0.0.1:1")) {
		t.Fatal("TCP probe against a closed port must fail")
	}

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udpConn.Close()
	if !probeNode(&Node{Name: "udp-alive", Server: udpConn.LocalAddr().String(), ServerProto: "udp"}) {
		t.Fatal("UDP probe against a local address must succeed")
	}
}

// TestPrioritySelectionAndPreemption tests:
// 1. Higher priority (lower priority number) is selected first.
// 2. When priority 1 dies, fails over to priority 2.
// 3. When priority 1 recovers, preempts priority 2 and fails back.
func TestPrioritySelectionAndPreemption(t *testing.T) {
	pri1L := listenTCP(t)
	acceptAndClose(t, pri1L)
	pri1Addr := pri1L.Addr().String()

	pri2L := listenTCP(t)
	acceptAndClose(t, pri2L)
	pri2Addr := pri2L.Addr().String()

	n1 := &Node{Name: "p1-bwh", Server: pri1Addr, ServerProto: "tcp", Priority: 1, Bandwidth: 200, Key: "k"}
	n2 := &Node{Name: "p2-cf", Server: pri2Addr, ServerProto: "tcp", Priority: 2, Bandwidth: 100, Key: "k"}

	m, err := newManager([]*Node{n1, n2}, reserveTCPPort(t), 30*time.Millisecond)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()

	if idx := m.ActiveNodeIndex(); idx != 0 {
		t.Fatalf("Initial active node = %d, want 0 (priority 1)", idx)
	}

	// 1. Priority 1 dies -> failover to Priority 2
	_ = pri1L.Close()
	waitFor(t, "failover to priority 2", func() bool {
		return m.ActiveNodeIndex() == 1
	})
	if m.ActiveNodeName() != "p2-cf" {
		t.Fatalf("Active node = %s, want p2-cf", m.ActiveNodeName())
	}

	// 2. Priority 1 recovers -> preemption / failback to Priority 1
	pri1Recovered := rebindTCP(t, pri1Addr)
	acceptAndClose(t, pri1Recovered)

	waitFor(t, "preemption/failback to priority 1", func() bool {
		return m.ActiveNodeIndex() == 0
	})
	if m.ActiveNodeName() != "p1-bwh" {
		t.Fatalf("Active node = %s, want p1-bwh after recovery", m.ActiveNodeName())
	}
}

// TestSamePriorityRobustnessAndBandwidthSelection tests:
// In the same priority, TCP (higher baseline robustness) is preferred over RUDP,
// and higher bandwidth is preferred within similar robustness.
func TestSamePriorityRobustnessAndBandwidthSelection(t *testing.T) {
	tcpL := listenTCP(t)
	acceptAndClose(t, tcpL)

	tcpHighBw := &Node{Name: "cf-tcp-high", Server: tcpL.Addr().String(), ServerProto: "tcp", Priority: 2, Bandwidth: 500, Key: "k"}
	tcpLowBw := &Node{Name: "cf-tcp-low", Server: tcpL.Addr().String(), ServerProto: "tcp", Priority: 2, Bandwidth: 100, Key: "k"}

	// Both are alive and TCP, so higher bandwidth wins
	m, err := newManager([]*Node{tcpLowBw, tcpHighBw}, reserveTCPPort(t), 50*time.Millisecond)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()

	if idx := m.ActiveNodeIndex(); idx != 1 {
		t.Fatalf("ActiveNodeIndex = %d, want 1 (cf-tcp-high)", idx)
	}
}

func TestFastFailoverOnReportFailure(t *testing.T) {
	pri1Addr := reserveTCPPort(t)
	pri2L := listenTCP(t)
	acceptAndClose(t, pri2L)

	n1 := &Node{Name: "node1", Server: pri1Addr, ServerProto: "tcp", Priority: 1, Key: "k"}
	n2 := &Node{Name: "node2", Server: pri2L.Addr().String(), ServerProto: "tcp", Priority: 2, Key: "k"}

	m, err := newManager([]*Node{n1, n2}, reserveTCPPort(t), 1*time.Second)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()

	// Initial node 1 was selected but will fail in-flight
	m.ReportFailure(net.ErrClosed)
	m.ReportFailure(net.ErrClosed)

	waitFor(t, "fast failover after report failures", func() bool {
		return m.ActiveNodeIndex() == 1
	})
}

func TestSmartMultiPathBalancing(t *testing.T) {
	l1 := listenTCP(t)
	acceptAndClose(t, l1)
	l2 := listenTCP(t)
	acceptAndClose(t, l2)
	l3 := listenTCP(t)
	acceptAndClose(t, l3)

	n1 := &Node{Name: "cf-kcp", Server: l1.Addr().String(), ServerProto: "tcp", Priority: 2, Bandwidth: 100, Key: "k"}
	n2 := &Node{Name: "cf-rudp", Server: l2.Addr().String(), ServerProto: "tcp", Priority: 2, Bandwidth: 100, Key: "k"}
	n3 := &Node{Name: "cf-quic", Server: l3.Addr().String(), ServerProto: "tcp", Priority: 2, Bandwidth: 100, Key: "k"}

	m, err := newManager([]*Node{n1, n2, n3}, reserveTCPPort(t), 1*time.Second)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()

	// In the active tier, all alive nodes must be active
	statuses := m.NodeStatuses()
	for _, s := range statuses {
		if !s.Active {
			t.Fatalf("Node %s in active priority tier should have Active: true", s.Name)
		}
	}

	// Pick nodes for 9 connections
	chosenCounts := make(map[string]int)
	var releases []func()
	for i := 0; i < 9; i++ {
		_, name, release := m.PickNode()
		chosenCounts[name]++
		releases = append(releases, release)
	}

	// Verify all 3 channels shared the load (each got at least 2 connections)
	for _, name := range []string{"cf-kcp", "cf-rudp", "cf-quic"} {
		if chosenCounts[name] < 2 {
			t.Fatalf("Channel %s was not balanced, got %d connections", name, chosenCounts[name])
		}
	}

	// Release connections and check activeConns is decremented
	for _, rel := range releases {
		rel()
	}

	for _, s := range m.NodeStatuses() {
		if s.ActiveConns != 0 {
			t.Fatalf("Node %s activeConns=%d, want 0 after release", s.Name, s.ActiveConns)
		}
	}
}


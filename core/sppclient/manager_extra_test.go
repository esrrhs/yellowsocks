package sppclient

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeUpstream is a fully controllable upstream implementation used to drive
// internal manager state without starting real network clients.
type fakeUpstream struct {
	mu     sync.Mutex
	closed bool
	recent bool
}

func (f *fakeUpstream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeUpstream) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeUpstream) HasRecentTraffic(window time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recent
}

// bareManager builds a Manager without running newManager, for states the
// constructor cannot produce (no runtimes at all, no active node).
func bareManager(nodes []*Node) *Manager {
	m := &Manager{
		nodes:          nodes,
		localSocks5:    "127.0.0.1:10999",
		checkStopCh:    make(chan struct{}),
		triggerCheckCh: make(chan struct{}, 1),
		activeIndex:    -1,
		healthInterval: defaultHealthCheckInterval,
	}
	for _, n := range nodes {
		m.runtimes = append(m.runtimes, &nodeRuntime{node: n, localSocks5: m.localSocks5})
	}
	return m
}

func TestProtoRobustnessBase(t *testing.T) {
	cases := map[string]int{
		"tcp":              100,
		"":                 100,
		"  TCP  ":          100,
		"kcp":              95,
		"KCP":              95,
		"rudp":             90,
		"quic":             50,
		"rhttp":            40,
		"pingtunnel":       20,
		"icmp":             20,
		"not-a-real-proto": 30,
	}
	for proto, want := range cases {
		if got := protoRobustnessBase(proto); got != want {
			t.Fatalf("protoRobustnessBase(%q)=%d want %d", proto, got, want)
		}
	}
}

func TestRobustnessScoreArithmetic(t *testing.T) {
	rt := &nodeRuntime{node: tcpNode("n", "127.0.0.1:1")} // tcp baseline 100
	if got := rt.robustnessScore(); got != 100 {
		t.Fatalf("fresh tcp score=%d want 100", got)
	}

	// score = base + oks*2 - fails*20 - (rtt-100)/50 when rtt > 100
	rt.consecutiveOks.Store(3)
	rt.consecutiveFails.Store(2)
	rt.rttMs.Store(260)
	if got := rt.robustnessScore(); got != 100+3*2-2*20-3 {
		t.Fatalf("score=%d want %d", got, 100+3*2-2*20-3)
	}

	// rtt at or below 100ms adds no penalty
	kcp := &nodeRuntime{node: &Node{Name: "k", ServerProto: "kcp", Server: "127.0.0.1:1"}}
	kcp.rttMs.Store(100)
	if got := kcp.robustnessScore(); got != 95 {
		t.Fatalf("kcp score=%d want 95", got)
	}
}

func TestIsBetterThan(t *testing.T) {
	newRT := func(proto string, bw int) *nodeRuntime {
		return &nodeRuntime{node: &Node{Name: "n", ServerProto: proto, Server: "127.0.0.1:1", Bandwidth: bw}}
	}

	// A noticeably more robust protocol wins regardless of bandwidth.
	tcp := newRT("tcp", 10)
	quic := newRT("quic", 1000)
	if !tcp.isBetterThan(quic) || quic.isBetterThan(tcp) {
		t.Fatal("tcp must beat quic on robustness")
	}

	// Same robustness tier: higher bandwidth wins.
	tcpLow := newRT("tcp", 100)
	tcpHigh := newRT("tcp", 500)
	if !tcpHigh.isBetterThan(tcpLow) || tcpLow.isBetterThan(tcpHigh) {
		t.Fatal("higher bandwidth must win within the same robustness tier")
	}

	// Same bandwidth: the higher score wins (difference within one tier).
	a := newRT("tcp", 100)
	b := newRT("tcp", 100)
	a.consecutiveOks.Store(3) // +6
	if !a.isBetterThan(b) || b.isBetterThan(a) {
		t.Fatal("higher score must win when bandwidth is equal")
	}

	// Everything equal: lower RTT wins.
	a2 := newRT("tcp", 100)
	b2 := newRT("tcp", 100)
	a2.rttMs.Store(50)
	b2.rttMs.Store(20)
	if !b2.isBetterThan(a2) {
		t.Fatal("lower RTT must win when scores are equal")
	}

	// No RTT information on either side: keep the current candidate.
	a3 := newRT("tcp", 100)
	b3 := newRT("tcp", 100)
	if !a3.isBetterThan(b3) {
		t.Fatal("equal candidates without RTT default to true")
	}
}

func TestNodeAliveOutOfRange(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)
	m, err := NewManager([]*Node{tcpNode("a", ln.Addr().String())}, reserveTCPPort(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	if m.NodeAlive(-1) || m.NodeAlive(99) {
		t.Fatal("out of range indexes must report not alive")
	}
	if !m.NodeAlive(0) {
		t.Fatal("node 0 must be alive")
	}
}

func TestPickNodeWithoutRuntimes(t *testing.T) {
	// A manager with zero runtimes has no active tier; PickNode must fall
	// back to the configured local socks5 address and report no node name.
	m := bareManager([]*Node{tcpNode("a", "127.0.0.1:1")})
	m.runtimes = nil // simulate a manager without any runtime
	defer m.Close()

	addr, name, release := m.PickNode()
	if name != "" || release != nil {
		t.Fatalf("PickNode on empty runtime set = name %q, release!=nil: %t", name, release != nil)
	}
	if addr != m.localSocks5 {
		t.Fatalf("PickNode addr=%q want fallback %q", addr, m.localSocks5)
	}
	if m.ActiveNodeName() != "" {
		t.Fatal("manager without an active node must report an empty name")
	}

	// An empty candidate list yields no choice instead of panicking.
	m.mu.Lock()
	if chosen := m.pickCandidateLocked(nil); chosen != nil {
		m.mu.Unlock()
		t.Fatal("pickCandidateLocked(nil) must return nil")
	}
	m.mu.Unlock()
}

func TestClosedManagerAPIs(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)
	m, err := NewManager([]*Node{tcpNode("a", ln.Addr().String())}, reserveTCPPort(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// A legacy active client handle must be released by Close too.
	fake := &fakeUpstream{}
	m.mu.Lock()
	m.activeClient = fake
	m.mu.Unlock()

	m.Close()

	if !fake.wasClosed() {
		t.Fatal("Close must close the legacy activeClient handle")
	}

	// Every entry point must become inert after Close.
	if _, name, release := m.PickNode(); name != "" || release != nil {
		t.Fatal("PickNode must be a no-op after Close")
	}
	if got := m.Socks5Addr(); got != m.localSocks5 {
		t.Fatalf("Socks5Addr after Close = %q want %q", got, m.localSocks5)
	}
	if err := m.ensureClientRunningLocked(m.runtimes[0]); err == nil {
		t.Fatal("ensureClientRunningLocked must fail after Close")
	}
	if err := m.switchNode(0); err == nil {
		t.Fatal("switchNode must fail after Close")
	}
	m.RecordNodeTraffic("a", 1, 1)
	if m.HasRecentNodeTraffic("a", time.Minute) {
		t.Fatal("HasRecentNodeTraffic must be false after Close")
	}
	m.ReportNodeFailure("a", errors.New("boom"))
}

func TestRecordNodeTrafficVariants(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)
	m, err := NewManager([]*Node{tcpNode("n0", ln.Addr().String())}, reserveTCPPort(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	// A nil manager must be a safe no-op.
	var nilManager *Manager
	nilManager.RecordNodeTraffic("x", 1, 1)
	if nilManager.HasRecentNodeTraffic("x", time.Minute) {
		t.Fatal("nil manager must report no traffic")
	}

	m.RecordNodeTraffic("", 1, 1)
	m.RecordNodeTraffic("unknown", 1, 1)
	if m.HasRecentNodeTraffic("", time.Minute) || m.HasRecentNodeTraffic("unknown", time.Minute) {
		t.Fatal("empty/unknown node names must report no traffic")
	}
	if m.HasRecentNodeTraffic("n0", time.Minute) {
		t.Fatal("no traffic recorded yet")
	}

	// One-way traffic does not count as "有来有回".
	m.RecordNodeTraffic("n0", 100, 0)
	if m.HasRecentNodeTraffic("n0", time.Minute) {
		t.Fatal("one-way traffic must not count as bidirectional")
	}

	// Completing the other direction makes the node traffic-active.
	m.RecordNodeTraffic("n0", 0, 200)
	if !m.HasRecentNodeTraffic("n0", time.Minute) {
		t.Fatal("bidirectional traffic must be reported")
	}
	// A non-positive window falls back to the default health interval.
	if !m.HasRecentNodeTraffic("n0", 0) {
		t.Fatal("non-positive window must use the default interval")
	}
}

func TestNodeRuntimeClientFallbackTraffic(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)
	m, err := NewManager([]*Node{tcpNode("n0", ln.Addr().String())}, reserveTCPPort(t))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	rt := m.runtimes[0]
	if rt.hasRecentTraffic(time.Minute) {
		t.Fatal("no traffic recorded yet")
	}

	// Without wire traffic the runtime falls back to the client's own view.
	fake := &fakeUpstream{recent: true}
	cl := upstream(fake)
	rt.clientPtr.Store(&cl)
	if !rt.hasRecentTraffic(time.Minute) {
		t.Fatal("client fallback must report recent traffic")
	}
	if !rt.hasRecentTraffic(0) {
		t.Fatal("non-positive window must default to the health interval")
	}
}

func TestNewManagerDefaultsAndAddrParsing(t *testing.T) {
	nodes := []*Node{tcpNode("a", "127.0.0.1:1")}

	// A non-positive health interval falls back to the default; a
	// non-numeric port keeps the default base port.
	m1, err := newManager(nodes, "127.0.0.1:abc", 0)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	if m1.healthInterval != defaultHealthCheckInterval {
		t.Fatalf("healthInterval=%v want default", m1.healthInterval)
	}
	if got := m1.runtimes[0].localSocks5; got != "127.0.0.1:10808" {
		t.Fatalf("runtime socks5=%q want 127.0.0.1:10808", got)
	}
	m1.Close()

	// A localSocks5 without any port falls back to host 127.0.0.1.
	m2, err := newManager(nodes, "loopback-no-port", -1)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m2.Close()
	if got := m2.runtimes[0].localSocks5; got != "127.0.0.1:10808" {
		t.Fatalf("runtime socks5=%q want 127.0.0.1:10808", got)
	}
}

func TestSwitchNode(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)
	m, err := newManager([]*Node{
		tcpNode("healthy", ln.Addr().String()),
		{Name: "bogus", Server: "127.0.0.1:1", ServerProto: "not-a-real-proto", Key: "k"},
	}, reserveTCPPort(t), time.Second)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()

	// Out of range indexes are rejected.
	if err := m.switchNode(-1); err == nil {
		t.Fatal("negative index must fail")
	}
	if err := m.switchNode(len(m.nodes)); err == nil {
		t.Fatal("too large index must fail")
	}

	// Starting the client for the bogus node fails and marks it down.
	if err := m.switchNode(1); err == nil {
		t.Fatal("bogus proto node must fail to switch")
	}
	if m.runtimes[1].alive.Load() {
		t.Fatal("a failed switch must mark the node down")
	}

	// Switching to the healthy node starts its client and activates it.
	if err := m.switchNode(0); err != nil {
		t.Fatalf("switchNode(0): %v", err)
	}
	if idx := m.ActiveNodeIndex(); idx != 0 {
		t.Fatalf("ActiveNodeIndex=%d want 0", idx)
	}
	if !m.NodeAlive(0) {
		t.Fatal("a successfully switched node must be alive")
	}

	// Switching again is a no-op success: the client is already running.
	if err := m.switchNode(0); err != nil {
		t.Fatalf("second switchNode(0): %v", err)
	}
}

func TestFallbackLocked(t *testing.T) {
	l1 := listenTCP(t)
	acceptAndClose(t, l1)
	l2 := listenTCP(t)
	acceptAndClose(t, l2)

	// n0 is a dead lower-priority node; n1/n2 are the live priority-1 tier.
	m, err := newManager([]*Node{
		{Name: "n0", Server: "127.0.0.1:1", ServerProto: "tcp", Priority: 2, Key: "k"},
		{Name: "n1", Server: l1.Addr().String(), ServerProto: "tcp", Priority: 1, Bandwidth: 100, Key: "k"},
		{Name: "n2", Server: l2.Addr().String(), ServerProto: "tcp", Priority: 1, Bandwidth: 300, Key: "k"},
	}, reserveTCPPort(t), time.Second)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()

	// exclude=-1 considers every node: the dead n0 is skipped, the
	// priority-1 tier wins and the higher-bandwidth n2 beats n1.
	m.mu.Lock()
	got := m.fallbackLocked(-1)
	m.mu.Unlock()
	if got != 2 {
		t.Fatalf("fallbackLocked(-1)=%d want 2", got)
	}
	if idx := m.ActiveNodeIndex(); idx != 2 {
		t.Fatalf("ActiveNodeIndex=%d want 2", idx)
	}

	// Excluding the current best selects the remaining alive node.
	m.mu.Lock()
	got = m.fallbackLocked(2)
	m.mu.Unlock()
	if got != 1 {
		t.Fatalf("fallbackLocked(2)=%d want 1", got)
	}
	if idx := m.ActiveNodeIndex(); idx != 1 {
		t.Fatalf("ActiveNodeIndex=%d want 1", idx)
	}
}

func TestFallbackLockedAllDead(t *testing.T) {
	m, err := newManager([]*Node{
		tcpNode("a", "127.0.0.1:1"),
		tcpNode("b", "127.0.0.1:2"),
	}, reserveTCPPort(t), time.Second)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()

	m.mu.Lock()
	got := m.fallbackLocked(-1)
	m.mu.Unlock()
	if got != -1 {
		t.Fatalf("fallbackLocked on an all-dead set=%d want -1", got)
	}
}

func TestFallbackLockedSwitchFailure(t *testing.T) {
	m, err := newManager([]*Node{
		tcpNode("a", "127.0.0.1:1"),
		tcpNode("b", "127.0.0.1:2"),
	}, reserveTCPPort(t), time.Second)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}

	// Pretend b is reachable, then close the manager: switchNode refuses to
	// start any client, so fallbackLocked must give up with -1.
	m.runtimes[1].alive.Store(true)
	m.Close()

	m.mu.Lock()
	got := m.fallbackLocked(-1)
	m.mu.Unlock()
	if got != -1 {
		t.Fatalf("fallbackLocked=%d want -1 when switchNode fails", got)
	}
}

func TestReportNodeFailureProbeConfirmsDown(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)
	m, err := newManager([]*Node{tcpNode("n0", ln.Addr().String())}, reserveTCPPort(t), time.Second)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()
	if !m.NodeAlive(0) {
		t.Fatal("node must start alive")
	}

	// The node goes down and two in-flight failures trigger the
	// confirmation probe, which must mark it down and wake the health loop.
	_ = ln.Close()
	m.ReportNodeFailure("n0", errors.New("conn reset"))
	m.ReportNodeFailure("n0", errors.New("conn reset"))

	waitFor(t, "probe to confirm node down", func() bool { return !m.NodeAlive(0) })
}

func TestReportNodeFailureProbeConfirmsAlive(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)
	m, err := newManager([]*Node{tcpNode("n0", ln.Addr().String())}, reserveTCPPort(t), time.Second)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()

	// Two failures against a healthy server: the confirmation probe
	// re-confirms liveness and resets the failure counter.
	m.ReportNodeFailure("n0", errors.New("boom"))
	m.ReportNodeFailure("n0", errors.New("boom"))

	waitFor(t, "probe to reset the failure counter", func() bool {
		return m.runtimes[0].consecutiveFails.Load() == 0
	})
	if !m.NodeAlive(0) {
		t.Fatal("a healthy node must stay alive")
	}
}

func TestReportNodeFailureSkipsCountingWithFreshTraffic(t *testing.T) {
	ln := listenTCP(t)
	acceptAndClose(t, ln)
	m, err := newManager([]*Node{tcpNode("n0", ln.Addr().String())}, reserveTCPPort(t), time.Second)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()

	// Fresh bidirectional traffic: a failure report must not count against
	// the node at all.
	m.RecordNodeTraffic("n0", 10, 10)
	m.ReportNodeFailure("n0", errors.New("ignored"))
	if f := m.runtimes[0].consecutiveFails.Load(); f != 0 {
		t.Fatalf("consecutiveFails=%d want 0", f)
	}
}

func TestReportNodeFailureTrafficRescuedDuringProbe(t *testing.T) {
	// A silent UDP listener never answers the RUDP handshake, so the
	// confirmation probe blocks for the full nodeProbeTimeout (3s) and the
	// test can record traffic while the probe is still in flight.
	silent, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer silent.Close()

	n0 := &Node{Name: "n0", Server: silent.LocalAddr().String(), ServerProto: "rudp", Key: "k"}
	rt := &nodeRuntime{node: n0, localSocks5: "127.0.0.1:19998"}
	rt.alive.Store(true) // pretend a session was already up
	m := &Manager{
		nodes:          []*Node{n0},
		runtimes:       []*nodeRuntime{rt},
		localSocks5:    "127.0.0.1:19998",
		checkStopCh:    make(chan struct{}),
		triggerCheckCh: make(chan struct{}, 1),
		activeIndex:    0,
		healthInterval: 10 * time.Second,
	}
	defer m.Close()

	m.ReportNodeFailure("n0", errors.New("stuck"))
	m.ReportNodeFailure("n0", errors.New("stuck"))

	// Real bidirectional traffic arrives mid-probe: the node must be
	// rescued and stay alive instead of being marked down.
	time.Sleep(200 * time.Millisecond)
	m.RecordNodeTraffic("n0", 100, 200)
	time.Sleep(3500 * time.Millisecond) // let the 3s probe finish

	if !rt.alive.Load() {
		t.Fatal("mid-probe bidirectional traffic must rescue the node")
	}
}

func TestProbeAllNodesTrafficRescue(t *testing.T) {
	// Same trick as above: the RUDP handshake probe blocks for ~3s while
	// bidirectional traffic is recorded, so probeAllNodes must keep the
	// node alive instead of trusting the failed synthetic probe.
	silent, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer silent.Close()

	n0 := &Node{Name: "n0", Server: silent.LocalAddr().String(), ServerProto: "rudp", Key: "k"}
	rt := &nodeRuntime{node: n0, localSocks5: "127.0.0.1:19997"}
	rt.alive.Store(true)
	m := &Manager{
		nodes:          []*Node{n0},
		runtimes:       []*nodeRuntime{rt},
		localSocks5:    "127.0.0.1:19997",
		checkStopCh:    make(chan struct{}),
		triggerCheckCh: make(chan struct{}, 1),
		activeIndex:    -1,
		healthInterval: 10 * time.Second,
	}
	defer m.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.probeAllNodes()
	}()

	time.Sleep(200 * time.Millisecond)
	m.RecordNodeTraffic("n0", 100, 200)
	<-done // probe gives up after nodeProbeTimeout

	if !rt.alive.Load() {
		t.Fatal("a failed probe must not mark a traffic-active node down")
	}
}

func TestProbeNodeWithRTTBranches(t *testing.T) {
	// pingtunnel nodes are probed through the throwaway pingtunnel client.
	if alive, _ := probeNodeWithRTT(&Node{Name: "pt", ServerProto: "pingtunnel", Server: "127.0.0.1", Key: "short"}, 500*time.Millisecond); alive {
		t.Fatal("pingtunnel probe with an invalid password must fail")
	}

	// A malformed address makes the protocol dialer fail synchronously.
	if alive, _ := probeNodeWithRTT(&Node{Name: "u", ServerProto: "udp", Server: "127.0.0.1:notaport"}, 500*time.Millisecond); alive {
		t.Fatal("a malformed udp address must fail the probe")
	}

	// A silent UDP listener never completes the RUDP handshake: the probe
	// must honour its timeout instead of hanging.
	silent, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer silent.Close()
	start := time.Now()
	if alive, _ := probeNodeWithRTT(&Node{Name: "r", ServerProto: "rudp", Server: silent.LocalAddr().String()}, 150*time.Millisecond); alive {
		t.Fatal("an rudp probe against a silent listener must time out")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe took %v, timeout not honoured", elapsed)
	}
}

func TestManagerWithPingTunnelNodeFailsGracefully(t *testing.T) {
	// Without root privileges the raw ICMP socket cannot be opened, so the
	// pingtunnel probe and client both fail; the manager must still come up
	// (and Close must release everything cleanly).
	m, err := newManager([]*Node{
		{Name: "pt", ServerProto: "pingtunnel", Server: "127.0.0.1", Key: "pingtunnel-pass"},
	}, reserveTCPPort(t), time.Second)
	if err != nil {
		t.Fatalf("newManager: %v", err)
	}
	defer m.Close()

	if m.NodeAlive(0) {
		t.Fatal("a pingtunnel node cannot be confirmed alive in a sandboxed test")
	}
	m.Close() // Close must stay idempotent
}

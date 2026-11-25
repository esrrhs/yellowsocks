package router

import (
	"net"
	"testing"
)

// The three modes are the only thing standing between "my rules" and "send
// everything to the proxy", and they are read on every connection, so the
// precedence between mode and rule lists is pinned here.
func TestModeOverridesRules(t *testing.T) {
	r := NewRouterWithOptions(Options{DirectDomains: []string{"example.cn"}})
	if got := r.Decide("example.cn", nil); got != Direct {
		t.Fatalf("rule mode: direct-listed domain = %v, want Direct", got)
	}
	if got := r.Decide("www.google.com", net.ParseIP("142.250.1.1")); got != Proxy {
		t.Fatalf("rule mode: unlisted domain = %v, want Proxy", got)
	}

	r.SetMode(ModeGlobal)
	if got := r.Decide("example.cn", nil); got != Proxy {
		t.Fatalf("global mode: direct-listed domain = %v, want Proxy", got)
	}
	// Private destinations must stay direct even in global mode: a remote node
	// cannot reach this LAN, and handing it internal addresses leaks topology.
	if got := r.Decide("192.168.1.1", net.ParseIP("192.168.1.1")); got != Direct {
		t.Fatalf("global mode: private address = %v, want Direct", got)
	}
	if got := r.Decide("127.0.0.1", net.ParseIP("127.0.0.1")); got != Direct {
		t.Fatalf("global mode: loopback = %v, want Direct", got)
	}

	r.SetMode(ModeDirect)
	if got := r.Decide("www.google.com", net.ParseIP("142.250.1.1")); got != Direct {
		t.Fatalf("direct mode: unlisted domain = %v, want Direct", got)
	}
}

// DNS decides between local and remote resolution with ShouldDirectDomain, so
// the mode has to be visible there too: global mode resolves every name
// remotely (that is what makes fake-ip cover the whole internet).
func TestShouldDirectDomainFollowsMode(t *testing.T) {
	r := NewRouterWithOptions(Options{DirectDomains: []string{"example.cn"}})
	if !r.ShouldDirectDomain("example.cn") {
		t.Fatal("rule mode: listed domain should resolve locally")
	}
	r.SetMode(ModeGlobal)
	if r.ShouldDirectDomain("example.cn") {
		t.Fatal("global mode: no domain may resolve locally")
	}
	r.SetMode(ModeDirect)
	if !r.ShouldDirectDomain("www.google.com") {
		t.Fatal("direct mode: every domain should resolve locally")
	}
}

func TestParseModeFallsBackToRule(t *testing.T) {
	cases := map[string]Mode{
		"":         ModeRule,
		"rule":     ModeRule,
		"GLOBAL":   ModeGlobal,
		" global ": ModeGlobal,
		"Direct":   ModeDirect,
		"nonsense": ModeRule,
	}
	for in, want := range cases {
		if got := ParseMode(in); got != want {
			t.Fatalf("ParseMode(%q) = %v, want %v", in, got, want)
		}
	}
	// The string form round-trips, which is what the settings file relies on.
	for _, m := range []Mode{ModeRule, ModeGlobal, ModeDirect} {
		if got := ParseMode(m.String()); got != m {
			t.Fatalf("round trip %v -> %q -> %v", m, m.String(), got)
		}
	}
}

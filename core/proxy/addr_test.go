package proxy

import (
	"net"
	"testing"
	"time"
)

func TestAddrIP(t *testing.T) {
	if ip := addrIP(nil); ip != nil {
		t.Fatalf("addrIP(nil) = %v, want nil", ip)
	}
	if ip := addrIP(&net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 1080}); ip == nil || !ip.Equal(net.ParseIP("192.0.2.7")) {
		t.Fatalf("tcp addr parsed as %v", ip)
	}
	if ip := addrIP(unparseableAddr{}); ip != nil {
		t.Fatalf("non-IP addr parsed as %v, want nil", ip)
	}
}

// unparseableAddr is a net.Addr whose String() is neither host:port nor an
// IP literal, forcing addrIP down its nil-return branch.
type unparseableAddr struct{}

func (unparseableAddr) Network() string { return "tcp" }
func (unparseableAddr) String() string  { return "not-an-address" }

type fakeRelayConn struct {
	local  net.Addr
	remote net.Addr
}

func (c *fakeRelayConn) Read([]byte) (int, error)         { return 0, nil }
func (c *fakeRelayConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *fakeRelayConn) Close() error                     { return nil }
func (c *fakeRelayConn) LocalAddr() net.Addr              { return c.local }
func (c *fakeRelayConn) RemoteAddr() net.Addr             { return c.remote }
func (c *fakeRelayConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeRelayConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeRelayConn) SetWriteDeadline(time.Time) error { return nil }

// listenClientUDPRelay used to call remote.To4() before checking remote for
// nil, which panicked when the peer address was not IP-parsable.
func TestListenClientUDPRelayNilAddrDoesNotPanic(t *testing.T) {
	cases := []struct {
		name   string
		conn   net.Conn
		wantV6 bool
	}{
		{
			name:   "both addresses unparsable falls back to ipv4",
			conn:   &fakeRelayConn{local: unparseableAddr{}, remote: unparseableAddr{}},
			wantV6: false,
		},
		{
			name: "ipv4 peer",
			conn: &fakeRelayConn{
				local:  &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1080},
				remote: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 40001},
			},
			wantV6: false,
		},
		{
			name: "ipv6 peer",
			conn: &fakeRelayConn{
				local:  &net.TCPAddr{IP: net.ParseIP("::1"), Port: 1080},
				remote: &net.TCPAddr{IP: net.ParseIP("::1"), Port: 40001},
			},
			wantV6: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, bnd, err := listenClientUDPRelay(tc.conn)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer conn.Close()

			host, _, err := net.SplitHostPort(bnd)
			if err != nil {
				t.Fatalf("bnd addr %q: %v", bnd, err)
			}
			isV6 := net.ParseIP(host).To4() == nil
			if isV6 != tc.wantV6 {
				t.Fatalf("relay bound to %s, wantIPv6=%v", bnd, tc.wantV6)
			}
		})
	}
}

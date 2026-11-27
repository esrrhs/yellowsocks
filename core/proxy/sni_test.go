package proxy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// clientHello builds a minimal but well-formed TLS 1.2 ClientHello record
// carrying one server_name extension.
func clientHello(serverName string) []byte {
	var exts []byte
	if serverName != "" {
		name := []byte(serverName)
		entry := append([]byte{0x00, byte(len(name) >> 8), byte(len(name))}, name...)
		list := append([]byte{byte(len(entry) >> 8), byte(len(entry))}, entry...)
		exts = append([]byte{0x00, 0x00, byte(len(list) >> 8), byte(len(list))}, list...)
	}

	body := []byte{0x03, 0x03}                              // legacy version
	body = append(body, make([]byte, 32)...)                // random
	body = append(body, 0x00)                               // session id length
	body = append(body, 0x00, 0x04, 0x13, 0x01, 0x13, 0x02) // cipher suites
	body = append(body, 0x01, 0x00)                         // compression methods
	body = append(body, byte(len(exts)>>8), byte(len(exts)))
	body = append(body, exts...)

	msg := []byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	msg = append(msg, body...)

	rec := []byte{0x16, 0x03, 0x01, byte(len(msg) >> 8), byte(len(msg))}
	return append(rec, msg...)
}

func TestSniffTLSClientHello(t *testing.T) {
	hello := clientHello("static.zhihu.com")
	name, decided := sniffName(hello)
	if !decided || name != "static.zhihu.com" {
		t.Fatalf("sniffName = %q,%v want static.zhihu.com,true", name, decided)
	}
}

func TestSniffAcrossReads(t *testing.T) {
	hello := clientHello("img.example.org")
	// Every split point must yield the same answer, because a ClientHello is
	// not guaranteed to arrive in one read.
	for cut := 1; cut < len(hello); cut++ {
		var s sniSniffer
		name, decided := s.feed(hello[:cut])
		if decided {
			t.Fatalf("cut %d: decided %q too early", cut, name)
		}
		name, decided = s.feed(hello[cut:])
		if !decided || name != "img.example.org" {
			t.Fatalf("cut %d: got %q,%v", cut, name, decided)
		}
	}
}

func TestSniffHTTPHost(t *testing.T) {
	req := []byte("GET /index.html HTTP/1.1\r\nHost: api.example.org:8443\r\nAccept: */*\r\n\r\n")
	name, decided := sniffName(req)
	if !decided || name != "api.example.org" {
		t.Fatalf("sniffName = %q,%v want api.example.org,true", name, decided)
	}

	// A request head that has not arrived in full yet waits for the blank line.
	var s sniSniffer
	part := req[:20]
	if _, decided := s.feed(part); decided {
		t.Fatal("decided before the head was complete")
	}
	if name, decided := s.feed(req[20:]); !decided || name != "api.example.org" {
		t.Fatalf("resumed: got %q,%v", name, decided)
	}
}

func TestSniffGivesUpOnUnknownProtocols(t *testing.T) {
	cases := map[string][]byte{
		"binary":       {0x00, 0x01, 0x02, 0x03},
		"ssh":          []byte("SSH-2.0-OpenSSH_9.0"),
		"tls-nonhello": {0x16, 0x03, 0x01, 0x00, 0x02, 0x16, 0x03},
	}
	for name, b := range cases {
		got, decided := sniffName(b)
		if got != "" || !decided {
			t.Errorf("%s: got %q,%v want \"\",true", name, got, decided)
		}
	}
}

func TestSniffRejectsUnusableNames(t *testing.T) {
	for _, sni := range []string{"1.2.3.4", "a b", "exa\r\nmple.com"} {
		name, decided := sniffName(clientHello(sni))
		if !decided || name != "" {
			t.Errorf("%q: got %q,%v want \"\",true", sni, name, decided)
		}
	}
	// No server_name extension at all (e.g. a name hidden by ECH).
	if name, decided := sniffName(clientHello("")); !decided || name != "" {
		t.Errorf("no SNI: got %q,%v", name, decided)
	}
}

func TestSnifferStopsAfterDecision(t *testing.T) {
	var s sniSniffer
	if name, decided := s.feed(clientHello("a.example.com")); !decided || name != "a.example.com" {
		t.Fatalf("first feed: %q,%v", name, decided)
	}
	if s.buf != nil {
		t.Fatal("buffer was retained after a decision")
	}
	if name, decided := s.feed(clientHello("b.example.com")); !decided || name != "" {
		t.Fatalf("second feed must be a no-op, got %q,%v", name, decided)
	}
}

func TestSnifferCapsBuffer(t *testing.T) {
	var s sniSniffer
	// A stream that never resolves must be abandoned, not buffered forever.
	chunk := bytes.Repeat([]byte{'A'}, 512)
	for i := 0; i < 16; i++ {
		if _, decided := s.feed(chunk); decided {
			return
		}
	}
	t.Fatal("sniffer never gave up")
}

// ---- server certificate sniffer ----

// selfSignedCert builds a throwaway TLS certificate carrying the given SANs,
// with whatever common name the test wants to see ignored.
func selfSignedCert(t *testing.T, dnsNames []string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return der
}

// tlsHandshakeBytes wraps handshake messages in TLS records the way a server
// flight looks on the wire.
func tlsHandshakeBytes(msgs ...[]byte) []byte {
	var stream []byte
	for _, m := range msgs {
		stream = append(stream, m...)
	}
	rec := []byte{0x16, 0x03, 0x03,
		byte(len(stream) >> 8), byte(len(stream))}
	return append(rec, stream...)
}

func tlsServerHello() []byte {
	msg := []byte{0x02, 0x00, 0x00, 0x28} // ServerHello, 40 bytes
	msg = append(msg, 0x03, 0x03)         // legacy version
	msg = append(msg, make([]byte, 32)...) // random
	msg = append(msg, 0x00)               // session id len
	msg = append(msg, 0x13, 0x01)         // cipher
	msg = append(msg, 0x00)               // compression
	msg = append(msg, 0x00, 0x00)         // extensions len
	return msg
}

func tlsCertificateMsg(der []byte) []byte {
	entry := []byte{byte(len(der) >> 16), byte(len(der) >> 8), byte(len(der))}
	entry = append(entry, der...)
	body := []byte{byte(len(entry) >> 16), byte(len(entry) >> 8), byte(len(entry))}
	body = append(body, entry...)
	msg := append([]byte{0x0b, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	return msg
}

func TestServerCertSnifferRecoversSAN(t *testing.T) {
	der := selfSignedCert(t, []string{"*.ftn.qq.com", "ftn.qq.com"})
	wire := tlsHandshakeBytes(tlsServerHello(), tlsCertificateMsg(der))

	var s serverCertSniffer
	var name string
	var decided bool
	for i := 0; i < len(wire); i++ {
		name, decided = s.feed(wire[i : i+1])
		if decided {
			break
		}
	}
	if !decided {
		t.Fatal("sniffer never decided on a complete flight")
	}
	if name != "ftn.qq.com" {
		t.Fatalf("recovered name = %q, want the exact SAN entry ftn.qq.com", name)
	}
}

func TestServerCertSnifferPrefersExactOverWildcard(t *testing.T) {
	der := selfSignedCert(t, []string{"*.weiyun.com"})
	wire := tlsHandshakeBytes(tlsServerHello(), tlsCertificateMsg(der))

	var s serverCertSniffer
	name, decided := s.feed(wire)
	if !decided || name != "weiyun.com" {
		t.Fatalf("recovered = %q decided=%v, want wildcard stripped to weiyun.com", name, decided)
	}
}

func TestServerCertSnifferGivesUpOnEncryptedFlight(t *testing.T) {
	// TLS 1.3: EncryptedExtensions (0x08) follows the ServerHello; the
	// certificate is unreadable and the sniffer must stop with no name.
	msg := append([]byte{0x08, 0x00, 0x00, 0x04}, []byte{0x00, 0x02, 0x00, 0x00}...)
	wire := tlsHandshakeBytes(tlsServerHello(), msg)

	var s serverCertSniffer
	name, decided := s.feed(wire)
	if !decided || name != "" {
		t.Fatalf("recovered = %q decided=%v, want a quiet give-up on TLS 1.3", name, decided)
	}
}

func TestServerCertSnifferNonTLS(t *testing.T) {
	var s serverCertSniffer
	name, decided := s.feed([]byte("HTTP/1.1 200 OK\r\n"))
	if !decided || name != "" {
		t.Fatalf("recovered = %q decided=%v, want an immediate give-up on non-TLS", name, decided)
	}
}

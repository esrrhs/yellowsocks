package dns

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func writeTestTLSCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certOut, err := os.Create(certFile)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	_ = certOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyOut, err := os.Create(keyFile)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	_ = keyOut.Close()
	return certFile, keyFile
}

func TestDNSServerUDPAndDoH(t *testing.T) {
	// Pick free ports for UDP DNS and TCP DoH
	udpL, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen packet: %v", err)
	}
	udpAddr := udpL.LocalAddr().String()
	_ = udpL.Close()

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen tcp: %v", err)
	}
	dohAddr := tcpL.Addr().String()
	_ = tcpL.Close()

	srv, err := NewServer(Config{
		ListenAddr:    udpAddr,
		DoHListenAddr: dohAddr,
		EnableFakeIP:  true, // use fake ip for fast deterministic unit test
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop()

	// Wait briefly for listeners to start
	time.Sleep(100 * time.Millisecond)

	// 1. Test UDP DNS query
	c := new(dns.Client)
	c.Timeout = 2 * time.Second
	m := new(dns.Msg)
	m.SetQuestion("google.com.", dns.TypeA)

	resp, _, err := c.Exchange(m, udpAddr)
	if err != nil {
		t.Fatalf("UDP DNS exchange failed: %v", err)
	}
	if len(resp.Answer) == 0 {
		t.Fatalf("expected answer in UDP DNS response, got 0")
	}
	aRecord, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("expected A record, got %T", resp.Answer[0])
	}
	if aRecord.A == nil {
		t.Fatalf("expected non-nil IP in A record")
	}

	// Verify reverse lookup
	domain, ok := srv.LookupDomainByIP(aRecord.A.String())
	if !ok || domain != "google.com" {
		t.Errorf("expected domain google.com from reverse lookup of %s, got %s (ok=%v)", aRecord.A.String(), domain, ok)
	}

	// 2. Test TCP DoH query via HTTP GET (?dns=...)
	dohQueryMsg := new(dns.Msg)
	dohQueryMsg.SetQuestion("github.com.", dns.TypeA)
	packedQuery, err := dohQueryMsg.Pack()
	if err != nil {
		t.Fatalf("failed to pack doh query msg: %v", err)
	}

	b64Query := base64.RawURLEncoding.EncodeToString(packedQuery)
	getUrl := "http://" + dohAddr + "/dns-query?dns=" + b64Query
	httpResp, err := http.Get(getUrl)
	if err != nil {
		t.Fatalf("DoH GET request failed: %v", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 from DoH GET, got %d", httpResp.StatusCode)
	}
	if ct := httpResp.Header.Get("Content-Type"); ct != "application/dns-message" {
		t.Fatalf("expected Content-Type application/dns-message, got %s", ct)
	}
	getRespBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		t.Fatalf("failed to read DoH GET body: %v", err)
	}
	getRespMsg := new(dns.Msg)
	if err := getRespMsg.Unpack(getRespBody); err != nil {
		t.Fatalf("failed to unpack DoH GET response: %v", err)
	}
	if len(getRespMsg.Answer) == 0 {
		t.Fatalf("expected answer in DoH GET response, got 0")
	}

	// 3. Test TCP DoH query via HTTP POST (binary body)
	postUrl := "http://" + dohAddr + "/dns-query"
	postReq, err := http.NewRequest("POST", postUrl, bytes.NewReader(packedQuery))
	if err != nil {
		t.Fatalf("failed to create DoH POST request: %v", err)
	}
	postReq.Header.Set("Content-Type", "application/dns-message")
	postReq.Header.Set("Accept", "application/dns-message")

	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("DoH POST request failed: %v", err)
	}
	defer postResp.Body.Close()

	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 from DoH POST, got %d", postResp.StatusCode)
	}
	postRespBody, err := io.ReadAll(postResp.Body)
	if err != nil {
		t.Fatalf("failed to read DoH POST body: %v", err)
	}
	postRespMsg := new(dns.Msg)
	if err := postRespMsg.Unpack(postRespBody); err != nil {
		t.Fatalf("failed to unpack DoH POST response: %v", err)
	}
	if len(postRespMsg.Answer) == 0 {
		t.Fatalf("expected answer in DoH POST response, got 0")
	}
}

func TestDNSServerDoT(t *testing.T) {
	udpL, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	udpAddr := udpL.LocalAddr().String()
	_ = udpL.Close()

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	dotAddr := tcpL.Addr().String()
	_ = tcpL.Close()

	certFile, keyFile := writeTestTLSCert(t, t.TempDir())
	srv, err := NewServer(Config{
		ListenAddr:    udpAddr,
		DoTListenAddr: dotAddr,
		TLSCertFile:   certFile,
		TLSKeyFile:    keyFile,
		EnableFakeIP:  true,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Stop()
	time.Sleep(100 * time.Millisecond)

	c := &dns.Client{
		Net: "tcp-tls",
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "localhost",
			NextProtos:         []string{"dot"},
		},
		Timeout: 2 * time.Second,
	}
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	resp, _, err := c.Exchange(m, dotAddr)
	if err != nil {
		t.Fatalf("DoT exchange failed: %v", err)
	}
	if len(resp.Answer) == 0 {
		t.Fatalf("expected DoT answer, got 0")
	}
}

func TestNewServerDoTRequiresCert(t *testing.T) {
	_, err := NewServer(Config{DoTListenAddr: ":853"})
	if err == nil {
		t.Fatal("expected error when DoT enabled without cert/key")
	}
}

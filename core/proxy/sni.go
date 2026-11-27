package proxy

import (
	"bytes"
	"crypto/x509"
	"net"
	"strings"
)

// Clients that resolve names themselves dial an IP literal, so a SOCKS5 or
// HTTP relay only ever learns the address. That address is useless in a
// connection list — "where is this going" is the question, and 112.49.67.179
// does not answer it. The name is still on the wire, though: a TLS client puts
// it in the SNI extension of its ClientHello, and a plain HTTP client puts it
// in the Host header. Both are inside the first bytes the client sends, which
// pass through the relay anyway.
//
// There is a third source for the clients that name nothing at all: the
// SERVER. A TLS server presents its certificate in the first flight, and on
// TLS 1.2 that certificate is plaintext — its SAN answers "where is this
// going" authoritatively even when the client stayed silent. serverCertSniffer
// below reads it off the relayed bytes; TLS 1.3 encrypts the certificate, and
// the sniffer simply gives up there.

// sniffLimit bounds how many leading bytes are kept while looking for the
// name. A ClientHello is typically well under 1 KiB; anything that does not
// resolve inside this window is given up on rather than buffered forever.
const sniffLimit = 4096

// sniSniffer inspects the first bytes of a stream, in order.
type sniSniffer struct {
	buf  []byte
	done bool
}

// feed takes the bytes that were just relayed and reports the name it found.
// decided is false when more bytes are needed to tell.
//
// The common case — a ClientHello that arrives in the first read — parses
// straight from b with no allocation or buffering. A copy is only kept when
// the name is split across reads, which is rare.
func (s *sniSniffer) feed(b []byte) (string, bool) {
	if s.done || len(b) == 0 {
		return "", s.done
	}
	if len(s.buf) > 0 {
		if room := sniffLimit - len(s.buf); room > 0 {
			if len(b) > room {
				b = b[:room]
			}
			s.buf = append(s.buf, b...)
		}
		b = s.buf
	}
	name, decided := sniffName(b)
	if !decided {
		if len(b) >= sniffLimit {
			decided = true
		} else if len(s.buf) == 0 && len(b) < sniffLimit {
			// Keep a copy to join with the next read. Copy rather than
			// retain: the caller reuses its buffer on the next loop.
			s.buf = append([]byte(nil), b...)
		}
	}
	if decided {
		s.done = true
		s.buf = nil
	}
	return name, decided
}

// sniffName looks for the requested name in the leading bytes of a stream.
func sniffName(b []byte) (string, bool) {
	if len(b) == 0 {
		return "", false
	}
	if b[0] == 0x16 {
		return sniffTLS(b)
	}
	if isUpperAlpha(b[0]) {
		if looksLikeHTTPMethod(b) {
			return sniffHTTP(b)
		}
		// A method name still in flight ("GE") is worth waiting for; anything
		// else that starts with a capital letter is not a request line.
		if bytes.IndexByte(b, ' ') < 0 && len(b) < 8 {
			return "", false
		}
	}
	return "", true
}

func isUpperAlpha(c byte) bool { return c >= 'A' && c <= 'Z' }

// looksLikeHTTPMethod reports whether b starts with a request method followed
// by a space.
func looksLikeHTTPMethod(b []byte) bool {
	end := bytes.IndexByte(b, ' ')
	if end < 0 {
		// No space yet: only methods up to 8 bytes long can still match.
		return len(b) < 8
	}
	switch string(b[:end]) {
	case "GET", "POST", "HEAD", "PUT", "DELETE", "OPTIONS", "PATCH", "CONNECT", "TRACE":
		return true
	}
	return false
}

// sniffTLS parses the SNI out of a TLS ClientHello record.
func sniffTLS(b []byte) (string, bool) {
	if len(b) < 5 {
		return "", false
	}
	if b[1] != 0x03 { // not a TLS record after all
		return "", true
	}
	recLen := int(b[3])<<8 | int(b[4])
	if recLen == 0 {
		return "", true
	}
	if 5+recLen > sniffLimit && len(b) < 5+recLen {
		return "", true // bigger than the window: cannot be assembled here
	}
	if len(b) < 5+recLen {
		return "", false
	}
	msg := b[5 : 5+recLen]
	if len(msg) < 4 || msg[0] != 0x01 { // handshake type: ClientHello
		return "", true
	}
	hl := int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3])
	if hl <= 0 || 4+hl > len(msg) {
		return "", true // truncated body: do not guess
	}
	return parseClientHello(msg[4 : 4+hl])
}

// parseClientHello walks the handshake body up to the extensions block and
// returns the first host_name entry of the server_name extension.
func parseClientHello(b []byte) (string, bool) {
	// legacy_version (2) + random (32)
	const fixed = 34
	if len(b) < fixed {
		return "", true
	}
	i := fixed
	if i >= len(b) {
		return "", true
	}
	i += 1 + int(b[i]) // session id
	if i+2 > len(b) {
		return "", true
	}
	i += 2 + (int(b[i])<<8 | int(b[i+1])) // cipher suites
	if i+1 > len(b) {
		return "", true
	}
	i += 1 + int(b[i]) // compression methods
	if i+2 > len(b) {
		return "", true
	}
	end := i + 2 + (int(b[i])<<8 | int(b[i+1])) // extensions
	i += 2
	if end > len(b) {
		return "", true
	}
	for i+4 <= end {
		etype := int(b[i])<<8 | int(b[i+1])
		elen := int(b[i+2])<<8 | int(b[i+3])
		i += 4
		if i+elen > end {
			return "", true
		}
		if etype == 0 { // server_name
			return parseServerName(b[i : i+elen])
		}
		i += elen
	}
	return "", true
}

// parseServerName reads the host_name out of a server_name extension body.
func parseServerName(ext []byte) (string, bool) {
	if len(ext) < 2 {
		return "", true
	}
	listLen := int(ext[0])<<8 | int(ext[1])
	if 2+listLen > len(ext) {
		return "", true
	}
	p := ext[2 : 2+listLen]
	for len(p) >= 3 {
		typ := p[0]
		n := int(p[1])<<8 | int(p[2])
		p = p[3:]
		if n > len(p) {
			return "", true
		}
		if typ == 0 {
			return cleanHost(string(p[:n])), true
		}
		p = p[n:]
	}
	return "", true
}

// sniffHTTP reads the Host header of a plain HTTP request head.
func sniffHTTP(b []byte) (string, bool) {
	end := bytes.Index(b, []byte("\r\n\r\n"))
	if end < 0 {
		return "", false
	}
	lines := strings.Split(string(b[:end]), "\r\n")
	for _, line := range lines[1:] {
		if len(line) >= 5 && strings.EqualFold(line[:5], "host:") {
			return cleanHost(line[5:]), true
		}
	}
	return "", true
}

// cleanHost normalises a candidate name and drops anything that is not a
// usable hostname, so a malformed extension cannot put junk in the UI.
func cleanHost(s string) string {
	s = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ".")))
	// Strip a port, e.g. "example.com:443".
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	if s == "" || len(s) > 255 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == '_':
		default:
			return "" // punycode/IDN is xn--, so this is enough
		}
	}
	if net.ParseIP(s) != nil {
		return "" // an address is what we already had
	}
	if !strings.Contains(s, ".") && s != "localhost" {
		return ""
	}
	return s
}

// ---- server certificate sniffing ----

// certSniffLimit bounds how many server bytes are kept while reconstructing
// the handshake. A TLS 1.2 ServerHello plus its certificate chain fits well
// inside this on any real deployment; a bigger flight is not worth buffering.
const certSniffLimit = 64 * 1024

// serverCertSniffer walks the server's first TLS flight, in record and
// handshake order, looking for the Certificate message. It is the
// client-side sniffer's complement: SNI names what the client asked for,
// the certificate names what the server answered as — the only name on the
// wire for clients that dial by IP without SNI. TLS 1.3 encrypts the
// certificate (EncryptedExtensions follows the ServerHello), so there the
// sniffer decides with no name.
type serverCertSniffer struct {
	buf       []byte // unframed record bytes
	handshake []byte // reassembled handshake stream after the record headers
	sawHello  bool
	done      bool
}

// feed takes the bytes that were just relayed from the server and reports
// the name on its certificate, if one is reachable.
func (s *serverCertSniffer) feed(b []byte) (string, bool) {
	if s.done || len(b) == 0 {
		return "", s.done
	}
	s.buf = append(s.buf, b...)

	// Frame complete records into the handshake stream.
	for {
		if len(s.buf) < 5 {
			break
		}
		if s.buf[0] != 0x16 || s.buf[1] != 0x03 {
			// Not a TLS handshake record: server is not speaking TLS (or is
			// speaking something we cannot read a name from).
			s.done = true
			return "", true
		}
		recLen := int(s.buf[3])<<8 | int(s.buf[4])
		if recLen == 0 {
			s.done = true
			return "", true
		}
		if len(s.buf) < 5+recLen {
			break // wait for the rest of the record
		}
		s.handshake = append(s.handshake, s.buf[5:5+recLen]...)
		s.buf = s.buf[5+recLen:]
	}

	// Parse complete handshake messages out of the stream.
	for {
		if len(s.handshake) < 4 {
			break
		}
		msgType := s.handshake[0]
		msgLen := int(s.handshake[1])<<16 | int(s.handshake[2])<<8 | int(s.handshake[3])
		if len(s.handshake) < 4+msgLen {
			break // wait for the rest of the message
		}
		msg := s.handshake[4 : 4+msgLen]
		s.handshake = s.handshake[4+msgLen:]

		switch msgType {
		case 0x02: // ServerHello (or HelloRetryRequest — either way, keep going)
			s.sawHello = true
		case 0x0b: // Certificate
			if !s.sawHello {
				s.done = true
				return "", true
			}
			name := parseServerCertificate(msg)
			s.done = true
			return name, true
		default:
			if s.sawHello {
				// The first flight after ServerHello belongs to the
				// negotiated version: on TLS 1.3 it is EncryptedExtensions
				// and the certificate is unreadable.
				s.done = true
				return "", true
			}
			s.done = true
			return "", true
		}
	}

	if len(s.buf)+len(s.handshake) > certSniffLimit {
		s.done = true
		return "", true
	}
	return "", false
}

// parseServerCertificate reads the first certificate of a TLS Certificate
// handshake message and returns a usable host name from it: an exact SAN
// entry first, then the sharpest wildcard, then the common name.
func parseServerCertificate(msg []byte) string {
	if len(msg) < 3 {
		return ""
	}
	listLen := int(msg[0])<<16 | int(msg[1])<<8 | int(msg[2])
	body := msg[3:]
	if listLen > len(body) {
		listLen = len(body)
	}
	body = body[:listLen]
	if len(body) < 3 {
		return ""
	}
	certLen := int(body[0])<<16 | int(body[1])<<8 | int(body[2])
	if certLen > len(body)-3 {
		certLen = len(body) - 3
	}
	cert, err := x509.ParseCertificate(body[3 : 3+certLen])
	if err != nil {
		return ""
	}
	for _, d := range cert.DNSNames {
		if !strings.HasPrefix(d, "*.") {
			if n := cleanHost(d); n != "" {
				return n
			}
		}
	}
	for _, d := range cert.DNSNames {
		if strings.HasPrefix(d, "*.") {
			if n := cleanHost(d[2:]); n != "" {
				return n
			}
		}
	}
	return cleanHost(cert.Subject.CommonName)
}

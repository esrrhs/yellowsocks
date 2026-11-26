package guiapp

import (
	"strings"
	"testing"
)

func TestProxyPortsAndFormat(t *testing.T) {
	httpPort, socksPort, err := proxyPorts("127.0.0.1:8080", "127.0.0.1:10808")
	if err != nil {
		t.Fatalf("proxyPorts: %v", err)
	}
	if httpPort != 8080 || socksPort != 10808 {
		t.Fatalf("proxyPorts = %d, %d", httpPort, socksPort)
	}
	got := formatProxyPorts(httpPort, socksPort)
	want := "http=127.0.0.1:8080;https=127.0.0.1:8080;socks=127.0.0.1:10808"
	if got != want {
		t.Fatalf("formatProxyPorts = %q, want %q", got, want)
	}
	if _, _, err := proxyPorts("", ""); err == nil {
		t.Fatal("proxyPorts accepted empty addresses")
	}
}

func TestProxyBypassListCoversLocalAndPrivate(t *testing.T) {
	list := proxyBypassList()
	for _, token := range []string{"localhost", "127.*", "10.*", "192.168.*", "<local>"} {
		if !strings.Contains(list, token) {
			t.Fatalf("bypass list %q missing %q", list, token)
		}
	}
}

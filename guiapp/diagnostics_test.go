package guiapp

import "testing"

// The exit-IP lookup picks the first provider that answers with a country, so
// every parser has to be tolerant: these are real payload shapes, trimmed.
func TestParseIPInfo(t *testing.T) {
	body := `{"ip":"104.28.211.7","city":"Los Angeles","region":"California","country":"US","loc":"34.05,-118.24","org":"AS13335 Cloudflare"}`
	g := parseIPInfo(body)
	if g.ip != "104.28.211.7" || g.country != "US" || g.city != "Los Angeles" {
		t.Fatalf("unexpected geo: %+v", g)
	}

	// Bogons come back with no country at all; the address is still usable.
	if g := parseIPInfo(`{"ip":"10.0.0.1","bogon":true}`); g.ip != "10.0.0.1" || g.country != "" {
		t.Fatalf("bogon: %+v", g)
	}
	if g := parseIPInfo(`not json`); g.ip != "" {
		t.Fatalf("garbage accepted: %+v", g)
	}
}

func TestParseIPWho(t *testing.T) {
	body := `{"ip":"45.76.1.2","success":true,"country":"Japan","country_code":"JP","city":"Tokyo"}`
	g := parseIPWho(body)
	if g.ip != "45.76.1.2" || g.country != "JP" || g.city != "Tokyo" {
		t.Fatalf("unexpected geo: %+v", g)
	}
	// success:false is the documented failure shape (reserved range, etc).
	if g := parseIPWho(`{"ip":"192.168.1.1","success":false,"message":"Reserved range"}`); g.ip != "" {
		t.Fatalf("failed lookup accepted: %+v", g)
	}
}

func TestParseCloudflareTrace(t *testing.T) {
	body := "fl=1f2\nh=www.cloudflare.com\nip=203.0.113.9\nts=1700000000\nvisit_scheme=https\nloc=NL\nwarp=off\n"
	g := parseCloudflareTrace(body)
	if g.ip != "203.0.113.9" || g.country != "NL" {
		t.Fatalf("unexpected geo: %+v", g)
	}
	// Tor answers loc=T1, which is not a country.
	if g := parseCloudflareTrace("ip=1.2.3.4\nloc=T1\n"); g.country != "" || g.ip != "1.2.3.4" {
		t.Fatalf("tor exit: %+v", g)
	}
}

func TestParseIPOnly(t *testing.T) {
	if g := parseIPOnly(`{"ip":"8.8.8.8"}`); g.ip != "8.8.8.8" || g.country != "" {
		t.Fatalf("unexpected geo: %+v", g)
	}
	// Plain-text shape (ifconfig.me) still works through parseIPify.
	if g := parseIPOnly("8.8.4.4\n"); g.ip != "8.8.4.4" {
		t.Fatalf("plain text: %+v", g)
	}
}

func TestNormCountry(t *testing.T) {
	ok := map[string]string{"us": "US", " De ": "DE", "\tjp\n": "JP"}
	for in, want := range ok {
		if got := normCountry(in); got != want {
			t.Fatalf("normCountry(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "USA", "1A", "XX", "T1", "U-"} {
		if got := normCountry(in); got != "" {
			t.Fatalf("normCountry(%q) = %q, want empty", in, got)
		}
	}
}

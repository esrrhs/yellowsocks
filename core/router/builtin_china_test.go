package router

import (
	"testing"

	gohomedns "github.com/esrrhs/gohome/dns"
)

func TestBuiltinIncludesXiaohongshuFromGohome(t *testing.T) {
	haveXHS, haveCDN := false, false
	for _, d := range gohomedns.DefaultChinaMainDomains {
		switch d {
		case "xiaohongshu.com":
			haveXHS = true
		case "xhscdn.com":
			haveCDN = true
		}
	}
	if !haveXHS || !haveCDN {
		t.Fatalf("gohome DefaultChinaMainDomains missing xhs entries (xhs=%vcdn=%v, n=%d)",
			haveXHS, haveCDN, len(gohomedns.DefaultChinaMainDomains))
	}

	r := NewRouter()
	defer r.Close()
	for _, host := range []string{
		"xiaohongshu.com",
		"www.xiaohongshu.com",
		"sns-video-v6.xhscdn.com",
		"xhscdn.com",
	} {
		if d := r.Decide(host, nil); d != Direct {
			t.Errorf("expected Direct for %s, got %v", host, d)
		}
	}
}

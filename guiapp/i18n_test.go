package guiapp

import (
	"encoding/json"
	"strings"
	"testing"
)

// The frontend build already fails when the two catalogues disagree, but the
// Go side renders from the same files (the tray), so it needs its own guard:
// a key present in one language only would silently fall back to Chinese in
// an English menu.
func TestLocaleCataloguesMatch(t *testing.T) {
	zh, en := localeTables["zh"], localeTables["en"]
	if len(zh) == 0 || len(en) == 0 {
		t.Fatal("locale tables are empty")
	}
	for key := range zh {
		if _, ok := en[key]; !ok {
			t.Errorf("key %q is missing from en.json", key)
		}
	}
	for key := range en {
		if _, ok := zh[key]; !ok {
			t.Errorf("key %q is missing from zh.json", key)
		}
	}
}

func TestTRFallsBack(t *testing.T) {
	if got := tr("en", "tray.show", nil); got != "Show window" {
		t.Errorf("en tray.show = %q", got)
	}
	if got := tr("zh", "tray.conns", map[string]string{"n": "3"}); got != "连接: 3" {
		t.Errorf("zh tray.conns = %q", got)
	}
	// Unknown language: Chinese, not an empty menu item.
	if got := tr("de", "tray.show", nil); got != "显示主界面" {
		t.Errorf("fallback tray.show = %q", got)
	}
	// Unknown key: the key itself, which is greppable in a bug report.
	if got := tr("en", "no.such.key", nil); got != "no.such.key" {
		t.Errorf("missing key = %q", got)
	}
}

func TestNormalizeLang(t *testing.T) {
	cases := map[string]string{
		"en":      "en",
		"EN":      "en",
		"en-US":   "en",
		"zh-CN":   "zh",
		"zh_Hans": "zh",
		"":        "zh",
		"fr":      "zh",
	}
	for in, want := range cases {
		if got := normalizeLang(in); got != want {
			t.Errorf("normalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

// A Go-side failure that the app words itself must cross the binding as a
// catalogue key, never as prose: prose is what left the error banner Chinese
// in an English window.
func TestUIErrorIsMachineReadable(t *testing.T) {
	err := uiErr("err.tunPortChanged", map[string]string{"from": "a:1", "to": "b:2"})
	msg := err.Error()
	if !strings.HasPrefix(msg, uiErrorPrefix) {
		t.Fatalf("message %q does not carry the prefix", msg)
	}
	var payload uiError
	if err := json.Unmarshal([]byte(strings.TrimPrefix(msg, uiErrorPrefix)), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload.Key != "err.tunPortChanged" || payload.Vars["from"] != "a:1" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if _, ok := localeTables["zh"][payload.Key]; !ok {
		t.Fatalf("key %q is not in the catalogue", payload.Key)
	}
}

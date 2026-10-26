package dashboard

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

func TestHTMLNonEmpty(t *testing.T) {
	html := HTML()
	if len(html) == 0 {
		t.Fatal("embedded dashboard page is empty")
	}
	if !utf8.Valid(html) {
		t.Fatal("embedded dashboard page is not valid UTF-8")
	}
}

// The page is a standalone HTML document: it must carry the usual document
// markers so browsers render it instead of downloading it.
func TestHTMLLooksLikeHTML(t *testing.T) {
	html := bytes.ToLower(HTML())
	for _, marker := range []string{"<!doctype html", "<html", "</html>", "<body", "</body>"} {
		if !bytes.Contains(html, []byte(marker)) {
			t.Errorf("dashboard page missing %q", marker)
		}
	}
}

func TestHTMLStableAcrossCalls(t *testing.T) {
	first := HTML()
	second := HTML()
	if !bytes.Equal(first, second) {
		t.Fatal("HTML() returned different content across calls")
	}
}

package guiapp

import (
	"embed"
	"encoding/json"
	"strings"
)

// The translation catalogue is shared with the frontend: the same files are
// imported by windows/frontend/src/i18n.ts at build time, so a string can only
// exist in one place and the window and the tray cannot drift apart. Adding a
// key means adding it to both files -- the frontend build fails when the two
// catalogues disagree on the key set.
//
//go:embed locales/*.json
var localeFS embed.FS

// Languages the catalogue ships with. Order matters only for documentation;
// the first entry is the fallback when a key is missing from another language.
var supportedLanguages = []string{"zh", "en"}

const fallbackLanguage = "zh"

var localeTables = mustLoadLocales()

func mustLoadLocales() map[string]map[string]string {
	out := make(map[string]map[string]string, len(supportedLanguages))
	for _, lang := range supportedLanguages {
		data, err := localeFS.ReadFile("locales/" + lang + ".json")
		if err != nil {
			panic("guiapp: read locale " + lang + ": " + err.Error())
		}
		var table map[string]string
		if err := json.Unmarshal(data, &table); err != nil {
			panic("guiapp: parse locale " + lang + ": " + err.Error())
		}
		out[lang] = table
	}
	return out
}

// normalizeLang maps anything the frontend or the settings file may contain
// onto a language the catalogue knows. An unknown value falls back to Chinese,
// which is also what a settings file written before language support existed
// means.
func normalizeLang(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	for _, l := range supportedLanguages {
		if l == lang {
			return l
		}
	}
	return fallbackLanguage
}

// tr renders one catalogue entry. Vars are substituted as {name}; a missing
// key or language degrades to Chinese, then to the key itself, so a partial
// translation is never a blank menu item.
func tr(lang, key string, vars map[string]string) string {
	s, ok := localeTables[lang][key]
	if !ok || s == "" {
		s = localeTables[fallbackLanguage][key]
	}
	if s == "" {
		return key
	}
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

// uiErrorPrefix marks an error whose message is machine readable rather than
// display text: "i18n:" followed by {"key": ..., "vars": {...}}. The window
// detects the prefix and renders the message in the user's language.
//
// Returning Chinese text from Go would mean the error banner stays Chinese in
// an English window, which is exactly the inconsistency the shared catalogue
// exists to prevent. Go only knows which message, never how to phrase it.
const uiErrorPrefix = "i18n:"

type uiError struct {
	Key  string            `json:"key"`
	Vars map[string]string `json:"vars,omitempty"`
}

func (e *uiError) Error() string {
	payload, err := json.Marshal(e)
	if err != nil {
		return uiErrorPrefix + `{"key":"err.unknown"}`
	}
	return uiErrorPrefix + string(payload)
}

// uiErr builds a translatable error. The key must exist in the catalogue.
func uiErr(key string, vars map[string]string) error {
	return &uiError{Key: key, Vars: vars}
}

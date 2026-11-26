import { ref } from "vue";
// The catalogue lives next to the Go code because the tray renders from it
// too (guiapp/i18n.go embeds the same two files). One source, two consumers:
// a label cannot exist in the window and be missing from the menu.
import zhRaw from "../../../guiapp/locales/zh.json";
import enRaw from "../../../guiapp/locales/en.json";

export type Locale = "zh" | "en";

// Every user-visible string is one of these keys. t() is typed against it, so
// a typo in a template fails the build instead of rendering the key.
export type MsgKey = keyof typeof zhRaw;
type Vars = Record<string, string | number>;

// Both directions are checked at compile time:
//  - `en: Table` rejects a catalogue that is missing a key (no half-translated
//    release can ship);
//  - _Parity rejects a key that only one language defines (no dead entries).
// This is what "no more mixed Chinese/English screens" rests on: the build
// fails rather than silently falling back to Chinese.
type SameKeys<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
type Assert<T extends true> = T;
type _Parity = Assert<SameKeys<MsgKey, keyof typeof enRaw>>;

const en: Record<MsgKey, string> = enRaw;

const tables: Record<Locale, Record<MsgKey, string>> = { zh: zhRaw, en };

const stored = localStorage.getItem("ys-lang");
const initial: Locale = stored === "en" ? "en" : "zh";

// Reactive so every t() inside a template re-renders on switch.
export const locale = ref<Locale>(initial);

export function setLocale(l: Locale) {
  locale.value = l;
  localStorage.setItem("ys-lang", l);
  document.documentElement.lang = l === "en" ? "en" : "zh-CN";
}

// A value may carry two variants separated by "|" (singular|plural), chosen by
// {n}. Only English needs it, but the mechanism is in the catalogue rather than
// in the components: "1 rows" was the kind of detail that gave away a
// translation bolted on afterwards.
function fill(raw: string, vars?: Vars): string {
  let s = raw;
  if (vars && s.includes("|")) {
    const [one, other] = s.split("|");
    s = Number(vars.n) === 1 ? one : (other ?? one);
  }
  if (vars) {
    for (const [k, v] of Object.entries(vars)) {
      s = s.replaceAll(`{${k}}`, String(v));
    }
  }
  return s;
}

// t looks the key up in the current language, then Chinese, then the key
// itself. The fallback is a safety net for a catalogue edited at runtime; with
// the typed key and the parity check above it should never be exercised.
export function t(key: MsgKey, vars?: Vars): string {
  const table = tables[locale.value] ?? zhRaw;
  const raw = table[key] ?? zhRaw[key] ?? key;
  return fill(raw, vars);
}

// hasKey tells a runtime value (a probe result key, an error code from Go)
// apart from a catalogue key without lying to the type system.
export function hasKey(key: string): key is MsgKey {
  return Object.prototype.hasOwnProperty.call(zhRaw, key);
}

// errText renders an error that crossed the binding boundary.
//
// Go reports failures the app words itself as "i18n:" + {"key","vars"} (see
// guiapp/i18n.go) so the banner speaks the user's language; anything else is
// shown verbatim, because a net error or an HTTP status is diagnostic text,
// not a sentence we can translate.
export function errText(e: unknown): string {
  const msg =
    typeof e === "string" ? e : String((e as { message?: string })?.message ?? e ?? "");
  if (!msg.startsWith("i18n:")) return msg;
  try {
    const payload = JSON.parse(msg.slice("i18n:".length)) as {
      key?: string;
      vars?: Record<string, string>;
    };
    if (payload.key && hasKey(payload.key)) {
      return t(payload.key, payload.vars);
    }
  } catch {
    // Malformed payload: fall through and show it as-is rather than nothing.
  }
  return msg;
}

setLocale(initial);

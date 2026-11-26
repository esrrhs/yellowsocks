// Shared display formatting. The three views all show byte counts and speeds,
// and a second copy of these rules is how the same number ends up looking
// different on two pages.
import { locale, t } from "./i18n";

export function fmtBytes(n?: number | null): string {
  if (!n || n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 2)} ${units[i]}`;
}

export function fmtBps(n?: number | null): string {
  return `${fmtBytes(n)}/s`;
}

// fmtAge renders a session age given in milliseconds. Values are coarse on
// purpose: a connection list refreshing every second does not need millisecond
// precision, and the short forms keep the column narrow. The unit suffixes are
// abbreviations of seconds/minutes/hours/days, identical in both languages.
export function fmtAge(ms?: number | null): string {
  if (!ms || ms <= 0) return "0s";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${s % 60 ? ` ${s % 60}s` : ""}`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h${m % 60 ? ` ${m % 60}m` : ""}`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

// fmtUptime is the dashboard variant: it spells the units out, so they come
// from the catalogue ("2天3时" / "2d 3h") instead of being baked into the
// format string.
export function fmtUptime(seconds?: number | null): string {
  if (!seconds || seconds <= 0) return "-";
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (d > 0) return t("fmt.uptimeDH", { d, h });
  if (h > 0) return t("fmt.uptimeHM", { h, m });
  if (m > 0) return t("fmt.uptimeMS", { m, s });
  return t("fmt.uptimeS", { s });
}

// fmtCountry turns an ISO 3166-1 alpha-2 code into the country name in the
// current UI language. The mapping comes from Intl.DisplayNames, which the
// Chromium runtime ships: a 250-entry table in the catalogue would be a second
// source of truth that goes stale every time a country renames itself.
const regionNames = new Map<string, Intl.DisplayNames>();

export function fmtCountry(code?: string | null): string {
  const cc = (code ?? "").trim().toUpperCase();
  if (!/^[A-Z]{2}$/.test(cc)) return "";
  try {
    let dn = regionNames.get(locale.value);
    if (!dn) {
      dn = new Intl.DisplayNames([locale.value === "en" ? "en" : "zh-CN"], {
        type: "region",
      });
      regionNames.set(locale.value, dn);
    }
    return dn.of(cc) ?? cc;
  } catch {
    // Runtime without Intl.DisplayNames, or a code ICU does not know: the
    // code itself still tells the user more than an empty line.
    return cc;
  }
}

// fmtHM is the chart's x axis: hour and minute only. Five labels across a
// 900px axis have room for "23:05"; seconds would either wrap or force the
// font down to unreadable.
export function fmtHM(unix?: number | null): string {
  if (!unix) return "";
  const d = new Date(unix * 1000);
  return d.toLocaleTimeString(locale.value === "en" ? "en-GB" : "zh-CN", {
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

// fmtClock renders a unix timestamp as HH:MM:SS, which is what all three
// tables want for "when". The locale decides the format (en-GB keeps the
// 24-hour clock that a dense table needs, unlike en-US).
export function fmtClock(unix?: number | null): string {
  if (!unix) return "-";
  const d = new Date(unix * 1000);
  return d.toLocaleTimeString(locale.value === "en" ? "en-GB" : "zh-CN", {
    hour12: false,
  });
}

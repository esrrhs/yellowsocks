// Shared display formatting. The three views all show byte counts and speeds,
// and a second copy of these rules is how the same number ends up looking
// different on two pages.

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
// precision, and the short forms keep the column narrow.
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

// fmtUptime is the dashboard variant: long Chinese labels are fine there and
// read better than the compact forms above.
export function fmtUptime(seconds?: number | null): string {
  if (!seconds || seconds <= 0) return "-";
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (d > 0) return `${d}天${h}时`;
  if (h > 0) return `${h}时${m}分`;
  if (m > 0) return `${m}分${s}秒`;
  return `${s}秒`;
}

// fmtClock renders a unix timestamp as HH:MM:SS, which is what all three
// tables want for "when".
export function fmtClock(unix?: number | null): string {
  if (!unix) return "-";
  const d = new Date(unix * 1000);
  return d.toLocaleTimeString("zh-CN", { hour12: false });
}

import type { stats } from "../wailsjs/go/models";
import { baseDomain, isAddrLiteral, matchVendor } from "./vendor-map";
import type { Locale } from "./i18n";

export interface DestRow {
  id: string;
  label: string;
  bytes: number;
  up: number;
  down: number;
  members: stats.DomainStat[];
  icon?: string; // app rows: the process icon as a data URL
}

export interface DestGroup {
  total: number;
  rows: DestRow[];
}

export interface DestLabels {
  unnamed: string;
  lang: Locale;
}

// Destination grouping for the lifetime domain table: one row per vendor
// (suffix match against the vendor table), one per registrable domain for
// unlisted sites, and a pinned bucket for bare addresses. Shared by the
// Stats page and the Connections destination tab so both classify the same
// bytes the same way.
export function buildDestGroup(
  domains: stats.DomainStat[],
  kind: "direct" | "proxy",
  labels: DestLabels,
): DestGroup {
  const bytesOf = (d: stats.DomainStat) =>
    kind === "direct" ? (d.direct_bytes ?? 0) : (d.proxy_bytes ?? 0);

  const groups = new Map<
    string,
    { label: string; members: stats.DomainStat[]; bytes: number; up: number; down: number }
  >();
  let total = 0;

  for (const d of domains) {
    const b = bytesOf(d);
    if (b <= 0) continue;
    const addr = isAddrLiteral(d.domain);
    const vendor = addr ? undefined : matchVendor(d.domain);
    // Unlisted sites group by their registrable domain — baidu.com.cn, not
    // a pile of anonymous hosts — so the leftovers read as a ranking.
    const base = addr ? "" : baseDomain(d.domain);
    const id = addr ? "@addr" : vendor ? vendor.id : "d:" + base;
    const label = addr
      ? labels.unnamed
      : vendor
        ? labels.lang === "zh"
          ? vendor.zh
          : vendor.en
        : base;

    // Per-direction numbers are stored per entry, not per decision; when an
    // entry rode both paths, scale them by the share this side owns.
    const whole = (d.upload_bytes ?? 0) + (d.download_bytes ?? 0);
    const up = whole > 0 ? ((d.upload_bytes ?? 0) * b) / whole : 0;
    const down = whole > 0 ? ((d.download_bytes ?? 0) * b) / whole : 0;

    const g = groups.get(id);
    if (g) {
      g.members.push(d);
      g.bytes += b;
      g.up += up;
      g.down += down;
    } else {
      groups.set(id, { label, members: [d], bytes: b, up, down });
    }
    total += b;
  }

  // Everything ranks by traffic, largest first; the unrecognised-address
  // bucket pins last, since it is not a destination of its own.
  const rows = [...groups.entries()]
    .map(([id, g]): DestRow => ({
      id,
      label: g.label,
      bytes: g.bytes,
      up: g.up,
      down: g.down,
      members: g.members,
    }))
    .sort((a, b) => {
      const ra = a.id === "@addr" ? 1 : 0;
      const rb = b.id === "@addr" ? 1 : 0;
      return ra - rb || b.bytes - a.bytes;
    });

  return { total, rows };
}

// Live variant of the breakdown, fed by the active session list instead of
// the lifetime domain table. Classification is identical (same vendor table,
// same base-domain split, same bare-address bucket); only the numbers differ:
// every entry is a connection that exists right now, so the rates are the
// current samples and the byte counts cover those connections' lifetime.
export interface LiveDestMember {
  name: string;
  upBps: number;
  downBps: number;
  bytes: number;
}

export interface LiveDestRow {
  id: string;
  label: string;
  upBps: number;
  downBps: number;
  bytes: number;
  up: number;
  down: number;
  members: LiveDestMember[];
}

export interface LiveDestGroup {
  upBps: number;
  downBps: number;
  rows: LiveDestRow[];
}

export function buildLiveDestGroup(
  sessions: stats.SessionView[],
  kind: "direct" | "proxy",
  labels: DestLabels,
): LiveDestGroup {
  const groups = new Map<string, LiveDestRow>();
  const membersOf = new Map<string, Map<string, LiveDestMember>>();
  let upBps = 0;
  let downBps = 0;

  for (const s of sessions) {
    if (s.decision !== kind) continue;
    // Same key as the collector's lifetime table: the sniffed name when there
    // is one, the connected host otherwise.
    const name = (s.domain || s.host || "").trim();
    if (!name) continue;
    const addr = isAddrLiteral(name);
    const vendor = addr ? undefined : matchVendor(name);
    const base = addr ? "" : baseDomain(name);
    const id = addr ? "@addr" : vendor ? vendor.id : "d:" + base;
    const label = addr
      ? labels.unnamed
      : vendor
        ? labels.lang === "zh"
          ? vendor.zh
          : vendor.en
        : base;

    const ru = s.upload_bps ?? 0;
    const rd = s.download_bps ?? 0;
    const up = s.upload_bytes ?? 0;
    const down = s.download_bytes ?? 0;

    let row = groups.get(id);
    if (!row) {
      row = {
        id,
        label,
        upBps: 0,
        downBps: 0,
        bytes: 0,
        up: 0,
        down: 0,
        members: [],
      };
      groups.set(id, row);
      membersOf.set(id, new Map());
    }
    row.upBps += ru;
    row.downBps += rd;
    row.up += up;
    row.down += down;
    row.bytes += up + down;

    const members = membersOf.get(id)!;
    let m = members.get(name);
    if (!m) {
      m = { name, upBps: 0, downBps: 0, bytes: 0 };
      members.set(name, m);
    }
    m.upBps += ru;
    m.downBps += rd;
    m.bytes += up + down;

    upBps += ru;
    downBps += rd;
  }

  // Busiest destination first: this view answers "who is using the link now",
  // and the byte total is the tiebreak for connections that have gone quiet.
  const byRate = (
    a: { upBps: number; downBps: number; bytes: number },
    b: { upBps: number; downBps: number; bytes: number },
  ) =>
    b.upBps + b.downBps - (a.upBps + a.downBps) ||
    b.bytes - a.bytes;

  const rows = [...groups.values()];
  for (const row of rows) {
    row.members = [...membersOf.get(row.id)!.values()].sort(byRate);
  }
  // Same pinned catch-all as the lifetime view: bare addresses are a summary
  // of what was not named, not a destination of their own.
  rows.sort((a, b) => {
    const ra = a.id === "@addr" ? 1 : 0;
    const rb = b.id === "@addr" ? 1 : 0;
    return ra - rb || byRate(a, b);
  });

  return { upBps, downBps, rows };
}

export function destShare(bytes: number, total: number): string {
  if (total <= 0) return "";
  return `${Math.round((bytes / total) * 100)}%`;
}

// The expanded list shows the vendor's own entries, biggest first, capped:
// a CDN vendor can own hundreds of hostnames and the box must not grow with
// them. The count line tells the user there is more.
export function topDestMembers(
  members: stats.DomainStat[],
  bytesOf: (d: stats.DomainStat) => number,
): stats.DomainStat[] {
  return [...members].sort((a, b) => bytesOf(b) - bytesOf(a)).slice(0, 8);
}
<script lang="ts" setup>
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref } from "vue";
import { GetOverview, GetProcesses, GetSessions } from "../../wailsjs/go/guiapp/App";
import { stats } from "../../wailsjs/go/models";
import { fmtBps, fmtBytes, fmtAge } from "../format";
import { buildLiveDestGroup } from "../dest-group";
import { locale, t } from "../i18n";

type Tab = "conns" | "apps" | "dest";

const tab = ref<Tab>("conns");
const sessions = ref<stats.SessionView[]>([]);
// The list comes back capped, so this is the collector's own count. Showing it
// as the denominator keeps "200 / 320" honest instead of pretending the table
// holds everything.
const liveTotal = ref(0);
const processes = ref<stats.ProcessStat[]>([]);
const query = ref("");
// UDP associations are no longer recorded (UDP relay is direct-only and
// unattributed), so the filter is just the routing decision.
const decision = ref<"all" | "proxy" | "direct">("all");

// Every column sorts. Numeric columns lead with the live rate (that is the
// number the cell leads with) and fall back to the cumulative total, so rows
// with an idle connection still land in a sensible place; text columns sort
// A→Z. Descending first for numbers because "who is using my bandwidth" is the
// question this page answers.
type SortKey = "host" | "client" | "decision" | "node" | "up" | "down" | "age";
const sortKey = ref<SortKey>("down");
const sortDir = ref<"asc" | "desc">("desc");

const isTextSort = (key: SortKey) =>
  key === "host" || key === "client" || key === "decision" || key === "node";

function toggleSort(key: SortKey) {
  if (sortKey.value === key) {
    sortDir.value = sortDir.value === "desc" ? "asc" : "desc";
    return;
  }
  sortKey.value = key;
  sortDir.value = isTextSort(key) ? "asc" : "desc";
}

let timer: number | undefined;

async function refresh() {
  // Each binding returns nil when the engine is stopped, which is also what an
  // empty list looks like here, so the tables just go blank.
  try {
    if (tab.value === "conns") {
      const [list, ov] = await Promise.all([GetSessions(), GetOverview()]);
      sessions.value = list ?? [];
      liveTotal.value = ov?.active_conns ?? sessions.value.length;
    } else if (tab.value === "apps") {
      processes.value = (await GetProcesses()) ?? [];
    } else {
      sessions.value = (await GetSessions()) ?? [];
    }
  } catch {
    // Engine stopped between the switch and the call; the next tick recovers.
  }
}

function switchTab(next: Tab) {
  if (tab.value === next) return;
  tab.value = next;
  // All three tabs read live data now, so one clock serves them all; the
  // first refresh below reseeds the view for the tab that just became visible.
  stopPolling();
  startPolling();
  void refresh();
}

// Per-filter totals, so the chips visibly add up to "all". A list that says
// 130 while proxy+direct only reach 46 reads like a broken filter.
const counts = computed(() => {
  let proxy = 0;
  let direct = 0;
  for (const s of sessions.value) {
    if (s.decision === "proxy") proxy++;
    else if (s.decision === "direct") direct++;
  }
  return { all: sessions.value.length, proxy, direct };
});

// Never below the rows we actually hold: a stale overview must not make the
// denominator smaller than the visible list.
const totalCount = computed(() => Math.max(sessions.value.length, liveTotal.value));

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase();
  return sessions.value.filter((s) => {
    if (decision.value !== "all" && s.decision !== decision.value) {
      return false;
    }
    if (!q) return true;
    return (
      (s.host ?? "").toLowerCase().includes(q) ||
      (s.domain ?? "").toLowerCase().includes(q) ||
      (s.client ?? "").toLowerCase().includes(q) ||
      (s.process_name ?? "").toLowerCase().includes(q) ||
      (s.node ?? "").toLowerCase().includes(q)
    );
  });
});

// filtered (search + route) first, then ordered by the chosen column.
const visible = computed(() => {
  const rows = filtered.value.slice();
  const key = sortKey.value;
  const dir = sortDir.value === "desc" ? -1 : 1;
  rows.sort((a, b) => {
    let d = 0;
    if (key === "host") {
      d = (a.domain || a.host || "").localeCompare(b.domain || b.host || "");
    } else if (key === "client") {
      // The cell shows the owning process when it is known, the raw client
      // address otherwise; sorting follows what is on screen.
      d = (a.process_name || a.client || "").localeCompare(b.process_name || b.client || "");
    } else if (key === "decision") {
      d = (a.decision ?? "").localeCompare(b.decision ?? "");
    } else if (key === "node") {
      d = (a.node ?? "").localeCompare(b.node ?? "");
    } else if (key === "up") {
      d = (a.upload_bps ?? 0) - (b.upload_bps ?? 0) ||
        (a.upload_bytes ?? 0) - (b.upload_bytes ?? 0);
    } else if (key === "down") {
      d = (a.download_bps ?? 0) - (b.download_bps ?? 0) ||
        (a.download_bytes ?? 0) - (b.download_bytes ?? 0);
    } else {
      d = (a.age_ms ?? 0) - (b.age_ms ?? 0);
    }
    // Ties (and the idle majority) fall back to connection id: without it the
    // rows reshuffle on every one-second refresh.
    if (d === 0) return (a.id ?? 0) - (b.id ?? 0);
    return d * dir;
  });
  return rows;
});

// Application tab sorting: rate columns lead (same "who is using bandwidth"
// question as the connections table), and the cumulative size columns sort
// too — including the domestic/foreign split, which ranks by that side's live
// rate with its cumulative bytes as the tiebreaker.
type AppSortKey = "name" | "upRate" | "downRate" | "total" | "domestic" | "foreign" | "conns";
const appSortKey = ref<AppSortKey>("downRate");
const appSortDir = ref<"asc" | "desc">("desc");

function toggleAppSort(key: AppSortKey) {
  if (appSortKey.value === key) {
    appSortDir.value = appSortDir.value === "desc" ? "asc" : "desc";
    return;
  }
  appSortKey.value = key;
  appSortDir.value = key === "name" ? "asc" : "desc";
}

const appTotal = (a: stats.ProcessStat) => (a.upload_bytes ?? 0) + (a.download_bytes ?? 0);

const visibleApps = computed(() => {
  const rows = processes.value.slice();
  const key = appSortKey.value;
  const dir = appSortDir.value === "desc" ? -1 : 1;
  rows.sort((a, b) => {
    let d = 0;
    if (key === "name") {
      d = (a.name ?? "").localeCompare(b.name ?? "");
    } else if (key === "upRate") {
      d = (a.upload_bps ?? 0) - (b.upload_bps ?? 0);
    } else if (key === "downRate") {
      d = (a.download_bps ?? 0) - (b.download_bps ?? 0);
    } else if (key === "total") {
      d = appTotal(a) - appTotal(b);
    } else if (key === "domestic") {
      d = (a.direct_upload_bps ?? 0) + (a.direct_download_bps ?? 0) -
          ((b.direct_upload_bps ?? 0) + (b.direct_download_bps ?? 0)) ||
        (a.direct_bytes ?? 0) - (b.direct_bytes ?? 0);
    } else if (key === "foreign") {
      d = (a.proxy_upload_bps ?? 0) + (a.proxy_download_bps ?? 0) -
          ((b.proxy_upload_bps ?? 0) + (b.proxy_download_bps ?? 0)) ||
        (a.proxy_bytes ?? 0) - (b.proxy_bytes ?? 0);
    } else {
      d = (a.conns ?? 0) - (b.conns ?? 0);
    }
    // Stable tie-break on the name so idle rows do not reshuffle every second.
    if (d === 0) return (a.name ?? "").localeCompare(b.name ?? "");
    return d * dir;
  });
  return rows;
});

function decisionLabel(d: string): string {
  if (d === "proxy") return t("conn.proxy");
  return t("conn.direct");
}

// Hostnames are longer than the column and have no spaces, so the browser
// used to break them mid-word ("metam ask.io"). A zero-width space after each
// separator gives it places to break that keep the name readable.
function brk(s?: string): string {
  return (s ?? "").replace(/[.:-]/g, (m) => m + "\u200b");
}

// Destination tab: the same classification as the Stats page's 按目标 view
// (shared dest-group.ts), fed by the active session list rather than the
// lifetime domain table. Every row is a destination that is connected right
// now, so the panel shows its live rates and the bytes those connections have
// carried; nothing here survives the connection.
const destSide = ref<"direct" | "proxy">("direct");
const destDirect = computed(() =>
  buildLiveDestGroup(sessions.value, "direct", {
    unnamed: t("stats.unnamed"),
    lang: locale.value,
  }),
);
const destProxy = computed(() =>
  buildLiveDestGroup(sessions.value, "proxy", {
    unnamed: t("stats.unnamed"),
    lang: locale.value,
  }),
);
const destGroupOf = (side: "direct" | "proxy") =>
  side === "direct" ? destDirect.value : destProxy.value;
const destGroup = computed(() => destGroupOf(destSide.value));
const hasDestData = computed(() => sessions.value.length > 0);

// Ordering for the destination rows. Every visible column sorts: the name, the
// live rates split by direction, the combined rate the view defaults to, and
// the cumulative bytes. The bare-address bucket is a summary of what was not
// named, not a destination of its own, so it stays pinned last whatever the
// order (the shared grouping already does this for its own default).
type DestSortKey = "rate" | "up" | "down" | "sum" | "name";
const destSortKey = ref<DestSortKey>("rate");
const destSortDir = ref<"asc" | "desc">("desc");

function toggleDestSort(key: DestSortKey) {
  if (destSortKey.value === key) {
    destSortDir.value = destSortDir.value === "desc" ? "asc" : "desc";
    return;
  }
  destSortKey.value = key;
  destSortDir.value = key === "name" ? "asc" : "desc";
}

const destRows = computed(() => {
  const rows = destGroup.value.rows.slice();
  const key = destSortKey.value;
  const dir = destSortDir.value === "desc" ? -1 : 1;
  rows.sort((a, b) => {
    const ra = a.id === "@addr" ? 1 : 0;
    const rb = b.id === "@addr" ? 1 : 0;
    if (ra !== rb) return ra - rb;
    let d = 0;
    if (key === "name") {
      d = a.label.localeCompare(b.label);
    } else if (key === "up") {
      d = a.upBps - b.upBps;
    } else if (key === "down") {
      d = a.downBps - b.downBps;
    } else if (key === "sum") {
      d = a.bytes - b.bytes;
    } else {
      d = a.upBps + a.downBps - (b.upBps + b.downBps) || a.bytes - b.bytes;
    }
    if (d === 0) return a.label.localeCompare(b.label);
    return d * dir;
  });
  return rows;
});

const destExpanded = ref(new Set<string>());
function toggleDestRow(id: string) {
  const key = `${destSide.value}:${id}`;
  const next = new Set(destExpanded.value);
  if (next.has(key)) next.delete(key);
  else next.add(key);
  destExpanded.value = next;
}
const destOpen = (id: string) => destExpanded.value.has(`${destSide.value}:${id}`);

// Kept alive across tab switches (see App.vue), so polling pauses while the
// page is hidden and resumes with a fresh sample the moment it is shown --
// otherwise the table would come back with a one-second-old (or, after a
// tray-hidden window, much older) set of rows.
let booted = false;

function startPolling() {
  if (timer) return;
  timer = window.setInterval(refresh, 1000);
}

function stopPolling() {
  if (timer) window.clearInterval(timer);
  timer = undefined;
}

function onVisibility() {
  if (document.visibilityState === "visible") void refresh();
}

onMounted(async () => {
  document.addEventListener("visibilitychange", onVisibility);
  await refresh();
  booted = true;
});

onActivated(() => {
  if (booted) void refresh();
  startPolling();
});

onDeactivated(stopPolling);

onUnmounted(() => {
  stopPolling();
  document.removeEventListener("visibilitychange", onVisibility);
});
</script>

<template>
  <section class="page">
    <div class="tabs">
      <button
        class="tab"
        :class="{ active: tab === 'conns' }"
        @click="switchTab('conns')"
      >
        {{ t("conn.tabConns") }}
      </button>
      <button
        class="tab"
        :class="{ active: tab === 'apps' }"
        @click="switchTab('apps')"
      >
        {{ t("conn.tabApps") }}
      </button>
      <button
        class="tab"
        :class="{ active: tab === 'dest' }"
        @click="switchTab('dest')"
      >
        {{ t("conn.tabDest") }}
      </button>
    </div>

    <template v-if="tab === 'conns'">
      <div class="toolbar">
        <input v-model="query" class="search" :placeholder="t('conn.search')" />
        <div class="seg">
          <button :class="{ active: decision === 'all' }" @click="decision = 'all'">
            {{ t("conn.all") }}<span class="seg-count">{{ counts.all }}</span>
          </button>
          <button
            :class="{ active: decision === 'proxy' }"
            @click="decision = 'proxy'"
          >
            {{ t("conn.proxy") }}<span class="seg-count">{{ counts.proxy }}</span>
          </button>
          <button
            :class="{ active: decision === 'direct' }"
            @click="decision = 'direct'"
          >
            {{ t("conn.direct") }}<span class="seg-count">{{ counts.direct }}</span>
          </button>
        </div>
        <span class="toolbar-meta">
          {{ t("conn.count", { n: filtered.length, m: totalCount }) }}
        </span>
      </div>

      <div class="card table-card">
        <table class="tbl tbl-fixed">
          <colgroup>
            <col style="width: 29%" />
            <col style="width: 16%" />
            <col style="width: 9%" />
            <col style="width: 8%" />
            <col style="width: 12%" />
            <col style="width: 16%" />
            <col style="width: 10%" />
          </colgroup>
          <thead>
            <tr>
              <th class="sortable" @click="toggleSort('host')">
                {{ t("conn.colHost") }}
                <span class="sort-mark" v-if="sortKey === 'host'">{{
                  sortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="sortable" @click="toggleSort('client')">
                {{ t("conn.colClient") }}
                <span class="sort-mark" v-if="sortKey === 'client'">{{
                  sortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="sortable" @click="toggleSort('decision')">
                {{ t("conn.colDecision") }}
                <span class="sort-mark" v-if="sortKey === 'decision'">{{
                  sortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="sortable" @click="toggleSort('node')">
                {{ t("conn.colNode") }}
                <span class="sort-mark" v-if="sortKey === 'node'">{{
                  sortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleSort('up')">
                {{ t("conn.colUp") }}
                <span class="sort-mark" v-if="sortKey === 'up'">{{
                  sortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleSort('down')">
                {{ t("conn.colDown") }}
                <span class="sort-mark" v-if="sortKey === 'down'">{{
                  sortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleSort('age')">
                {{ t("conn.colAge") }}
                <span class="sort-mark" v-if="sortKey === 'age'">{{
                  sortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="s in visible" :key="s.id">
              <td class="mono wrap">
                <div
                  class="host-name"
                  :title="(s.domain ? s.domain + ' → ' : '') + (s.host || '') + (s.port ? ':' + s.port : '')"
                >
                  {{ brk(s.domain || s.host) || "—"
                  }}<span v-if="!s.domain && s.port" class="port">:{{ s.port }}</span>
                </div>
                <div v-if="s.domain && s.host" class="host-addr">
                  {{ brk(s.host) }}:{{ s.port }}
                </div>
              </td>
              <td class="dim mono wrap">
                <span v-if="s.process_name" class="proc" :title="s.client">
                  <img v-if="s.process_icon" :src="s.process_icon" class="proc-icon" alt="" />
                  <span class="proc-name">{{ s.process_name }}</span>
                </span>
                <span v-else>{{ s.client || "—" }}</span>
              </td>
              <td>
                <span class="tag" :class="s.decision || 'direct'">
                  {{ decisionLabel(s.decision) }}
                </span>
              </td>
              <td class="dim wrap">{{ s.node || "—" }}</td>
              <td
                class="right num wrap"
                :title="fmtBytes(s.upload_bytes)"
              >
                <span class="up">↑{{ fmtBps(s.upload_bps) }}</span>
                <span class="total dim">{{ fmtBytes(s.upload_bytes) }}</span>
              </td>
              <td
                class="right num wrap"
                :title="fmtBytes(s.download_bytes)"
              >
                <span class="down">↓{{ fmtBps(s.download_bps) }}</span>
                <span class="total dim">{{ fmtBytes(s.download_bytes) }}</span>
              </td>
              <td class="right num dim">{{ fmtAge(s.age_ms) }}</td>
            </tr>
            <tr v-if="!visible.length">
              <td colspan="7" class="empty-row">
                {{ sessions.length ? t("conn.noMatch") : t("conn.empty") }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>

    <template v-else-if="tab === 'apps'">
      <div class="card table-card">
        <table class="tbl tbl-fixed">
          <colgroup>
            <col style="width: 26%" />
            <col style="width: 12%" />
            <col style="width: 12%" />
            <col style="width: 10%" />
            <col style="width: 15%" />
            <col style="width: 15%" />
            <col style="width: 10%" />
          </colgroup>
          <thead>
            <tr>
              <th class="sortable" @click="toggleAppSort('name')">
                {{ t("conn.colApp") }}
                <span class="sort-mark" v-if="appSortKey === 'name'">{{
                  appSortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleAppSort('upRate')">
                {{ t("conn.colUp") }}
                <span class="sort-mark" v-if="appSortKey === 'upRate'">{{
                  appSortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleAppSort('downRate')">
                {{ t("conn.colDown") }}
                <span class="sort-mark" v-if="appSortKey === 'downRate'">{{
                  appSortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleAppSort('total')">
                {{ t("conn.colTotal") }}
                <span class="sort-mark" v-if="appSortKey === 'total'">{{
                  appSortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleAppSort('domestic')">
                {{ t("conn.colDomestic") }}
                <span class="sort-mark" v-if="appSortKey === 'domestic'">{{
                  appSortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleAppSort('foreign')">
                {{ t("conn.colForeign") }}
                <span class="sort-mark" v-if="appSortKey === 'foreign'">{{
                  appSortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleAppSort('conns')">
                {{ t("conn.colAppConns") }}
                <span class="sort-mark" v-if="appSortKey === 'conns'">{{
                  appSortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="a in visibleApps" :key="a.name">
              <td class="mono wrap">
                <span class="proc" :title="a.name">
                  <img v-if="a.icon" :src="a.icon" class="proc-icon" alt="" />
                  <span class="proc-name">{{ a.name }}</span>
                </span>
              </td>
              <td class="right num wrap" :title="fmtBytes(a.upload_bytes)">
                <span class="up">↑{{ fmtBps(a.upload_bps) }}</span>
                <span class="total dim">{{ fmtBytes(a.upload_bytes) }}</span>
              </td>
              <td class="right num wrap" :title="fmtBytes(a.download_bytes)">
                <span class="down">↓{{ fmtBps(a.download_bps) }}</span>
                <span class="total dim">{{ fmtBytes(a.download_bytes) }}</span>
              </td>
              <td class="right num">{{ fmtBytes(appTotal(a)) }}</td>
              <td class="right num wrap" :title="fmtBytes(a.direct_bytes)">
                <span class="side-pair">
                  <span class="up">↑{{ fmtBps(a.direct_upload_bps) }}</span>
                  <span class="down">↓{{ fmtBps(a.direct_download_bps) }}</span>
                </span>
                <span class="total dim">{{ fmtBytes(a.direct_bytes) }}</span>
              </td>
              <td class="right num wrap" :title="fmtBytes(a.proxy_bytes)">
                <span class="side-pair">
                  <span class="up">↑{{ fmtBps(a.proxy_upload_bps) }}</span>
                  <span class="down">↓{{ fmtBps(a.proxy_download_bps) }}</span>
                </span>
                <span class="total dim">{{ fmtBytes(a.proxy_bytes) }}</span>
              </td>
              <td class="right num dim">{{ a.conns ?? 0 }}</td>
            </tr>
            <tr v-if="!visibleApps.length">
              <td colspan="7" class="empty-row">{{ t("conn.noApps") }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>

    <template v-else>
      <div class="card chart-card">
        <div class="chart-head">
          <span>{{ t("stats.title") }}</span>
        </div>
        <p class="hint">{{ t("conn.destHint") }}</p>

        <template v-if="hasDestData">
          <div class="dest-tabs">
            <button
              v-for="side in ['direct', 'proxy'] as const"
              :key="side"
              class="dest-tab"
              :class="{ active: destSide === side }"
              @click="destSide = side"
            >
              {{ side === "direct" ? t("lifetime.direct") : t("lifetime.proxy") }}
              <span class="dest-tab-total">
                <span class="up">↑{{ fmtBps(destGroupOf(side).upBps) }}</span>
                <span class="down">↓{{ fmtBps(destGroupOf(side).downBps) }}</span>
              </span>
            </button>
          </div>
          <div class="dest-sort">
            <button
              class="dest-sort-btn"
              :class="{ active: destSortKey === 'name' }"
              @click="toggleDestSort('name')"
            >
              {{ t("conn.sortName") }}
              <span class="sort-mark" v-if="destSortKey === 'name'">{{
                destSortDir === "desc" ? "▾" : "▴"
              }}</span>
            </button>
            <button
              class="dest-sort-btn"
              :class="{ active: destSortKey === 'rate' }"
              @click="toggleDestSort('rate')"
            >
              {{ t("conn.sortRate") }}
              <span class="sort-mark" v-if="destSortKey === 'rate'">{{
                destSortDir === "desc" ? "▾" : "▴"
              }}</span>
            </button>
            <button
              class="dest-sort-btn"
              :class="{ active: destSortKey === 'up' }"
              @click="toggleDestSort('up')"
            >
              ↑{{ t("conn.colUp") }}
              <span class="sort-mark" v-if="destSortKey === 'up'">{{
                destSortDir === "desc" ? "▾" : "▴"
              }}</span>
            </button>
            <button
              class="dest-sort-btn"
              :class="{ active: destSortKey === 'down' }"
              @click="toggleDestSort('down')"
            >
              ↓{{ t("conn.colDown") }}
              <span class="sort-mark" v-if="destSortKey === 'down'">{{
                destSortDir === "desc" ? "▾" : "▴"
              }}</span>
            </button>
            <button
              class="dest-sort-btn"
              :class="{ active: destSortKey === 'sum' }"
              @click="toggleDestSort('sum')"
            >
              {{ t("conn.sortSum") }}
              <span class="sort-mark" v-if="destSortKey === 'sum'">{{
                destSortDir === "desc" ? "▾" : "▴"
              }}</span>
            </button>
          </div>
          <div class="dest-side">
            <div class="dest-rows" v-if="destRows.length">
              <template v-for="row in destRows" :key="destSide + row.id">
                <button
                  class="dest-row"
                  :class="{ open: destOpen(row.id), bare: row.id.startsWith('@') }"
                  @click="toggleDestRow(row.id)"
                >
                  <span class="dest-name">{{ row.label }}</span>
                  <span class="dest-live">
                    <span class="dest-rates">
                      <span class="up">↑{{ fmtBps(row.upBps) }}</span>
                      <span class="down">↓{{ fmtBps(row.downBps) }}</span>
                    </span>
                    <span class="dest-sums">↑{{ fmtBytes(row.up) }} ↓{{ fmtBytes(row.down) }}</span>
                  </span>
                  <span class="dest-caret">{{ destOpen(row.id) ? "▾" : "▸" }}</span>
                </button>
                <div class="dest-detail" v-if="destOpen(row.id)">
                  <div
                    class="dest-member"
                    v-for="m in row.members.slice(0, 8)"
                    :key="m.name"
                  >
                    <span class="dest-member-name" :title="m.name">{{ m.name }}</span>
                    <span class="dest-member-live">
                      <span class="up">↑{{ fmtBps(m.upBps) }}</span>
                      <span class="down">↓{{ fmtBps(m.downBps) }}</span>
                    </span>
                    <span class="dest-member-bytes">{{ fmtBytes(m.bytes) }}</span>
                  </div>
                  <div class="dest-more" v-if="row.members.length > 8">
                    {{ t("stats.showDomains", { n: row.members.length }) }}
                  </div>
                </div>
              </template>
            </div>
            <div class="dest-empty" v-else>{{ t("conn.destEmpty") }}</div>
          </div>
        </template>
        <p class="hint" v-else>{{ t("conn.empty") }}</p>
      </div>
    </template>
  </section>
</template>

<script lang="ts" setup>
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref } from "vue";
import { GetErrors, GetNodes, GetOverview, GetProcesses, GetSessions } from "../../wailsjs/go/guiapp/App";
import { stats } from "../../wailsjs/go/models";
import { fmtBps, fmtBytes, fmtAge, fmtClock } from "../format";
import { t } from "../i18n";

type Tab = "conns" | "apps" | "nodes" | "errors";

const tab = ref<Tab>("conns");
const sessions = ref<stats.SessionView[]>([]);
// The list comes back capped, so this is the collector's own count. Showing it
// as the denominator keeps "200 / 320" honest instead of pretending the table
// holds everything.
const liveTotal = ref(0);
const nodes = ref<stats.NodeStat[]>([]);
const errors = ref<stats.ErrorEntry[]>([]);
const processes = ref<stats.ProcessStat[]>([]);
const query = ref("");
// UDP associations are no longer recorded (UDP relay is direct-only and
// unattributed), so the filter is just the routing decision.
const decision = ref<"all" | "proxy" | "direct">("all");

// Sorting is by the live rate (that is the number the cell leads with), with the
// cumulative total as the tiebreaker so rows with an idle connection still land
// in a sensible place. Descending first because "who is using my bandwidth" is
// the question this page answers.
type SortKey = "up" | "down" | "age";
const sortKey = ref<SortKey>("down");
const sortDir = ref<"asc" | "desc">("desc");

function toggleSort(key: SortKey) {
  if (sortKey.value === key) {
    sortDir.value = sortDir.value === "desc" ? "asc" : "desc";
    return;
  }
  sortKey.value = key;
  sortDir.value = "desc";
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
    } else if (tab.value === "nodes") {
      nodes.value = (await GetNodes()) ?? [];
    } else {
      errors.value = (await GetErrors()) ?? [];
    }
  } catch {
    // Engine stopped between the switch and the call; the next tick recovers.
  }
}

function switchTab(next: Tab) {
  if (tab.value === next) return;
  tab.value = next;
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
    if (key === "up") {
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
// question as the connections table) but the cumulative size columns sort
// too, because "what moved the most today" is the other reason to open it.
type AppSortKey = "name" | "upRate" | "upTotal" | "downRate" | "downTotal" | "conns";
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
    } else if (key === "upTotal") {
      d = (a.upload_bytes ?? 0) - (b.upload_bytes ?? 0);
    } else if (key === "downRate") {
      d = (a.download_bps ?? 0) - (b.download_bps ?? 0);
    } else if (key === "downTotal") {
      d = (a.download_bytes ?? 0) - (b.download_bytes ?? 0);
    } else {
      d = (a.conns ?? 0) - (b.conns ?? 0);
    }
    // Stable tie-break on the name so idle rows do not reshuffle every second.
    if (d === 0) return (a.name ?? "").localeCompare(b.name ?? "");
    return d * dir;
  });
  return rows;
});

const nodeTotals = computed(() => {
  let up = 0;
  let down = 0;
  for (const n of nodes.value) {
    up += n.upload_bytes ?? 0;
    down += n.download_bytes ?? 0;
  }
  return { up, down };
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
        <span class="badge" v-if="totalCount">{{ totalCount }}</span>
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
        :class="{ active: tab === 'nodes' }"
        @click="switchTab('nodes')"
      >
        {{ t("conn.tabNodes") }}
      </button>
      <button
        class="tab"
        :class="{ active: tab === 'errors' }"
        @click="switchTab('errors')"
      >
        {{ t("conn.tabErrors") }}
        <span class="badge bad" v-if="errors.length">{{ errors.length }}</span>
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
              <th>{{ t("conn.colHost") }}</th>
              <th>{{ t("conn.colClient") }}</th>
              <th>{{ t("conn.colDecision") }}</th>
              <th>{{ t("conn.colNode") }}</th>
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
            <col style="width: 40%" />
            <col style="width: 13%" />
            <col style="width: 12%" />
            <col style="width: 13%" />
            <col style="width: 12%" />
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
                {{ t("conn.colUpRate") }}
                <span class="sort-mark" v-if="appSortKey === 'upRate'">{{
                  appSortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleAppSort('upTotal')">
                {{ t("conn.colUpTotal") }}
                <span class="sort-mark" v-if="appSortKey === 'upTotal'">{{
                  appSortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleAppSort('downRate')">
                {{ t("conn.colDownRate") }}
                <span class="sort-mark" v-if="appSortKey === 'downRate'">{{
                  appSortDir === "desc" ? "▾" : "▴"
                }}</span>
              </th>
              <th class="right sortable" @click="toggleAppSort('downTotal')">
                {{ t("conn.colDownTotal") }}
                <span class="sort-mark" v-if="appSortKey === 'downTotal'">{{
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
              <td class="right num wrap">
                <span class="up">↑{{ fmtBps(a.upload_bps) }}</span>
              </td>
              <td class="right num dim">{{ fmtBytes(a.upload_bytes) }}</td>
              <td class="right num wrap">
                <span class="down">↓{{ fmtBps(a.download_bps) }}</span>
              </td>
              <td class="right num dim">{{ fmtBytes(a.download_bytes) }}</td>
              <td class="right num dim">{{ a.conns ?? 0 }}</td>
            </tr>
            <tr v-if="!visibleApps.length">
              <td colspan="6" class="empty-row">{{ t("conn.noApps") }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>

    <template v-else-if="tab === 'nodes'">
      <div class="card table-card">
        <table class="tbl">
          <thead>
            <tr>
              <th>{{ t("conn.nodeTitle") }}</th>
              <th class="right">{{ t("conn.colNodeConns") }}</th>
              <th class="right">{{ t("conn.colNodeUp") }}</th>
              <th class="right">{{ t("conn.colNodeDown") }}</th>
              <th class="right">{{ t("conn.colNodeSeen") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="n in nodes" :key="n.name">
              <td class="mono">{{ n.name || t("conn.defaultNode") }}</td>
              <td class="right num">{{ n.conns ?? 0 }}</td>
              <td class="right num">{{ fmtBytes(n.upload_bytes) }}</td>
              <td class="right num">{{ fmtBytes(n.download_bytes) }}</td>
              <td class="right num dim">{{ fmtClock(n.last_seen) }}</td>
            </tr>
            <tr v-if="!nodes.length">
              <td colspan="5" class="empty-row">{{ t("conn.noNodes") }}</td>
            </tr>
            <tr v-else class="sum-row">
              <td>{{ t("conn.sum") }}</td>
              <td></td>
              <td class="right num">{{ fmtBytes(nodeTotals.up) }}</td>
              <td class="right num">{{ fmtBytes(nodeTotals.down) }}</td>
              <td></td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>

    <template v-else>
      <div class="card table-card">
        <table class="tbl">
          <thead>
            <tr>
              <th class="narrow">{{ t("conn.colTime") }}</th>
              <th>{{ t("conn.colHost") }}</th>
              <th class="narrow">{{ t("conn.colScope") }}</th>
              <th>{{ t("conn.colDecision") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(e, i) in errors" :key="`${e.t}-${i}`">
              <td class="num dim">{{ e.time || fmtClock(e.t) }}</td>
              <td class="mono ellipsis">{{ e.host || "—" }}</td>
              <td class="dim">{{ e.scope || "—" }}</td>
              <td class="err-msg">{{ e.message }}</td>
            </tr>
            <tr v-if="!errors.length">
              <td colspan="4" class="empty-row">{{ t("conn.noErrors") }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </section>
</template>

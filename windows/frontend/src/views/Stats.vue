<script lang="ts" setup>
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref } from "vue";
import {
  GetLifetimeDomains,
  GetLifetimeProcesses,
  GetLifetimeStats,
  ResetLifetimeStats,
} from "../../wailsjs/go/guiapp/App";
import { stats } from "../../wailsjs/go/models";
import { fmtBytes, fmtBps } from "../format";
import {
  buildDestGroup,
  destShare as share,
  topDestMembers as topMembers,
  type DestGroup,
  type DestRow,
} from "../dest-group";
import { errText, locale, t } from "../i18n";

const lifetime = ref<stats.LifetimeStats | null>(null);
const domains = ref<stats.DomainStat[]>([]);
const actionError = ref("");

let timer: number | undefined;

function lifetimeTotalOf(c?: stats.Counters | null): number {
  if (!c) return 0;
  return (c.upload_bytes ?? 0) + (c.download_bytes ?? 0);
}

// avgBps: the lifetime average over the accumulated on-time. Upload when
// up is true, download otherwise; zero while nothing has been measured.
function avgBps(up: boolean): string {
  const sec = lifetime.value?.active_sec ?? 0;
  if (sec <= 0) return fmtBps(0);
  const bytes = up
    ? (lifetime.value?.totals?.upload_bytes ?? 0)
    : (lifetime.value?.totals?.download_bytes ?? 0);
  return fmtBps(Math.round(bytes / sec));
}

async function refresh() {
  const [life, doms, procs] = await Promise.all([
    GetLifetimeStats().catch(() => null),
    GetLifetimeDomains(3000).catch(() => [] as stats.DomainStat[]),
    GetLifetimeProcesses().catch(() => [] as stats.ProcessStat[]),
  ]);
  // Same rule as the home page: the lifetime record keeps its numbers while
  // the engine is stopped, served from the Go-side cache.
  if (life) lifetime.value = life;
  domains.value = doms ?? [];
  processes.value = procs ?? [];
}

async function resetLifetime() {
  actionError.value = "";
  try {
    await ResetLifetimeStats();
    lifetime.value = await GetLifetimeStats();
    domains.value = await GetLifetimeDomains(3000);
    processes.value = await GetLifetimeProcesses();
  } catch (e: unknown) {
    actionError.value = errText(e);
  }
}

// ---------- destination grouping ----------
//
// Shared with the Connections page's destination tab (dest-group.ts), so
// both screens classify the same bytes the same way.

const processes = ref<stats.ProcessStat[]>([]);

// One side of the application view: rows are the attributed processes, and
// the catch-all is TCP traffic whose process could not be determined (UDP
// is relayed direct-only and stays out of this card).
function buildAppGroup(kind: "direct" | "proxy"): DestGroup {
  const bytesOf = (d: stats.ProcessStat) =>
    kind === "direct" ? (d.direct_bytes ?? 0) : (d.proxy_bytes ?? 0);

  const rows: DestRow[] = [];
  let otherBytes = 0, otherUp = 0, otherDown = 0;
  let total = 0;
  for (const d of processes.value) {
    const b = bytesOf(d);
    if (b <= 0) continue;
    total += b;
    const whole = (d.upload_bytes ?? 0) + (d.download_bytes ?? 0);
    const up = whole > 0 ? ((d.upload_bytes ?? 0) * b) / whole : 0;
    const down = whole > 0 ? ((d.download_bytes ?? 0) * b) / whole : 0;
    if (d.name) {
      rows.push({
        id: "p:" + d.name,
        label: d.name,
        bytes: b,
        up,
        down,
        members: [],
        icon: d.icon ?? "",
      });
    } else {
      otherBytes += b;
      otherUp += up;
      otherDown += down;
    }
  }
  rows.sort((a, b) => b.bytes - a.bytes);
  if (otherBytes > 0) {
    rows.push({
      id: "@app-other",
      label: t("stats.unnamed"),
      bytes: otherBytes,
      up: otherUp,
      down: otherDown,
      members: [],
    });
  }
  return { total, rows };
}

const appDirectGroup = computed(() => buildAppGroup("direct"));
const appProxyGroup = computed(() => buildAppGroup("proxy"));
const appGroupOf = (side: "direct" | "proxy") =>
  side === "direct" ? appDirectGroup.value : appProxyGroup.value;
const appActiveSide = ref<"direct" | "proxy">("direct");
const hasAppData = computed(() => processes.value.length > 0);

const destLabels = () => ({ unnamed: t("stats.unnamed"), lang: locale.value });
const directGroup = computed(() => buildDestGroup(domains.value, "direct", destLabels()));
const proxyGroup = computed(() => buildDestGroup(domains.value, "proxy", destLabels()));
const hasData = computed(() => domains.value.length > 0);
const groupOf = (side: "direct" | "proxy") => (side === "direct" ? directGroup.value : proxyGroup.value);

// One side at a time: 国内 and 国外 are separate views, not two stacked
// blocks — the card stays half as tall and the ranking reads on its own.
const activeSide = ref<"direct" | "proxy">("direct");

// The page's top-level tabs: totals, destination breakdown, application
// breakdown. Each keeps its own 国内/国外 sub-tabs inside.
const pageTab = ref<"totals" | "dest" | "app">("totals");

const expanded = ref(new Set<string>());
function toggleRow(side: string, id: string) {
  const key = `${side}:${id}`;
  const next = new Set(expanded.value);
  if (next.has(key)) next.delete(key);
  else next.add(key);
  expanded.value = next;
}
const isOpen = (side: string, id: string) => expanded.value.has(`${side}:${id}`);

// Poll slower than the home page: the lifetime record moves at human speed,
// and every read re-sorts a table of up to a few thousand entries.
function startPolling() {
  if (timer) return;
  timer = window.setInterval(refresh, 2000);
}
function stopPolling() {
  if (timer) window.clearInterval(timer);
  timer = undefined;
}

let booted = false;

onMounted(async () => {
  await refresh();
  booted = true;
});

// The view survives tab switches (KeepAlive), so polling pauses off-screen
// and resumes with a fresh sample on the way back in.
onActivated(() => {
  if (booted) void refresh();
  startPolling();
});

onDeactivated(stopPolling);

onUnmounted(() => {
  stopPolling();
});
</script>

<template>
  <section class="page">
    <div v-if="actionError" class="error-banner">{{ actionError }}</div>

    <div class="stats-tabs">
      <button class="stats-tab" :class="{ active: pageTab === 'totals' }" @click="pageTab = 'totals'">
        {{ t("stats.tabTotals") }}
      </button>
      <button class="stats-tab" :class="{ active: pageTab === 'dest' }" @click="pageTab = 'dest'">
        {{ t("stats.title") }}
      </button>
      <button class="stats-tab" :class="{ active: pageTab === 'app' }" @click="pageTab = 'app'">
        {{ t("stats.appTitle") }}
      </button>
    </div>

    <div class="card chart-card" v-if="pageTab === 'totals'">
      <div class="chart-head">
        <span>{{ t("lifetime.title") }}</span>
        <button class="btn small" @click="resetLifetime">{{ t("lifetime.reset") }}</button>
      </div>
      <div class="lifetime-grid">
        <div class="lt-item">
          <div class="lt-label lt-cn">{{ t("lifetime.direct") }}</div>
          <div class="lt-value">{{ fmtBytes(lifetimeTotalOf(lifetime?.direct)) }}</div>
          <div class="lt-sub">
            ↑ {{ fmtBytes(lifetime?.direct?.upload_bytes) }} · ↓
            {{ fmtBytes(lifetime?.direct?.download_bytes) }}
          </div>
        </div>
        <div class="lt-item">
          <div class="lt-label lt-foreign">{{ t("lifetime.proxy") }}</div>
          <div class="lt-value">{{ fmtBytes(lifetimeTotalOf(lifetime?.proxy)) }}</div>
          <div class="lt-sub">
            ↑ {{ fmtBytes(lifetime?.proxy?.upload_bytes) }} · ↓
            {{ fmtBytes(lifetime?.proxy?.download_bytes) }}
          </div>
        </div>
        <div class="lt-item">
          <div class="lt-label">{{ t("lifetime.total") }}</div>
          <div class="lt-value">{{ fmtBytes(lifetimeTotalOf(lifetime?.totals)) }}</div>
          <div class="lt-sub">
            ↑ {{ fmtBytes(lifetime?.totals?.upload_bytes) }} · ↓
            {{ fmtBytes(lifetime?.totals?.download_bytes) }}
          </div>
        </div>
      </div>

      <div class="lifetime-grid">
        <div class="lt-item">
          <div class="lt-label">{{ t("stats.metricPeak") }}</div>
          <div class="lt-value small">↑ {{ fmtBps(lifetime?.peak_up) }} · ↓ {{ fmtBps(lifetime?.peak_down) }}</div>
        </div>
        <div class="lt-item">
          <div class="lt-label">{{ t("stats.metricAvg") }}</div>
          <div class="lt-value small">↑ {{ avgBps(true) }} · ↓ {{ avgBps(false) }}</div>
        </div>
        <div class="lt-item">
          <div class="lt-label">{{ t("stats.metricMaxConns") }}</div>
          <div class="lt-value small">{{ lifetime?.max_conns ?? 0 }}</div>
          <div class="lt-sub">{{ t("stats.metricMaxConnsNote") }}</div>
        </div>
      </div>
    </div>

    <div class="card chart-card" v-else-if="pageTab === 'dest'">
      <div class="chart-head">
        <span>{{ t("stats.title") }}</span>
      </div>
      <p class="hint">{{ t("stats.hint") }}</p>

      <template v-if="hasData">
        <div class="dest-tabs">
          <button
            v-for="side in ['direct', 'proxy'] as const"
            :key="side"
            class="dest-tab"
            :class="{ active: activeSide === side }"
            @click="activeSide = side"
          >
            {{ side === "direct" ? t("lifetime.direct") : t("lifetime.proxy") }}
            <span class="dest-tab-total">{{ fmtBytes(groupOf(side).total) }}</span>
          </button>
        </div>
        <div class="dest-side">
          <div class="dest-rows">
            <template v-for="row in groupOf(activeSide).rows" :key="activeSide + row.id">
              <button
                class="dest-row"
                :class="{ open: isOpen(activeSide, row.id), bare: row.id.startsWith('@') }"
                @click="toggleRow(activeSide, row.id)"
              >
                <span class="dest-name">{{ row.label }}</span>
                <span class="dest-share">{{ share(row.bytes, groupOf(activeSide).total) }}</span>
                <span class="dest-bytes">{{ fmtBytes(row.bytes) }}</span>
                <span class="dest-updown">↑{{ fmtBytes(row.up) }} ↓{{ fmtBytes(row.down) }}</span>
                <span class="dest-caret">{{ isOpen(activeSide, row.id) ? "▾" : "▸" }}</span>
              </button>
              <div class="dest-detail" v-if="isOpen(activeSide, row.id)">
                <div
                  class="dest-member"
                  v-for="m in topMembers(row.members, (d) =>
                    activeSide === 'direct' ? (d.direct_bytes ?? 0) : (d.proxy_bytes ?? 0),
                  )"
                  :key="m.domain"
                >
                  <span class="dest-member-name" :title="m.domain">{{ m.domain }}</span>
                  <span class="dest-member-bytes">{{
                    fmtBytes(activeSide === "direct" ? (m.direct_bytes ?? 0) : (m.proxy_bytes ?? 0))
                  }}</span>
                </div>
                <div class="dest-more" v-if="row.members.length > 8">
                  {{ t("stats.showDomains", { n: row.members.length }) }}
                </div>
              </div>
            </template>
          </div>
        </div>
      </template>
      <p class="hint" v-else>{{ t("stats.noData") }}</p>
    </div>

    <div class="card chart-card" v-else-if="pageTab === 'app'">
      <div class="chart-head">
        <span>{{ t("stats.appTitle") }}</span>
      </div>
      <p class="hint">{{ t("stats.appHint") }}</p>

      <template v-if="hasAppData">
        <div class="dest-tabs">
          <button
            v-for="side in ['direct', 'proxy'] as const"
            :key="side"
            class="dest-tab"
            :class="{ active: appActiveSide === side }"
            @click="appActiveSide = side"
          >
            {{ side === "direct" ? t("lifetime.direct") : t("lifetime.proxy") }}
            <span class="dest-tab-total">{{ fmtBytes(appGroupOf(side).total) }}</span>
          </button>
        </div>
        <div class="dest-side">
          <div class="dest-rows">
            <template v-for="row in appGroupOf(appActiveSide).rows" :key="appActiveSide + row.id">
              <div class="dest-row" :class="{ bare: row.id === '@app-other' }">
                <img v-if="row.icon" :src="row.icon" class="proc-icon" alt="" />
                <span class="dest-name">{{ row.label }}</span>
                <span class="dest-share">{{ share(row.bytes, appGroupOf(appActiveSide).total) }}</span>
                <span class="dest-bytes">{{ fmtBytes(row.bytes) }}</span>
                <span class="dest-updown">↑{{ fmtBytes(row.up) }} ↓{{ fmtBytes(row.down) }}</span>
                <span class="dest-caret"></span>
              </div>
            </template>
          </div>
        </div>
      </template>
      <p class="hint" v-else>{{ t("stats.appNoData") }}</p>
    </div>
  </section>
</template>

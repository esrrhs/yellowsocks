<script lang="ts" setup>
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref } from "vue";
import {
  GetLifetimeDomains,
  GetLifetimeStats,
  ResetLifetimeStats,
} from "../../wailsjs/go/guiapp/App";
import { stats } from "../../wailsjs/go/models";
import { fmtBytes } from "../format";
import { isAddrLiteral, matchVendor } from "../vendor-map";
import { errText, locale, t } from "../i18n";

const lifetime = ref<stats.LifetimeStats | null>(null);
const domains = ref<stats.DomainStat[]>([]);
const actionError = ref("");

let timer: number | undefined;

function lifetimeTotalOf(c?: stats.Counters | null): number {
  if (!c) return 0;
  return (c.upload_bytes ?? 0) + (c.download_bytes ?? 0);
}

async function refresh() {
  const [life, doms] = await Promise.all([
    GetLifetimeStats().catch(() => null),
    GetLifetimeDomains(3000).catch(() => [] as stats.DomainStat[]),
  ]);
  // Same rule as the home page: the lifetime record keeps its numbers while
  // the engine is stopped, served from the Go-side cache.
  if (life) lifetime.value = life;
  domains.value = doms ?? [];
}

async function resetLifetime() {
  actionError.value = "";
  try {
    await ResetLifetimeStats();
    lifetime.value = await GetLifetimeStats();
    domains.value = await GetLifetimeDomains(3000);
  } catch (e: unknown) {
    actionError.value = errText(e);
  }
}

// ---------- destination grouping ----------
//
// The collector reports one row per domain (or bare address) with the
// domestic/foreign byte split. Grouping happens here, in the display layer:
// suffix matching against the vendor table is presentation, not accounting,
// and keeping it in the frontend means a new vendor needs no engine change.

interface DestRow {
  id: string;
  label: string;
  bytes: number;
  up: number;
  down: number;
  members: stats.DomainStat[];
}

interface DestGroup {
  total: number;
  rows: DestRow[];
}

// One side of the split. Rows come out sorted by bytes, descending, with the
// catch-all buckets pinned to the end: 其他 for unmatched domains, 未识别
// for bare addresses (UDP datagrams, pre-SNI bytes, DoH-only clients).
function buildGroup(kind: "direct" | "proxy"): DestGroup {
  const bytesOf = (d: stats.DomainStat) =>
    kind === "direct" ? (d.direct_bytes ?? 0) : (d.proxy_bytes ?? 0);

  const groups = new Map<
    string,
    { label: string; members: stats.DomainStat[]; bytes: number; up: number; down: number }
  >();
  let total = 0;

  for (const d of domains.value) {
    const b = bytesOf(d);
    if (b <= 0) continue;
    const addr = isAddrLiteral(d.domain);
    const vendor = addr ? undefined : matchVendor(d.domain);
    const id = addr ? "@addr" : vendor ? vendor.id : "@other";
    const label = addr
      ? t("stats.unnamed")
      : vendor
        ? locale.value === "zh"
          ? vendor.zh
          : vendor.en
        : t("stats.other");

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

  // Named vendors rank by bytes; the catch-alls (其他, then 未识别地址)
  // sit below them whatever their weight, so the list reads as a ranking of
  // names with the leftovers summarised underneath.
  const rank = (id: string) => (id === "@addr" ? 2 : id === "@other" ? 1 : 0);
  const rows = [...groups.entries()]
    .map(([id, g]): DestRow => ({
      id,
      label: g.label,
      bytes: g.bytes,
      up: g.up,
      down: g.down,
      members: g.members,
    }))
    .sort((a, b) => (rank(a.id) !== rank(b.id) ? rank(a.id) - rank(b.id) : b.bytes - a.bytes));

  return { total, rows };
}

const directGroup = computed(() => buildGroup("direct"));
const proxyGroup = computed(() => buildGroup("proxy"));
const hasData = computed(() => domains.value.length > 0);
const groupOf = (side: "direct" | "proxy") => (side === "direct" ? directGroup.value : proxyGroup.value);

const expanded = ref(new Set<string>());
function toggleRow(side: string, id: string) {
  const key = `${side}:${id}`;
  const next = new Set(expanded.value);
  if (next.has(key)) next.delete(key);
  else next.add(key);
  expanded.value = next;
}
const isOpen = (side: string, id: string) => expanded.value.has(`${side}:${id}`);

// The expanded list shows the vendor's own entries, biggest first, capped:
// a CDN vendor can own hundreds of hostnames and the box must not grow with
// them. The count line tells the user there is more.
function topMembers(members: stats.DomainStat[], bytesOf: (d: stats.DomainStat) => number) {
  return [...members].sort((a, b) => bytesOf(b) - bytesOf(a)).slice(0, 8);
}

function share(bytes: number, total: number): string {
  if (total <= 0) return "";
  return `${Math.round((bytes / total) * 100)}%`;
}

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

    <div class="card chart-card">
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
    </div>

    <div class="card chart-card">
      <div class="chart-head">
        <span>{{ t("stats.title") }}</span>
      </div>
      <p class="hint">{{ t("stats.hint") }}</p>

      <template v-if="hasData">
        <div class="dest-side" v-for="side in ['direct', 'proxy'] as const" :key="side">
          <div class="dest-side-head">
            <i class="sw" :class="side === 'direct' ? 'dim' : 'dim intl'"></i>
            <span>{{ side === "direct" ? t("lifetime.direct") : t("lifetime.proxy") }}</span>
            <span class="dest-side-total">{{ fmtBytes(groupOf(side).total) }}</span>
          </div>
          <div class="dest-rows">
            <template v-for="row in groupOf(side).rows" :key="side + row.id">
              <button
                class="dest-row"
                :class="{ open: isOpen(side, row.id), bare: row.id.startsWith('@') }"
                @click="toggleRow(side, row.id)"
              >
                <span class="dest-name">{{ row.label }}</span>
                <span class="dest-share">{{ share(row.bytes, groupOf(side).total) }}</span>
                <span class="dest-bytes">{{ fmtBytes(row.bytes) }}</span>
                <span class="dest-updown">↑{{ fmtBytes(row.up) }} ↓{{ fmtBytes(row.down) }}</span>
                <span class="dest-caret">{{ isOpen(side, row.id) ? "▾" : "▸" }}</span>
              </button>
              <div class="dest-detail" v-if="isOpen(side, row.id)">
                <div
                  class="dest-member"
                  v-for="m in topMembers(row.members, (d) =>
                    side === 'direct' ? (d.direct_bytes ?? 0) : (d.proxy_bytes ?? 0),
                  )"
                  :key="m.domain"
                >
                  <span class="dest-member-name" :title="m.domain">{{ m.domain }}</span>
                  <span class="dest-member-bytes">{{
                    fmtBytes(side === "direct" ? (m.direct_bytes ?? 0) : (m.proxy_bytes ?? 0))
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
  </section>
</template>

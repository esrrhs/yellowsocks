<script lang="ts" setup>
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref } from "vue";
import {
  EngineStatus,
  GetAutoStartCore,
  GetLaunchAtLogin,
  GetLifetimeStats,
  GetOverview,
  GetProxyGuard,
  GetProxyMode,
  GetSeries,
  GetSystemProxy,
  GetTunMode,
  ResetLifetimeStats,
  SetAutoStartCore,
  SetLaunchAtLogin,
  SetProxyGuard,
  SetProxyMode,
  SetSystemProxy,
  SetTunMode,
  StartEngine,
  StopEngine,
  TestConnectivity,
} from "../../wailsjs/go/guiapp/App";
import { EventsOn } from "../../wailsjs/runtime/runtime";
import { guiapp, stats } from "../../wailsjs/go/models";
import { fmtBytes, fmtBps, fmtCountry, fmtHM, fmtUptime } from "../format";
import { errText, t, type MsgKey } from "../i18n";

const status = ref(new guiapp.EngineStatus());
const overview = ref<stats.Overview | null>(null);
const series = ref<stats.SeriesPoint[]>([]);
const actionError = ref("");
const busy = ref(false);
const autoStart = ref(false);
const launchAtLogin = ref(false);
const tunMode = ref(false);
const lifetime = ref<stats.LifetimeStats | null>(null);
const proxyMode = ref("rule");
const probe = ref<guiapp.ProbeReport | null>(null);
const probing = ref(false);
const proxyGuard = ref(false);

let timer: number | undefined;
// The view is kept alive across tab switches, so this listener is installed
// once and stays for the life of the window: the chips and the status line
// keep tracking the engine even while another page is on screen.
let offStatus: (() => void) | undefined;

// The three live calls go out together. Awaited in turn they cost three round
// trips per tick and, worse, the counters on screen lag the tick that just
// fired; in parallel the page has all of them by the time the tick ends.
async function refresh() {
  const [ov, pts, life] = await Promise.all([
    GetOverview().catch(() => null),
    GetSeries(1800).catch(() => [] as stats.SeriesPoint[]),
    GetLifetimeStats().catch(() => null),
  ]);
  if (ov) overview.value = ov;
  // A nil overview while the engine is not running is "no data", not "keep the
  // last numbers"; when it is running, a failed read means the engine is
  // restarting and the previous sample is still the best thing to show.
  else if (!status.value.running) overview.value = null;
  if (status.value.running) series.value = pts ?? [];
  else series.value = [];
  // The lifetime record is served from a cache on the Go side, so it stays
  // readable even while the engine is stopped.
  if (life) lifetime.value = life;
}

// One round trip per fact, all in flight at once. Serially awaited this was a
// visible cascade: the status dot, then the proxy chip, then 开机启动 (which
// shells out to PowerShell and took ~1s on its own), then the traffic.
async function bootstrap() {
  const [st, sysProxy, auto, login, tun, mode, guard] = await Promise.all([
    EngineStatus().catch(() => null),
    GetSystemProxy().catch(() => null),
    GetAutoStartCore().catch(() => null),
    GetLaunchAtLogin().catch(() => null),
    GetTunMode().catch(() => null),
    GetProxyMode().catch(() => null),
    GetProxyGuard().catch(() => null),
  ]);
  if (st) {
    status.value = new guiapp.EngineStatus(st);
    tunMode.value = !!(st.tun ?? tun);
    if (st.proxy_mode) proxyMode.value = st.proxy_mode;
    autoStart.value = !!(st.auto_start_core ?? auto);
    launchAtLogin.value = !!(st.launch_at_login ?? login);
    proxyGuard.value = !!(st.proxy_guard ?? guard);
  }
  if (sysProxy !== null) status.value.system_proxy = sysProxy;
  if (auto !== null) autoStart.value = auto;
  if (login !== null) launchAtLogin.value = login;
  if (tun !== null && !st) tunMode.value = tun;
  if (mode) proxyMode.value = mode;
  if (guard !== null) proxyGuard.value = guard;
  await refresh();
}

async function toggle() {
  busy.value = true;
  actionError.value = "";
  try {
    if (status.value.running) {
      await StopEngine();
    } else {
      await StartEngine();
    }
  } catch (e: any) {
    actionError.value = errText(e);
  } finally {
    busy.value = false;
    await refresh();
  }
}

async function toggleSystemProxy() {
  actionError.value = "";
  try {
    await SetSystemProxy(!status.value.system_proxy);
    // Do NOT flip the local copy: the backend emits engine:status before this
    // promise settles, and flipping on top of that delivered value undoes it
    // (the chip then shows the state it had before the click, which reads as
    // "the button does nothing"). Ask the backend what it actually applied.
    status.value.system_proxy = await GetSystemProxy();
  } catch (e: any) {
    actionError.value = errText(e);
  }
}

async function toggleAutoStart() {
  actionError.value = "";
  try {
    await SetAutoStartCore(!autoStart.value);
    autoStart.value = await GetAutoStartCore();
  } catch (e: any) {
    actionError.value = errText(e);
  }
}

async function toggleLaunchAtLogin() {
  actionError.value = "";
  try {
    await SetLaunchAtLogin(!launchAtLogin.value);
    // The OS registry is the source of truth for this one.
    launchAtLogin.value = await GetLaunchAtLogin();
  } catch (e: any) {
    actionError.value = errText(e);
  }
}

async function toggleTunMode() {
  actionError.value = "";
  try {
    const next = !tunMode.value;
    await SetTunMode(next);
    tunMode.value = next;
  } catch (e: any) {
    actionError.value = errText(e);
  }
}

async function resetLifetime() {
  actionError.value = "";
  try {
    await ResetLifetimeStats();
    lifetime.value = await GetLifetimeStats();
  } catch (e: any) {
    actionError.value = errText(e);
  }
}

async function toggleProxyGuard() {
  actionError.value = "";
  try {
    await SetProxyGuard(!proxyGuard.value);
    proxyGuard.value = await GetProxyGuard();
  } catch (e: any) {
    actionError.value = errText(e);
  }
}

async function setMode(mode: string) {
  actionError.value = "";
  try {
    await SetProxyMode(mode);
    // Read back rather than assume: the backend normalises unknown values and
    // emits the status event at the same time.
    proxyMode.value = await GetProxyMode();
    proxyGuard.value = await GetProxyGuard();
  } catch (e: any) {
    actionError.value = errText(e);
  }
}

async function runProbe() {
  probing.value = true;
  actionError.value = "";
  try {
    probe.value = await TestConnectivity();
  } catch (e: any) {
    actionError.value = errText(e);
  } finally {
    probing.value = false;
  }
}

// The probe report carries no display text: Go reports which row it is (key)
// and which sentence applies (detail_key), and the wording lives here, in both
// languages. Raw details (a Go net error, an HTTP status) are shown verbatim
// because they are diagnostics, not copy.
const probeNames: Record<string, MsgKey> = {
  direct: "probe.item.direct",
  proxied: "probe.item.proxied",
  upstream: "probe.item.upstream",
  exitip: "probe.item.exitip",
  engine: "probe.item.engine",
};
const probeDetails: Record<string, MsgKey> = {
  engine_stopped: "probe.detail.engineStopped",
  not_configured: "probe.detail.notConfigured",
  tcp_ok: "probe.detail.tcpOk",
  unparsed: "probe.detail.unparsed",
};

// "United States · Los Angeles" / "美国 · 洛杉矶". The country is localised
// from the code the probe returned; the city is the provider's own spelling
// and stays untranslated, like the address next to it.
const exitPlace = computed(() => {
  const p = probe.value;
  if (!p) return "";
  const country = fmtCountry(p.exit_country);
  const city = (p.exit_city ?? "").trim();
  if (country && city) return `${country} · ${city}`;
  return country || city;
});

// How much of the current speed is domestic, in percent. The bar is a split
// of one direction, so both halves are the same hue and the foreign part is
// just dimmer -- purple here never means "download", which is what the colour
// means everywhere else on this page.
function share(direct?: number | null, proxy?: number | null): number {
  const d = direct ?? 0;
  const p = proxy ?? 0;
  if (d + p <= 0) return 0;
  return Math.round((d / (d + p)) * 100);
}

const upShare = computed(() =>
  share(overview.value?.rate?.direct_upload_bps, overview.value?.rate?.proxy_upload_bps),
);
const downShare = computed(() =>
  share(overview.value?.rate?.direct_download_bps, overview.value?.rate?.proxy_download_bps),
);
const connShare = computed(() =>
  share(overview.value?.active_direct, overview.value?.active_proxy),
);
// UDP associations carry no single decision (each datagram is routed on its
// own), so they show up in the total but in neither half. Naming the
// remainder keeps "104 connections" from looking like broken arithmetic
// next to "domestic 9 / foreign 43".
const connOther = computed(() => {
  const total = overview.value?.active_conns ?? 0;
  const split = (overview.value?.active_direct ?? 0) + (overview.value?.active_proxy ?? 0);
  return Math.max(0, total - split);
});

// True when a domestic/foreign pair carries any value at all. Shared by the
// speed cards and the connection card: an empty pair must render the plain
// track, never a full-width bar that reads as "everything is foreign".
function hasSplit(direct?: number | null, proxy?: number | null): boolean {
  return (direct ?? 0) + (proxy ?? 0) > 0;
}

function probeName(it: guiapp.ProbeItem): string {
  const key = probeNames[it.key];
  return key ? t(key) : it.key;
}

function probeDetail(it: guiapp.ProbeItem): string {
  const key = it.detail_key ? probeDetails[it.detail_key] : undefined;
  return key ? t(key) : it.detail || "";
}

function lifetimeTotal(c?: stats.Counters | null): number {
  if (!c) return 0;
  return (c.upload_bytes ?? 0) + (c.download_bytes ?? 0);
}

// Set once the first snapshot has landed, so the activation that coincides
// with the first mount does not race the bootstrap with an early read.
let booted = false;

function startPolling() {
  if (timer) return;
  timer = window.setInterval(refresh, 1000);
}

function stopPolling() {
  if (timer) window.clearInterval(timer);
  timer = undefined;
}

// WebView2 throttles timers while the window sits hidden in the tray, so the
// first tick after it is shown again can be up to a second late and the
// counters would sit on the stale sample. Re-poll the moment the page is
// visible instead of waiting for that tick.
function onVisibility() {
  if (document.visibilityState === "visible") void refresh();
}

onMounted(async () => {
  offStatus = EventsOn("engine:status", (st: guiapp.EngineStatus) => {
    status.value = new guiapp.EngineStatus(st);
    tunMode.value = !!st.tun;
    if (st.proxy_mode) proxyMode.value = st.proxy_mode;
    // The option chips ride along on the event now, so nothing has to ask for
    // them again: each ask was a round trip, and the launch-at-login one was a
    // PowerShell launch -- that is what kept the chip dark for a second.
    autoStart.value = !!st.auto_start_core;
    launchAtLogin.value = !!st.launch_at_login;
    proxyGuard.value = !!st.proxy_guard;
    if (!st.running) {
      overview.value = null;
      series.value = [];
    }
  });
  document.addEventListener("visibilitychange", onVisibility);
  await bootstrap();
  booted = true;
});

// The view is kept alive across tab switches, so polling has to be paused on
// the way out (no point sampling a page nobody is looking at) and resumed with
// an immediate sample on the way in, rather than a one-second-old one.
onActivated(() => {
  if (booted) void refresh();
  startPolling();
});

onDeactivated(stopPolling);

onUnmounted(() => {
  stopPolling();
  document.removeEventListener("visibilitychange", onVisibility);
  offStatus?.();
  offStatus = undefined;
});

// Traffic chart. Four polylines normalised against one shared ceiling, drawn
// in a 0..100 viewBox and stretched by CSS; colour stays "direction" (upload
// blue, download purple, as in the stat cards) and the dashed line is the
// foreign share of the same direction, so a pair reads as one quantity split
// in two. Keeping one ceiling for all four makes the dashed line exactly
// comparable to its solid twin.
interface ChartTick {
  pos: number;
  label: string;
}

const Y_DIVS = 4;
const X_DIVS = 4;

// Axis ceiling rounded up to 1/2/5/10/20/50… × 1024^n. The labels then read
// "2.00 MB/s, 1.50 MB/s, 1.00 MB/s" instead of "1.37, 1.03, 0.69": an axis
// whose ticks only differ in the third digit is noise, not a scale.
function niceRateCeil(v: number): number {
  if (!(v > 0)) return 1024;
  const mag = Math.pow(1024, Math.floor(Math.log(v) / Math.log(1024)));
  const n = v / mag;
  const step =
    n <= 1 ? 1
    : n <= 2 ? 2
    : n <= 5 ? 5
    : n <= 10 ? 10
    : n <= 20 ? 20
    : n <= 50 ? 50
    : n <= 100 ? 100
    : n <= 200 ? 200
    : n <= 500 ? 500
    : 1024;
  return step * mag;
}

const chart = computed(() => {
  const pts = series.value;
  if (!pts.length) {
    return {
      up: "",
      upIntl: "",
      down: "",
      downIntl: "",
      has: false,
      yTicks: [] as ChartTick[],
      xTicks: [] as ChartTick[],
    };
  }

  // The series is uniform, so the gap between two points is the bucket width.
  // That is the only thing that turns "bytes in a slot" into the rate the
  // vertical axis claims to show.
  const stepSec = pts.length > 1 && pts[1].t > pts[0].t ? pts[1].t - pts[0].t : 60;
  const rate = (v?: number | null) => (v ?? 0) / stepSec;
  const peak = niceRateCeil(
    Math.max(
      ...pts.map((p) =>
        Math.max(
          rate(p.direct_upload_bytes),
          rate(p.proxy_upload_bytes),
          rate(p.direct_download_bytes),
          rate(p.proxy_download_bytes),
        ),
      ),
    ),
  );

  const toPath = (pick: (p: stats.SeriesPoint) => number) =>
    pts
      .map(
        (p, i) =>
          `${(i / Math.max(1, pts.length - 1)) * 100},${
            100 - (Math.min(rate(pick(p)), peak) / peak) * 100
          }`,
      )
      .join(" ");

  // Five labels each way: peak-to-zero down the side, first-to-last sample
  // along the bottom (naming the window that is actually drawn, which is not
  // always the full 30 minutes right after a restart).
  const yTicks: ChartTick[] = Array.from({ length: Y_DIVS + 1 }, (_, i) => ({
    pos: (i / Y_DIVS) * 100,
    label: fmtBps(peak * (1 - i / Y_DIVS)),
  }));
  const xTicks: ChartTick[] = [];
  const seenIdx = new Set<number>();
  for (let i = 0; i <= X_DIVS; i++) {
    // With only a handful of samples two ticks land on the same point; one
    // label is enough and five copies at the same offset look like a bug.
    const idx = Math.round((i / X_DIVS) * (pts.length - 1));
    if (seenIdx.has(idx)) continue;
    seenIdx.add(idx);
    xTicks.push({
      pos: (idx / Math.max(1, pts.length - 1)) * 100,
      label: fmtHM(pts[idx].t),
    });
  }

  return {
    up: toPath((p) => p.direct_upload_bytes ?? 0),
    upIntl: toPath((p) => p.proxy_upload_bytes ?? 0),
    down: toPath((p) => p.direct_download_bytes ?? 0),
    downIntl: toPath((p) => p.proxy_download_bytes ?? 0),
    has: true,
    yTicks,
    xTicks,
  };
});
</script>

<template>
  <section class="page">
    <div class="status-card card">
      <div class="status-main">
        <span class="dot" :class="status.running ? 'on' : 'off'"></span>
        <div>
          <div class="status-title">
            {{ status.running ? t("engine.running") : t("engine.stopped") }}
          </div>
          <div class="status-sub" v-if="status.running">
            SOCKS5 {{ status.socks5 }} · HTTP {{ status.http }} · DNS
            {{ status.dns }}
          </div>
          <div class="status-sub" v-else-if="status.spp_server">
            {{
              t("engine.upstreamPending", {
                v: `${status.spp_server} (${status.spp_proto})`,
              })
            }}
          </div>
          <div class="status-sub" v-else>{{ t("engine.noUpstream") }}</div>
        </div>
      </div>
      <button
        class="btn"
        :class="status.running ? 'danger' : 'primary'"
        :disabled="busy"
        @click="toggle"
      >
        {{ busy ? t("engine.working") : status.running ? t("engine.stop") : t("engine.start") }}
      </button>
    </div>

    <div class="mode-row">
      <span class="mode-label">{{ t("mode.label") }}</span>
      <div class="seg">
        <button :class="{ active: proxyMode === 'rule' }" @click="setMode('rule')">
          {{ t("mode.rule") }}
        </button>
        <button
          :class="{ active: proxyMode === 'global' }"
          @click="setMode('global')"
        >
          {{ t("mode.global") }}
        </button>
        <button
          :class="{ active: proxyMode === 'direct' }"
          @click="setMode('direct')"
        >
          {{ t("mode.direct") }}
        </button>
      </div>
      <span class="mode-hint">
        {{
          proxyMode === "rule"
            ? t("mode.ruleHint")
            : proxyMode === "global"
              ? t("mode.globalHint")
              : t("mode.directHint")
        }}
      </span>
    </div>

    <div class="options-row">
      <button
        class="card option-chip"
        :class="{ on: status.system_proxy }"
        @click="toggleSystemProxy"
      >
        <span class="dot" :class="status.system_proxy ? 'on' : 'off'"></span>
        <span class="chip-title">{{ t("chip.systemProxy") }}</span>
        <span class="chip-state">{{ status.system_proxy ? t("chip.on") : t("chip.off") }}</span>
      </button>
      <button
        class="card option-chip"
        :class="{ on: autoStart }"
        @click="toggleAutoStart"
      >
        <span class="dot" :class="autoStart ? 'on' : 'off'"></span>
        <span class="chip-title">{{ t("chip.autoConnect") }}</span>
        <span class="chip-state">{{ autoStart ? t("chip.connectOnStart") : t("chip.connectManual") }}</span>
      </button>
      <button
        class="card option-chip"
        :class="{ on: launchAtLogin }"
        @click="toggleLaunchAtLogin"
      >
        <span class="dot" :class="launchAtLogin ? 'on' : 'off'"></span>
        <span class="chip-title">{{ t("chip.launchAtLogin") }}</span>
        <span class="chip-state">{{
          launchAtLogin ? t("chip.loginAuto") : t("chip.disabled")
        }}</span>
      </button>
      <button
        class="card option-chip"
        :class="{ on: status.tun }"
        @click="toggleTunMode"
      >
        <span class="dot" :class="status.tun ? 'on' : 'off'"></span>
        <span class="chip-title">{{ t("chip.tun") }}</span>
        <span class="chip-state">{{
          status.tun
            ? t("chip.tunActive")
            : status.tun_error
              ? t("chip.tunRetry")
              : tunMode
                ? t("chip.tunPending")
                : t("chip.tunIdle")
        }}</span>
      </button>
      <button
        class="card option-chip"
        :class="{ on: proxyGuard }"
        @click="toggleProxyGuard"
      >
        <span class="dot" :class="proxyGuard ? 'on' : 'off'"></span>
        <span class="chip-title">{{ t("chip.guard") }}</span>
        <span class="chip-state">{{
          proxyGuard ? t("chip.guardOn") : t("chip.disabled")
        }}</span>
      </button>
    </div>

    <div v-if="actionError" class="error-banner">{{ actionError }}</div>

    <div class="cards">
      <div class="card stat">
        <div class="stat-label">{{ t("stat.conns") }}</div>
        <div class="stat-value">{{ overview?.active_conns ?? 0 }}</div>
        <div
          class="split-bar conns"
          :class="{ idle: !hasSplit(overview?.active_direct, overview?.active_proxy) }"
        >
          <i class="seg direct" :style="{ width: connShare + '%' }"></i>
          <i class="seg proxy"></i>
        </div>
        <div class="split-line">
          <span>{{ t("lifetime.direct") }} {{ overview?.active_direct ?? 0 }}</span>
          <span>{{ t("lifetime.proxy") }} {{ overview?.active_proxy ?? 0 }}</span>
          <span v-if="connOther > 0">{{ t("stat.connsUdp") }} {{ connOther }}</span>
        </div>
      </div>
      <div class="card stat">
        <div class="stat-label">{{ t("stat.upRate") }}</div>
        <div class="stat-value up">{{ fmtBps(overview?.rate?.upload_bps) }}</div>
        <div
          class="split-bar up"
          :class="{ idle: !hasSplit(overview?.rate?.direct_upload_bps, overview?.rate?.proxy_upload_bps) }"
        >
          <i class="seg direct" :style="{ width: upShare + '%' }"></i>
          <i class="seg proxy"></i>
        </div>
        <div class="split-line">
          <span>{{ t("lifetime.direct") }} {{ fmtBps(overview?.rate?.direct_upload_bps) }}</span>
          <span>{{ t("lifetime.proxy") }} {{ fmtBps(overview?.rate?.proxy_upload_bps) }}</span>
        </div>
      </div>
      <div class="card stat">
        <div class="stat-label">{{ t("stat.downRate") }}</div>
        <div class="stat-value down">
          {{ fmtBps(overview?.rate?.download_bps) }}
        </div>
        <div
          class="split-bar down"
          :class="{ idle: !hasSplit(overview?.rate?.direct_download_bps, overview?.rate?.proxy_download_bps) }"
        >
          <i class="seg direct" :style="{ width: downShare + '%' }"></i>
          <i class="seg proxy"></i>
        </div>
        <div class="split-line">
          <span>{{ t("lifetime.direct") }} {{ fmtBps(overview?.rate?.direct_download_bps) }}</span>
          <span>{{ t("lifetime.proxy") }} {{ fmtBps(overview?.rate?.proxy_download_bps) }}</span>
        </div>
      </div>
      <div class="card stat">
        <div class="stat-label">{{ t("stat.upTotal") }}</div>
        <div class="stat-value">{{ fmtBytes(overview?.totals?.upload_bytes) }}</div>
      </div>
      <div class="card stat">
        <div class="stat-label">{{ t("stat.downTotal") }}</div>
        <div class="stat-value">
          {{ fmtBytes(overview?.totals?.download_bytes) }}
        </div>
      </div>
      <div class="card stat">
        <div class="stat-label">{{ t("stat.uptime") }}</div>
        <div class="stat-value small">
          {{ fmtUptime(overview?.uptime_seconds) }}
        </div>
      </div>
    </div>

    <div class="card chart-card">
      <div class="chart-head">
        <span>{{ t("probe.title") }}</span>
        <button class="btn small" :disabled="probing" @click="runProbe">
          {{ probing ? t("probe.running") : t("probe.run") }}
        </button>
      </div>
      <div v-if="probe" class="probe-list">
        <div v-for="it in probe.items" :key="it.key" class="probe-row">
          <span class="dot" :class="it.ok ? 'on' : 'off'"></span>
          <span class="probe-name">{{ probeName(it) }}</span>
          <span class="probe-ms" :class="{ dim: !it.ok }">
            {{ it.ok ? `${it.ms}ms` : t("probe.fail") }}
          </span>
          <span class="probe-detail">{{ probeDetail(it) }}</span>
        </div>
        <div class="probe-exit" v-if="probe.exit_ip">
          {{ t("probe.exitIPValue", { ip: probe.exit_ip }) }}
          <span v-if="exitPlace" class="probe-exit-place"> · {{ exitPlace }}</span>
        </div>
      </div>
      <p v-else class="hint">
        {{ t("probe.hint") }}
      </p>
    </div>

    <div class="card chart-card">
      <div class="chart-head">
        <span>{{ t("lifetime.title") }}</span>
        <button class="btn small" @click="resetLifetime">{{ t("lifetime.reset") }}</button>
      </div>
      <div class="lifetime-grid">
        <div class="lt-item">
          <div class="lt-label lt-cn">{{ t("lifetime.direct") }}</div>
          <div class="lt-value">{{ fmtBytes(lifetimeTotal(lifetime?.direct)) }}</div>
          <div class="lt-sub">
            ↑ {{ fmtBytes(lifetime?.direct?.upload_bytes) }} · ↓
            {{ fmtBytes(lifetime?.direct?.download_bytes) }}
          </div>
        </div>
        <div class="lt-item">
          <div class="lt-label lt-foreign">{{ t("lifetime.proxy") }}</div>
          <div class="lt-value">{{ fmtBytes(lifetimeTotal(lifetime?.proxy)) }}</div>
          <div class="lt-sub">
            ↑ {{ fmtBytes(lifetime?.proxy?.upload_bytes) }} · ↓
            {{ fmtBytes(lifetime?.proxy?.download_bytes) }}
          </div>
        </div>
        <div class="lt-item">
          <div class="lt-label">{{ t("lifetime.total") }}</div>
          <div class="lt-value">{{ fmtBytes(lifetimeTotal(lifetime?.totals)) }}</div>
          <div class="lt-sub">
            ↑ {{ fmtBytes(lifetime?.totals?.upload_bytes) }} · ↓
            {{ fmtBytes(lifetime?.totals?.download_bytes) }}
          </div>
        </div>
      </div>
    </div>

    <div class="card chart-card">
      <div class="chart-head">
        <span>{{ t("chart.title") }}</span>
        <span class="legend">
          <span class="lg"><i class="sw up"></i>{{ t("chart.up") }} · {{ t("lifetime.direct") }}</span>
          <span class="lg"><i class="sw up intl"></i>{{ t("chart.up") }} · {{ t("lifetime.proxy") }}</span>
          <span class="lg"><i class="sw down"></i>{{ t("chart.down") }} · {{ t("lifetime.direct") }}</span>
          <span class="lg"><i class="sw down intl"></i>{{ t("chart.down") }} · {{ t("lifetime.proxy") }}</span>
        </span>
      </div>
      <div class="chart-body">
        <div class="chart-y">
          <span
            v-for="tk in chart.yTicks"
            :key="tk.pos"
            :style="{ top: `${tk.pos}%` }"
            >{{ tk.label }}</span
          >
        </div>
        <div class="chart-plot">
          <div class="chart-grid" aria-hidden="true">
            <i v-for="g in chart.yTicks" :key="g.pos" :style="{ top: `${g.pos}%` }"></i>
          </div>
          <svg class="chart" preserveAspectRatio="none" viewBox="0 0 100 100">
            <polyline
              v-if="chart.has"
              class="line-up intl"
              :points="chart.upIntl"
              fill="none"
            />
            <polyline
              v-if="chart.has"
              class="line-down intl"
              :points="chart.downIntl"
              fill="none"
            />
            <polyline
              v-if="chart.has"
              class="line-up"
              :points="chart.up"
              fill="none"
            />
            <polyline
              v-if="chart.has"
              class="line-down"
              :points="chart.down"
              fill="none"
            />
          </svg>
          <div class="chart-empty" v-if="!chart.has">
            {{ status.running ? t("chart.empty") : t("chart.noEngine") }}
          </div>
        </div>
      </div>
      <div class="chart-x">
        <span
          v-for="tk in chart.xTicks"
          :key="tk.pos"
          :class="{ start: tk.pos === 0, end: tk.pos === 100 }"
          :style="{ left: `${tk.pos}%` }"
          >{{ tk.label }}</span
        >
      </div>
    </div>
  </section>
</template>

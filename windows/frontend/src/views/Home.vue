<script lang="ts" setup>
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref } from "vue";
import {
  EngineStatus,
  GetAutoStartCore,
  GetLastProbe,
  GetLaunchAtLogin,
  GetLifetimeStats,
  GetOverview,
  GetProxyGuard,
  GetProxyMode,
  GetSeries,
  GetSystemProxy,
  GetTunMode,
  SetAutoStartCore,
  SetLaunchAtLogin,
  SetProxyGuard,
  SetProxyMode,
  SetSystemProxy,
  SetTunMode,
} from "../../wailsjs/go/guiapp/App";
import { EventsOn } from "../../wailsjs/runtime/runtime";
import { guiapp, stats } from "../../wailsjs/go/models";
import { fmtBytes, fmtBps, fmtCountry, fmtHM, fmtUptime } from "../format";
import { errText, t, type MsgKey } from "../i18n";

const status = ref(new guiapp.EngineStatus());
const overview = ref<stats.Overview | null>(null);
const lifetime = ref<stats.LifetimeStats | null>(null);
const series = ref<stats.SeriesPoint[]>([]);
const actionError = ref("");
const autoStart = ref(false);
const launchAtLogin = ref(false);
const tunMode = ref(false);
const proxyMode = ref("rule");
const probe = ref<guiapp.ProbeReport | null>(null);
const proxyGuard = ref(false);

let timer: number | undefined;
// The view is kept alive across tab switches, so this listener is installed
// once and stays for the life of the window: the chips and the status line
// keep tracking the engine even while another page is on screen.
let offStatus: (() => void) | undefined;
// The self-check report event: the backend probes on its own schedule and
// the strip renders whatever lands.
let offProbe: (() => void) | undefined;

// The live calls go out together. Awaited in turn they cost a round trip per
// fact and, worse, the counters on screen lag the tick that just fired; in
// parallel the page has all of them by the time the tick ends.
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
  // The lifetime record is served from the app's cache and survives the
  // engine, so it is kept even when the read fails transiently.
  if (life) lifetime.value = life;
  if (status.value.running) series.value = pts ?? [];
  else series.value = [];
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
  // The backend probes on its own clock and pushes reports as events; this
  // read only fills the strip with the newest cached report at boot so it
  // is never blank until the first minute boundary.
  const lastProbe = await GetLastProbe().catch(() => null);
  if (lastProbe) probe.value = new guiapp.ProbeReport(lastProbe);
  await refresh();
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

// The probe report carries no display text: Go reports which row it is (key)
// and which sentence applies (detail_key), and the wording lives here, in both
// languages. Raw details (a Go net error, an HTTP status) are shown verbatim
// because they are diagnostics, not copy.
const probeNames: Record<string, MsgKey> = {
  direct: "probe.item.direct",
  proxied: "probe.item.proxied",
  upstream: "probe.item.upstream",
  exitip: "probe.item.exitip",
  exitip_direct: "probe.item.exitipDirect",
  engine: "probe.item.engine",
};
const probeDetails: Record<string, MsgKey> = {
  engine_stopped: "probe.detail.engineStopped",
  not_configured: "probe.detail.notConfigured",
  tcp_ok: "probe.detail.tcpOk",
  unparsed: "probe.detail.unparsed",
};

// The toggles carry only their label; the full state sentence the old rows
// showed lives in the tooltip. TUN is the one exception: pending/retry are
// states the user must act on (a UAC prompt, a failed bring-up), so they stay
// visible text next to the switch.
const tunTitle = computed(() => {
  if (status.value.tun) return t("chip.tunActive");
  if (status.value.tun_pending) return t("chip.tunStarting");
  if (status.value.tun_retrying) return t("chip.tunRetrying");
  if (status.value.tun_error) return t("chip.tunRetry");
  if (tunMode.value) return t("chip.tunPending");
  return t("chip.tunIdle");
});

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

// The mode row's exit chip: the proxied egress address and the latency of
// the check that saw it. Hidden until the engine has probed at least once.
const exitLeg = computed(() => probe.value?.items.find((i) => i.key === "exitip"));
const exitShown = computed(
  () => !!status.value.running && !!probe.value && !!probe.value.exit_ip && !!exitLeg.value?.ok,
);
const exitText = computed(() => {
  const leg = exitLeg.value!;
  const place = exitPlace.value;
  return `出口 ${leg.ms}ms · ${probe.value!.exit_ip}${place ? " · " + place : ""}`;
});

function probeName(it: guiapp.ProbeItem): string {
  const key = probeNames[it.key];
  return key ? t(key) : it.key;
}

function probeDetail(it: guiapp.ProbeItem): string {
  const key = it.detail_key ? probeDetails[it.detail_key] : undefined;
  return key ? t(key) : it.detail || "";
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
  // The backend runs the self-check on its own clock (per minute, plus once
  // after every engine start) and pushes the report; the strip just renders
  // whatever lands, hidden or not.
  offProbe = EventsOn("probe:report", (r: guiapp.ProbeReport) => {
    probe.value = new guiapp.ProbeReport(r);
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
  offProbe?.();
  offProbe = undefined;
});

// Traffic chart. Four curves normalised against one shared ceiling, drawn
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

// Monotone cubic interpolation (Fritsch–Carlson) through the sample points.
// Straight segments between one-minute samples read as a seismograph; a curve
// is what the underlying traffic looks like. The monotone tangents are what
// keep the curve honest: it passes through every sample and never dips below
// the axis or above the samples it joins, which a plain Catmull-Rom spline
// does on flat stretches.
function smoothPath(values: number[]): string {
  const n = values.length;
  if (!n) return "";
  const xAt = (i: number) => (i / Math.max(1, n - 1)) * 100;
  if (n === 1) return `M ${xAt(0).toFixed(2)},${values[0].toFixed(2)}`;

  const h = xAt(1) - xAt(0);
  const secant: number[] = [];
  for (let i = 0; i < n - 1; i++) secant.push((values[i + 1] - values[i]) / h);

  // Tangents start as the average of the neighbouring secants; a sign change
  // means a local extremum, where the curve has to level off.
  const tangent: number[] = new Array(n);
  tangent[0] = secant[0];
  tangent[n - 1] = secant[n - 2];
  for (let i = 1; i < n - 1; i++) {
    tangent[i] = secant[i - 1] * secant[i] <= 0 ? 0 : (secant[i - 1] + secant[i]) / 2;
  }

  // Clamp the tangents so each segment stays monotone: no overshoot beyond
  // the two samples it connects.
  for (let i = 0; i < n - 1; i++) {
    const d = secant[i];
    if (d === 0) {
      tangent[i] = 0;
      tangent[i + 1] = 0;
      continue;
    }
    let a = tangent[i] / d;
    let b = tangent[i + 1] / d;
    if (a < 0) {
      tangent[i] = 0;
      a = 0;
    }
    if (b < 0) {
      tangent[i + 1] = 0;
      b = 0;
    }
    const s = a * a + b * b;
    if (s > 9) {
      const t = 3 / Math.sqrt(s);
      tangent[i] = t * a * d;
      tangent[i + 1] = t * b * d;
    }
  }

  let path = `M ${xAt(0).toFixed(2)},${values[0].toFixed(2)}`;
  for (let i = 0; i < n - 1; i++) {
    const c1x = xAt(i) + h / 3;
    const c1y = values[i] + (tangent[i] * h) / 3;
    const c2x = xAt(i + 1) - h / 3;
    const c2y = values[i + 1] - (tangent[i + 1] * h) / 3;
    path += ` C ${c1x.toFixed(2)},${c1y.toFixed(2)} ${c2x.toFixed(2)},${c2y.toFixed(2)} ${xAt(i + 1).toFixed(2)},${values[i + 1].toFixed(2)}`;
  }
  return path;
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

  const curve = (pick: (p: stats.SeriesPoint) => number) =>
    smoothPath(
      pts.map((p) => 100 - (Math.min(rate(pick(p)), peak) / peak) * 100),
    );

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
    up: curve((p) => p.direct_upload_bytes ?? 0),
    upIntl: curve((p) => p.proxy_upload_bytes ?? 0),
    down: curve((p) => p.direct_download_bytes ?? 0),
    downIntl: curve((p) => p.proxy_download_bytes ?? 0),
    has: true,
    yTicks,
    xTicks,
  };
});

// ---------- load dials ----------
//
// Each live reading in the top row carries a small dial whose full sweep is
// that reading's all-time record on the statistics page (max concurrent
// connections, peak upload, peak download). The needle answers "how close to
// the record is the link right now", which the bare number cannot: 2 MB/s is
// idle or near the ceiling depending on what this machine has done before.
// The cap is printed in the card header; a record of zero means nothing has
// been measured yet, and the dial simply stays at rest.
const GAUGE_ARC = Math.PI * 28;

function gaugeOf(value: number, max: number) {
  const frac = max > 0 ? Math.min(1, value / max) : 0;
  return { frac, offset: GAUGE_ARC * (1 - frac) };
}

const gauge = computed(() => ({
  conns: gaugeOf(overview.value?.active_conns ?? 0, lifetime.value?.max_conns ?? 0),
  up: gaugeOf(overview.value?.rate?.upload_bps ?? 0, lifetime.value?.peak_up ?? 0),
  down: gaugeOf(overview.value?.rate?.download_bps ?? 0, lifetime.value?.peak_down ?? 0),
}));

function capRate(v?: number | null): string {
  return v && v > 0 ? fmtBps(v) : "—";
}
</script>

<template>
  <section class="page">
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
      <span class="mode-exit" v-if="exitShown">
        <i class="dot on"></i>
        <span>{{ exitText }}</span>
      </span>
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
        class="option-toggle"
        :class="{ on: status.system_proxy }"
        :title="status.system_proxy ? t('chip.on') : t('chip.off')"
        @click="toggleSystemProxy"
      >
        <i class="tk"><i class="knob"></i></i>
        <span>{{ t("chip.systemProxy") }}</span>
      </button>
      <button
        class="option-toggle"
        :class="{ on: autoStart }"
        :title="autoStart ? t('chip.connectOnStart') : t('chip.connectManual')"
        @click="toggleAutoStart"
      >
        <i class="tk"><i class="knob"></i></i>
        <span>{{ t("chip.autoConnect") }}</span>
      </button>
      <button
        class="option-toggle"
        :class="{ on: launchAtLogin }"
        :title="launchAtLogin ? t('chip.loginAuto') : t('chip.disabled')"
        @click="toggleLaunchAtLogin"
      >
        <i class="tk"><i class="knob"></i></i>
        <span>{{ t("chip.launchAtLogin") }}</span>
      </button>
      <button
        class="option-toggle"
        :class="{ on: status.tun || status.tun_pending, err: !!status.tun_error && !status.tun_retrying }"
        :title="tunTitle"
        @click="toggleTunMode"
      >
        <i class="tk"><i class="knob"></i></i>
        <span>{{ t("chip.tun") }}</span>
        <span v-if="status.tun_pending && !status.tun" class="tg-state">{{ t("chip.tunStarting") }}</span>
        <span v-else-if="status.tun_retrying" class="tg-state">{{ t("chip.tunRetrying") }}</span>
        <span v-else-if="status.tun_error" class="tg-state">{{ t("chip.tunRetry") }}</span>
        <span v-else-if="!status.tun && tunMode" class="tg-state">{{ t("chip.tunPending") }}</span>
      </button>
      <button
        class="option-toggle"
        :class="{ on: proxyGuard }"
        :title="proxyGuard ? t('chip.guardOn') : t('chip.disabled')"
        @click="toggleProxyGuard"
      >
        <i class="tk"><i class="knob"></i></i>
        <span>{{ t("chip.guard") }}</span>
      </button>
    </div>

    <div v-if="actionError" class="error-banner">{{ actionError }}</div>

    <div class="cards">
      <div class="card stat">
        <div class="stat-head">
          <span class="stat-label">{{ t("stat.conns") }}</span>
          <span class="stat-cap">{{ t("stat.cap") }} {{ lifetime?.max_conns || "—" }}</span>
        </div>
        <div class="stat-main">
          <div class="stat-value">{{ overview?.active_conns ?? 0 }}</div>
          <svg class="gauge" viewBox="0 0 72 42" aria-hidden="true">
            <path class="gauge-track" d="M 8 38 A 28 28 0 0 1 64 38" />
            <path
              class="gauge-fill"
              d="M 8 38 A 28 28 0 0 1 64 38"
              :style="{ strokeDasharray: GAUGE_ARC, strokeDashoffset: gauge.conns.offset }"
            />
            <g class="gauge-needle" :style="{ transform: `rotate(${gauge.conns.frac * 180}deg)` }">
              <line x1="36" y1="38" x2="12" y2="38" />
            </g>
            <circle class="gauge-hub" cx="36" cy="38" r="2.2" />
          </svg>
        </div>
        <div class="split-line">
          <span class="lg"><i class="sw dim"></i>{{ t("lifetime.direct") }} {{ overview?.active_direct ?? 0 }}</span>
          <span class="lg"><i class="sw dim intl"></i>{{ t("lifetime.proxy") }} {{ overview?.active_proxy ?? 0 }}</span>
        </div>
      </div>
      <div class="card stat">
        <div class="stat-head">
          <span class="stat-label">{{ t("stat.upRate") }}</span>
          <span class="stat-cap">{{ t("stat.cap") }} {{ capRate(lifetime?.peak_up) }}</span>
        </div>
        <div class="stat-main">
          <div class="stat-value up">{{ fmtBps(overview?.rate?.upload_bps) }}</div>
          <svg class="gauge up" viewBox="0 0 72 42" aria-hidden="true">
            <path class="gauge-track" d="M 8 38 A 28 28 0 0 1 64 38" />
            <path
              class="gauge-fill"
              d="M 8 38 A 28 28 0 0 1 64 38"
              :style="{ strokeDasharray: GAUGE_ARC, strokeDashoffset: gauge.up.offset }"
            />
            <g class="gauge-needle" :style="{ transform: `rotate(${gauge.up.frac * 180}deg)` }">
              <line x1="36" y1="38" x2="12" y2="38" />
            </g>
            <circle class="gauge-hub" cx="36" cy="38" r="2.2" />
          </svg>
        </div>
        <div class="split-line">
          <span class="lg"><i class="sw up"></i>{{ t("lifetime.direct") }} {{ fmtBps(overview?.rate?.direct_upload_bps) }}</span>
          <span class="lg"><i class="sw up intl"></i>{{ t("lifetime.proxy") }} {{ fmtBps(overview?.rate?.proxy_upload_bps) }}</span>
        </div>
      </div>
      <div class="card stat">
        <div class="stat-head">
          <span class="stat-label">{{ t("stat.downRate") }}</span>
          <span class="stat-cap">{{ t("stat.cap") }} {{ capRate(lifetime?.peak_down) }}</span>
        </div>
        <div class="stat-main">
          <div class="stat-value down">
            {{ fmtBps(overview?.rate?.download_bps) }}
          </div>
          <svg class="gauge down" viewBox="0 0 72 42" aria-hidden="true">
            <path class="gauge-track" d="M 8 38 A 28 28 0 0 1 64 38" />
            <path
              class="gauge-fill"
              d="M 8 38 A 28 28 0 0 1 64 38"
              :style="{ strokeDasharray: GAUGE_ARC, strokeDashoffset: gauge.down.offset }"
            />
            <g class="gauge-needle" :style="{ transform: `rotate(${gauge.down.frac * 180}deg)` }">
              <line x1="36" y1="38" x2="12" y2="38" />
            </g>
            <circle class="gauge-hub" cx="36" cy="38" r="2.2" />
          </svg>
        </div>
        <div class="split-line">
          <span class="lg"><i class="sw down"></i>{{ t("lifetime.direct") }} {{ fmtBps(overview?.rate?.direct_download_bps) }}</span>
          <span class="lg"><i class="sw down intl"></i>{{ t("lifetime.proxy") }} {{ fmtBps(overview?.rate?.proxy_download_bps) }}</span>
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
            <path
              v-if="chart.has"
              class="line-up intl"
              :d="chart.upIntl"
              fill="none"
            />
            <path
              v-if="chart.has"
              class="line-down intl"
              :d="chart.downIntl"
              fill="none"
            />
            <path
              v-if="chart.has"
              class="line-up"
              :d="chart.up"
              fill="none"
            />
            <path
              v-if="chart.has"
              class="line-down"
              :d="chart.down"
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

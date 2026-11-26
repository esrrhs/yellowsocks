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
import { fmtBytes, fmtBps, fmtCountry, fmtUptime } from "../format";
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

// Traffic chart: two SVG polylines (upload/download) normalised against the
// same peak, drawn in a 0..100 viewBox and stretched by CSS.
const chart = computed(() => {
  const pts = series.value;
  if (!pts.length) return { up: "", down: "", has: false };
  const peak = Math.max(
    1,
    ...pts.map((p) => Math.max(p.upload_bytes, p.download_bytes)),
  );
  const toPath = (pick: (p: stats.SeriesPoint) => number) =>
    pts
      .map(
        (p, i) =>
          `${(i / Math.max(1, pts.length - 1)) * 100},${
            100 - (pick(p) / peak) * 100
          }`,
      )
      .join(" ");
  return {
    up: toPath((p) => p.upload_bytes),
    down: toPath((p) => p.download_bytes),
    has: true,
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
      </div>
      <div class="card stat">
        <div class="stat-label">{{ t("stat.upRate") }}</div>
        <div class="stat-value up">{{ fmtBps(overview?.rate?.upload_bps) }}</div>
      </div>
      <div class="card stat">
        <div class="stat-label">{{ t("stat.downRate") }}</div>
        <div class="stat-value down">
          {{ fmtBps(overview?.rate?.download_bps) }}
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
          <i class="sw up"></i>{{ t("chart.up") }}
          <i class="sw down"></i>{{ t("chart.down") }}
        </span>
      </div>
      <svg class="chart" preserveAspectRatio="none" viewBox="0 0 100 100">
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
        <text
          v-if="!chart.has"
          class="empty"
          x="50"
          y="52"
          text-anchor="middle"
        >
          {{ status.running ? t("chart.empty") : t("chart.noEngine") }}
        </text>
      </svg>
    </div>
  </section>
</template>

<script lang="ts" setup>
import { computed, onMounted, onUnmounted, ref } from "vue";
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
import { fmtBytes, fmtBps, fmtUptime } from "../format";
import { t } from "../i18n";

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
// Unsubscribing matters: this view is re-mounted on every tab switch, and a
// listener left behind would keep replacing the state of an unmounted view
// (and pile up one more copy each time).
let offStatus: (() => void) | undefined;

async function refresh() {
  try {
    overview.value = await GetOverview();
    series.value = (await GetSeries(1800)) ?? [];
  } catch {
    // Engine stopped: bindings return null and the next engine:status event
    // refreshes the UI state.
  }
  // The lifetime record is served from a cache on the Go side, so it stays
  // readable even while the engine is stopped.
  lifetime.value = await GetLifetimeStats();
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
    actionError.value = String(e?.message ?? e);
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
    actionError.value = String(e?.message ?? e);
  }
}

async function toggleAutoStart() {
  actionError.value = "";
  try {
    await SetAutoStartCore(!autoStart.value);
    autoStart.value = await GetAutoStartCore();
  } catch (e: any) {
    actionError.value = String(e?.message ?? e);
  }
}

async function toggleLaunchAtLogin() {
  actionError.value = "";
  try {
    await SetLaunchAtLogin(!launchAtLogin.value);
    // The OS registry is the source of truth for this one.
    launchAtLogin.value = await GetLaunchAtLogin();
  } catch (e: any) {
    actionError.value = String(e?.message ?? e);
  }
}

async function toggleTunMode() {
  actionError.value = "";
  try {
    const next = !tunMode.value;
    await SetTunMode(next);
    tunMode.value = next;
  } catch (e: any) {
    actionError.value = String(e?.message ?? e);
  }
}

async function resetLifetime() {
  actionError.value = "";
  try {
    await ResetLifetimeStats();
    lifetime.value = await GetLifetimeStats();
  } catch (e: any) {
    actionError.value = String(e?.message ?? e);
  }
}

async function toggleProxyGuard() {
  actionError.value = "";
  try {
    await SetProxyGuard(!proxyGuard.value);
    proxyGuard.value = await GetProxyGuard();
  } catch (e: any) {
    actionError.value = String(e?.message ?? e);
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
    actionError.value = String(e?.message ?? e);
  }
}

async function runProbe() {
  probing.value = true;
  actionError.value = "";
  try {
    probe.value = await TestConnectivity();
  } catch (e: any) {
    actionError.value = String(e?.message ?? e);
  } finally {
    probing.value = false;
  }
}

function lifetimeTotal(c?: stats.Counters | null): number {
  if (!c) return 0;
  return (c.upload_bytes ?? 0) + (c.download_bytes ?? 0);
}

onMounted(async () => {
  offStatus = EventsOn("engine:status", (st: guiapp.EngineStatus) => {
    status.value = new guiapp.EngineStatus(st);
    tunMode.value = !!st.tun;
    if (st.proxy_mode) proxyMode.value = st.proxy_mode;
    if (!st.running) {
      overview.value = null;
      series.value = [];
    }
  });
  status.value = new guiapp.EngineStatus(await EngineStatus());
  try {
    status.value.system_proxy = await GetSystemProxy();
    autoStart.value = await GetAutoStartCore();
    launchAtLogin.value = await GetLaunchAtLogin();
    tunMode.value = await GetTunMode();
    proxyMode.value = await GetProxyMode();
  } catch {
    // binding unavailable; keep default
  }
  await refresh();
  timer = window.setInterval(refresh, 1000);
});

onUnmounted(() => {
  if (timer) window.clearInterval(timer);
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
          <span class="probe-name">{{ it.name }}</span>
          <span class="probe-ms" :class="{ dim: !it.ok }">
            {{ it.ok ? `${it.ms}ms` : t("probe.fail") }}
          </span>
          <span class="probe-detail">{{ it.detail }}</span>
        </div>
        <div class="probe-exit" v-if="probe.exit_ip">
          {{ t("probe.exitIP") }}:{{ probe.exit_ip }}
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

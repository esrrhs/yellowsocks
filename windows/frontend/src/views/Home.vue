<script lang="ts" setup>
import { computed, onMounted, onUnmounted, ref } from "vue";
import {
  EngineStatus,
  GetAutoStartCore,
  GetLaunchAtLogin,
  GetLifetimeStats,
  GetOverview,
  GetSeries,
  GetSystemProxy,
  GetTunMode,
  ResetLifetimeStats,
  SetAutoStartCore,
  SetLaunchAtLogin,
  SetSystemProxy,
  SetTunMode,
  StartEngine,
  StopEngine,
} from "../../wailsjs/go/guiapp/App";
import { EventsOn } from "../../wailsjs/runtime/runtime";
import { guiapp, stats } from "../../wailsjs/go/models";

const status = ref(new guiapp.EngineStatus());
const overview = ref<stats.Overview | null>(null);
const series = ref<stats.SeriesPoint[]>([]);
const actionError = ref("");
const busy = ref(false);
const autoStart = ref(false);
const launchAtLogin = ref(false);
const tunMode = ref(false);
const lifetime = ref<stats.LifetimeStats | null>(null);

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

function fmtBytes(n?: number | null): string {
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

function lifetimeTotal(c?: stats.Counters | null): number {
  if (!c) return 0;
  return (c.upload_bytes ?? 0) + (c.download_bytes ?? 0);
}

function fmtBps(n?: number | null): string {
  return `${fmtBytes(n)}/s`;
}

function fmtUptime(seconds?: number | null): string {
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

onMounted(async () => {
  offStatus = EventsOn("engine:status", (st: guiapp.EngineStatus) => {
    status.value = new guiapp.EngineStatus(st);
    tunMode.value = !!st.tun;
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
            {{ status.running ? "代理运行中" : "代理已停止" }}
          </div>
          <div class="status-sub" v-if="status.running">
            SOCKS5 {{ status.socks5 }} · HTTP {{ status.http }} · DNS
            {{ status.dns }}
          </div>
          <div class="status-sub" v-else-if="status.spp_server">
            上游 {{ status.spp_server }} ({{ status.spp_proto }}),启动后生效
          </div>
          <div class="status-sub" v-else>在设置中配置上游服务器</div>
        </div>
      </div>
      <button
        class="btn"
        :class="status.running ? 'danger' : 'primary'"
        :disabled="busy"
        @click="toggle"
      >
        {{ busy ? "处理中..." : status.running ? "停止代理" : "启动代理" }}
      </button>
    </div>

    <div class="options-row">
      <button
        class="card option-chip"
        :class="{ on: status.system_proxy }"
        @click="toggleSystemProxy"
      >
        <span class="dot" :class="status.system_proxy ? 'on' : 'off'"></span>
        <span class="chip-title">系统透明代理</span>
        <span class="chip-state">{{ status.system_proxy ? "已开启" : "已关闭" }}</span>
      </button>
      <button
        class="card option-chip"
        :class="{ on: autoStart }"
        @click="toggleAutoStart"
      >
        <span class="dot" :class="autoStart ? 'on' : 'off'"></span>
        <span class="chip-title">自动连接</span>
        <span class="chip-state">{{ autoStart ? "启动时连接" : "手动连接" }}</span>
      </button>
      <button
        class="card option-chip"
        :class="{ on: launchAtLogin }"
        @click="toggleLaunchAtLogin"
      >
        <span class="dot" :class="launchAtLogin ? 'on' : 'off'"></span>
        <span class="chip-title">开机启动</span>
        <span class="chip-state">{{
          launchAtLogin ? "登录后自动运行" : "未启用"
        }}</span>
      </button>
      <button
        class="card option-chip"
        :class="{ on: status.tun }"
        @click="toggleTunMode"
      >
        <span class="dot" :class="status.tun ? 'on' : 'off'"></span>
        <span class="chip-title">TUN 模式</span>
        <span class="chip-state">{{
          status.tun
            ? "全局接管中"
            : status.tun_error
              ? "失败,点重试"
              : tunMode
                ? "授权后生效"
                : "零配置全局代理"
        }}</span>
      </button>
    </div>

    <div v-if="actionError" class="error-banner">{{ actionError }}</div>

    <div class="cards">
      <div class="card stat">
        <div class="stat-label">活动连接</div>
        <div class="stat-value">{{ overview?.active_conns ?? 0 }}</div>
      </div>
      <div class="card stat">
        <div class="stat-label">上传速率</div>
        <div class="stat-value up">{{ fmtBps(overview?.rate?.upload_bps) }}</div>
      </div>
      <div class="card stat">
        <div class="stat-label">下载速率</div>
        <div class="stat-value down">
          {{ fmtBps(overview?.rate?.download_bps) }}
        </div>
      </div>
      <div class="card stat">
        <div class="stat-label">上传总量</div>
        <div class="stat-value">{{ fmtBytes(overview?.totals?.upload_bytes) }}</div>
      </div>
      <div class="card stat">
        <div class="stat-label">下载总量</div>
        <div class="stat-value">
          {{ fmtBytes(overview?.totals?.download_bytes) }}
        </div>
      </div>
      <div class="card stat">
        <div class="stat-label">运行时长</div>
        <div class="stat-value small">
          {{ fmtUptime(overview?.uptime_seconds) }}
        </div>
      </div>
    </div>

    <div class="card chart-card">
      <div class="chart-head">
        <span>累计流量(永久记录,重启与重置后仍保留)</span>
        <button class="btn small" @click="resetLifetime">清零</button>
      </div>
      <div class="lifetime-grid">
        <div class="lt-item">
          <div class="lt-label lt-cn">国内</div>
          <div class="lt-value">{{ fmtBytes(lifetimeTotal(lifetime?.direct)) }}</div>
          <div class="lt-sub">
            ↑ {{ fmtBytes(lifetime?.direct?.upload_bytes) }} · ↓
            {{ fmtBytes(lifetime?.direct?.download_bytes) }}
          </div>
        </div>
        <div class="lt-item">
          <div class="lt-label lt-foreign">国外</div>
          <div class="lt-value">{{ fmtBytes(lifetimeTotal(lifetime?.proxy)) }}</div>
          <div class="lt-sub">
            ↑ {{ fmtBytes(lifetime?.proxy?.upload_bytes) }} · ↓
            {{ fmtBytes(lifetime?.proxy?.download_bytes) }}
          </div>
        </div>
        <div class="lt-item">
          <div class="lt-label">合计</div>
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
        <span>近 30 分钟流量</span>
        <span class="legend">
          <i class="sw up"></i>上传
          <i class="sw down"></i>下载
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
          {{ status.running ? "暂无数据" : "引擎未运行" }}
        </text>
      </svg>
    </div>
  </section>
</template>

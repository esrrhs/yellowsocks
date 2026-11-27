<script lang="ts" setup>
import { computed, onMounted, onUnmounted, ref, watch, type Component } from "vue";
import Home from "./views/Home.vue";
import Connections from "./views/Connections.vue";
import Nodes from "./views/Nodes.vue";
import Logs from "./views/Logs.vue";
import Stats from "./views/Stats.vue";
import Settings from "./views/Settings.vue";
import About from "./views/About.vue";
import { GetLanguage, SetLanguage } from "../wailsjs/go/guiapp/App";
import { locale, setLocale, t, type Locale, type MsgKey } from "./i18n";

// Drawn, not typed. The previous icons were the Unicode glyphs ⌂ ⇄ ☰ ⚙ ⓘ,
// which come from whatever font the system falls back to: different weights,
// different baselines, and instantly recognisable as decoration rather than
// design. These are 16px stroke paths on a shared grid, so they all sit on the
// same baseline and take their colour from the nav item.
const pages: { id: string; key: MsgKey; paths: string[] }[] = [
  {
    id: "home",
    key: "nav.home",
    paths: ["M2.5 3.5h4.5v4.5H2.5z", "M9 3.5h4.5v4.5H9z", "M2.5 9h4.5v4.5H2.5z", "M9 9h4.5v4.5H9z"],
  },
  {
    id: "connections",
    key: "nav.connections",
    paths: ["M2.5 6h11", "M10.5 3.5 13 6l-2.5 2.5", "M13.5 10h-11", "M5.5 7.5 3 10l2.5 2.5"],
  },
  {
    id: "nodes",
    key: "nav.nodes",
    paths: ["M2.5 2.5h4.5v4.5H2.5z", "M9 9h4.5v4.5H9z", "M5.5 5l3.5 4"],
  },
  {
    id: "logs",
    key: "nav.logs",
    paths: ["M2.5 4h11", "M2.5 8h11", "M2.5 12h6.5"],
  },
  {
    id: "settings",
    key: "nav.settings",
    paths: ["M3.5 2.5v11", "M8 2.5v11", "M12.5 2.5v11", "M1.5 6h4", "M6 10h4", "M11 4.5h4"],
  },
  {
    id: "stats",
    key: "nav.stats",
    paths: ["M3 13.5V9", "M8 13.5V3.5", "M13 13.5V6.5"],
  },
  {
    id: "about",
    key: "nav.about",
    paths: ["M8 2.6a5.4 5.4 0 1 0 0 10.8 5.4 5.4 0 1 0 0-10.8", "M8 7.2v4", "M8 4.9v.8"],
  },
];
const current = ref("home");

// One instance per page, kept alive across tab switches. The old v-if chain
// tore the view down and rebuilt it on every switch, so the page came back
// with its state reset -- traffic counters at 0 until the first poll, the
// option chips dark until each getter had answered in turn. KeepAlive keeps
// the state (and the scroll position, the search box, the selected
// sub-tab) and the views refresh themselves through onActivated.
const views: Record<string, Component> = {
  home: Home,
  connections: Connections,
  nodes: Nodes,
  logs: Logs,
  settings: Settings,
  stats: Stats,
  about: About,
};
const view = computed<Component>(() => views[current.value] ?? About);

// Theme: the preference is stored, the resolved value comes from the OS when
// the preference is "system". WebView2 reports the OS setting through
// prefers-color-scheme, so no Win32 call is needed.
type ThemeMode = "system" | "dark" | "light";
const themeMode = ref<ThemeMode>(
  (localStorage.getItem("ys-theme") as ThemeMode) || "system",
);

const themes: { id: ThemeMode; key: MsgKey }[] = [
  { id: "system", key: "theme.system" },
  { id: "dark", key: "theme.dark" },
  { id: "light", key: "theme.light" },
];

const mq = window.matchMedia("(prefers-color-scheme: dark)");

function applyTheme() {
  const sysDark = mq.matches;
  const dark = themeMode.value === "dark" || (themeMode.value === "system" && sysDark);
  document.documentElement.dataset.theme = dark ? "dark" : "light";
  // The native window chrome is drawn by WebView2, not CSS; the page
  // background has to match or the edges flash during resize.
  document.documentElement.style.colorScheme = dark ? "dark" : "light";
}

function setTheme(m: ThemeMode) {
  themeMode.value = m;
  localStorage.setItem("ys-theme", m);
  applyTheme();
}

const langs: { id: Locale; label: string }[] = [
  { id: "zh", label: "中" },
  { id: "en", label: "EN" },
];

// The persisted choice lives in settings.json (readable by the tray before the
// WebView even loads); localStorage is only the cache that gets the first paint
// right. Reading it back here is what keeps the two stores from drifting: if
// they disagree, settings.json wins, and an empty answer ("never chosen") is
// answered with the language the window is already showing.
watch(locale, (l) => {
  void SetLanguage(l).catch(() => {});
});

// Applied during setup, not onMounted: the first paint must already have the
// right theme or a light-theme user sees a dark flash on every launch.
applyTheme();

onMounted(async () => {
  mq.addEventListener("change", applyTheme);
  const saved = await GetLanguage().catch(() => "");
  if ((saved === "zh" || saved === "en") && saved !== locale.value) {
    setLocale(saved);
  } else if (saved !== "zh" && saved !== "en") {
    void SetLanguage(locale.value).catch(() => {});
  }
});

onUnmounted(() => {
  mq.removeEventListener("change", applyTheme);
});
</script>

<template>
  <div class="app">
    <aside class="sidebar">
      <div class="brand">
        <div class="logo">YS</div>
        <div class="brand-name">YellowSocks</div>
      </div>
      <nav class="nav">
        <button
          v-for="p in pages"
          :key="p.id"
          class="nav-item"
          :class="{ active: current === p.id }"
          @click="current = p.id"
        >
          <span class="nav-icon">
            <svg
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              stroke-width="1.1"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <path v-for="(d, i) in p.paths" :key="i" :d="d" />
            </svg>
          </span>{{ t(p.key) }}
        </button>
      </nav>
      <div class="sidebar-foot">
        <div class="mini-switch">
          <button
            v-for="th in themes"
            :key="th.id"
            :class="{ active: themeMode === th.id }"
            :title="t(th.key)"
            @click="setTheme(th.id)"
          >
            {{ t(th.key) }}
          </button>
        </div>
        <div class="mini-switch">
          <button
            v-for="l in langs"
            :key="l.id"
            :class="{ active: locale === l.id }"
            @click="setLocale(l.id)"
          >
            {{ l.label }}
          </button>
        </div>
      </div>
    </aside>
    <main class="content">
      <KeepAlive>
        <component :is="view" />
      </KeepAlive>
    </main>
  </div>
</template>

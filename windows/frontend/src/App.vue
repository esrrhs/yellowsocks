<script lang="ts" setup>
import { onMounted, onUnmounted, ref } from "vue";
import Home from "./views/Home.vue";
import Connections from "./views/Connections.vue";
import Logs from "./views/Logs.vue";
import Settings from "./views/Settings.vue";
import About from "./views/About.vue";
import { locale, setLocale, t, type Locale } from "./i18n";

const pages = [
  { id: "home", key: "nav.home", icon: "⌂" },
  { id: "connections", key: "nav.connections", icon: "⇄" },
  { id: "logs", key: "nav.logs", icon: "☰" },
  { id: "settings", key: "nav.settings", icon: "⚙" },
  { id: "about", key: "nav.about", icon: "ⓘ" },
];
const current = ref("home");

// Theme: the preference is stored, the resolved value comes from the OS when
// the preference is "system". WebView2 reports the OS setting through
// prefers-color-scheme, so no Win32 call is needed.
type ThemeMode = "system" | "dark" | "light";
const themeMode = ref<ThemeMode>(
  (localStorage.getItem("ys-theme") as ThemeMode) || "system",
);

const themes: { id: ThemeMode; label: string }[] = [
  { id: "system", label: "跟" },
  { id: "dark", label: "暗" },
  { id: "light", label: "亮" },
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

// Applied during setup, not onMounted: the first paint must already have the
// right theme or a light-theme user sees a dark flash on every launch.
applyTheme();

onMounted(() => {
  mq.addEventListener("change", applyTheme);
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
          <span class="nav-icon">{{ p.icon }}</span>{{ t(p.key) }}
        </button>
      </nav>
      <div class="sidebar-foot">
        <div class="mini-switch">
          <button
            v-for="th in themes"
            :key="th.id"
            :class="{ active: themeMode === th.id }"
            :title="th.id"
            @click="setTheme(th.id)"
          >
            {{ th.label }}
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
      <Home v-if="current === 'home'" />
      <Connections v-else-if="current === 'connections'" />
      <Logs v-else-if="current === 'logs'" />
      <Settings v-else-if="current === 'settings'" />
      <About v-else />
    </main>
  </div>
</template>

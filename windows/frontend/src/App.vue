<script lang="ts" setup>
import { ref } from "vue";
import Home from "./views/Home.vue";
import Settings from "./views/Settings.vue";
import About from "./views/About.vue";

const pages = [
  { id: "home", label: "主页", icon: "⌂" },
  { id: "settings", label: "设置", icon: "⚙" },
  { id: "about", label: "关于", icon: "ⓘ" },
];
const current = ref("home");
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
          <span class="nav-icon">{{ p.icon }}</span>{{ p.label }}
        </button>
      </nav>
    </aside>
    <main class="content">
      <Home v-if="current === 'home'" />
      <Settings v-else-if="current === 'settings'" />
      <About v-else />
    </main>
  </div>
</template>

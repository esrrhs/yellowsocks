<script lang="ts" setup>
import { computed, nextTick, onMounted, onUnmounted, ref } from "vue";
import { GetLogs, GetLogPath } from "../../wailsjs/go/guiapp/App";
import { t } from "../i18n";

// loggo writes one file per level, so "all" is a merge of four files served
// oldest-first by the backend. The view re-reads the tail every second instead
// of streaming: the file is the source of truth, and a read cannot drift.
type Level = "" | "INFO" | "WARN" | "ERROR" | "DEBUG";

const level = ref<Level>("");
const lines = ref<string[]>([]);
const query = ref("");
const follow = ref(true);
const logDir = ref("");
const box = ref<HTMLElement | null>(null);

let timer: number | undefined;

interface Parsed {
  level: string;
  time: string;
  file: string;
  msg: string;
}

// A loggo line is "[LEVEL] [ISO time] [file:line] [func] message" (the func
// bracket is dropped from the display: it is the same for every line of a
// burst and eats the whole width).
function parse(line: string): Parsed {
  const m = line.match(/^\[(\w+)\] \[([^\]]+)\] \[([^\]]*)\] \[[^\]]*\] ?([\s\S]*)$/);
  if (!m) return { level: "", time: "", file: "", msg: line };
  const iso = m[2];
  const time = iso.length >= 19 ? iso.slice(11, 23) : iso;
  return { level: m[1], time, file: m[3], msg: m[4] };
}

const parsed = computed(() => lines.value.map(parse));

const shown = computed(() => {
  const q = query.value.trim().toLowerCase();
  if (!q) return parsed.value;
  return parsed.value.filter(
    (p) =>
      p.msg.toLowerCase().includes(q) ||
      p.level.toLowerCase().includes(q) ||
      p.file.toLowerCase().includes(q),
  );
});

function onScroll() {
  const el = box.value;
  if (!el) return;
  // Sticking to the bottom only while the user is already there: otherwise a
  // refresh would yank the view away from whatever line is being read.
  follow.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
}

async function load() {
  try {
    lines.value = (await GetLogs(level.value, 500)) ?? [];
  } catch {
    lines.value = [];
  }
  if (follow.value) {
    await nextTick();
    const el = box.value;
    if (el) el.scrollTop = el.scrollHeight;
  }
}

function pick(lv: Level) {
  level.value = lv;
  void load();
}

function levelClass(lv: string): string {
  const l = lv.toLowerCase();
  if (l === "error") return "lv-error";
  if (l === "warn") return "lv-warn";
  if (l === "debug") return "lv-debug";
  return "lv-info";
}

onMounted(async () => {
  try {
    logDir.value = await GetLogPath();
  } catch {
    logDir.value = "";
  }
  await load();
  timer = window.setInterval(load, 1000);
});

onUnmounted(() => {
  if (timer) window.clearInterval(timer);
});
</script>

<template>
  <section class="page">
    <div class="toolbar">
      <div class="seg">
        <button :class="{ active: level === '' }" @click="pick('')">{{ t("conn.all") }}</button>
        <button :class="{ active: level === 'INFO' }" @click="pick('INFO')">
          INFO
        </button>
        <button :class="{ active: level === 'WARN' }" @click="pick('WARN')">
          WARN
        </button>
        <button :class="{ active: level === 'ERROR' }" @click="pick('ERROR')">
          ERROR
        </button>
        <button :class="{ active: level === 'DEBUG' }" @click="pick('DEBUG')">
          DEBUG
        </button>
      </div>
      <input v-model="query" class="search" :placeholder="t('logs.search')" />
      <span class="toolbar-meta">{{ t("logs.rows", { n: shown.length }) }}</span>
    </div>

    <div ref="box" class="card log-box" @scroll="onScroll">
      <div
        v-for="(p, i) in shown"
        :key="i"
        class="log-line"
        :title="lines[i]"
      >
        <span class="log-time">{{ p.time }}</span>
        <span class="log-level" :class="levelClass(p.level)">{{ p.level }}</span>
        <span class="log-msg">{{ p.msg }}</span>
      </div>
      <div v-if="!shown.length" class="empty-row">
        {{ t("logs.empty") }}
      </div>
    </div>

    <p class="hint log-hint" v-if="logDir">
      {{ t("logs.dir", { path: logDir }) }}
    </p>
  </section>
</template>

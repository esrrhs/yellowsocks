<script lang="ts" setup>
import { computed, onMounted, onUnmounted, ref } from "vue";
import { GetErrors, GetNodes, GetSessions } from "../../wailsjs/go/guiapp/App";
import { stats } from "../../wailsjs/go/models";
import { fmtBps, fmtBytes, fmtAge, fmtClock } from "../format";
import { t } from "../i18n";

type Tab = "conns" | "nodes" | "errors";

const tab = ref<Tab>("conns");
const sessions = ref<stats.SessionView[]>([]);
const nodes = ref<stats.NodeStat[]>([]);
const errors = ref<stats.ErrorEntry[]>([]);
const query = ref("");
const decision = ref<"all" | "proxy" | "direct">("all");

let timer: number | undefined;

async function refresh() {
  // Each binding returns nil when the engine is stopped, which is also what an
  // empty list looks like here, so the tables just go blank.
  try {
    if (tab.value === "conns") {
      sessions.value = (await GetSessions()) ?? [];
    } else if (tab.value === "nodes") {
      nodes.value = (await GetNodes()) ?? [];
    } else {
      errors.value = (await GetErrors()) ?? [];
    }
  } catch {
    // Engine stopped between the switch and the call; the next tick recovers.
  }
}

function switchTab(next: Tab) {
  if (tab.value === next) return;
  tab.value = next;
  void refresh();
}

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase();
  return sessions.value.filter((s) => {
    if (decision.value !== "all" && s.decision !== decision.value) return false;
    if (!q) return true;
    return (
      (s.host ?? "").toLowerCase().includes(q) ||
      (s.client ?? "").toLowerCase().includes(q) ||
      (s.node ?? "").toLowerCase().includes(q)
    );
  });
});

const nodeTotals = computed(() => {
  let up = 0;
  let down = 0;
  for (const n of nodes.value) {
    up += n.upload_bytes ?? 0;
    down += n.download_bytes ?? 0;
  }
  return { up, down };
});

function decisionLabel(d: string): string {
  if (d === "proxy") return t("conn.proxy");
  if (d === "direct") return t("conn.direct");
  return "?";
}

onMounted(async () => {
  await refresh();
  timer = window.setInterval(refresh, 1000);
});

onUnmounted(() => {
  if (timer) window.clearInterval(timer);
});
</script>

<template>
  <section class="page">
    <div class="tabs">
      <button
        class="tab"
        :class="{ active: tab === 'conns' }"
        @click="switchTab('conns')"
      >
        {{ t("conn.tabConns") }}
        <span class="badge" v-if="sessions.length">{{ sessions.length }}</span>
      </button>
      <button
        class="tab"
        :class="{ active: tab === 'nodes' }"
        @click="switchTab('nodes')"
      >
        {{ t("conn.tabNodes") }}
      </button>
      <button
        class="tab"
        :class="{ active: tab === 'errors' }"
        @click="switchTab('errors')"
      >
        {{ t("conn.tabErrors") }}
        <span class="badge bad" v-if="errors.length">{{ errors.length }}</span>
      </button>
    </div>

    <template v-if="tab === 'conns'">
      <div class="toolbar">
        <input v-model="query" class="search" :placeholder="t('conn.search')" />
        <div class="seg">
          <button :class="{ active: decision === 'all' }" @click="decision = 'all'">
            {{ t("conn.all") }}
          </button>
          <button
            :class="{ active: decision === 'proxy' }"
            @click="decision = 'proxy'"
          >
            {{ t("conn.proxy") }}
          </button>
          <button
            :class="{ active: decision === 'direct' }"
            @click="decision = 'direct'"
          >
            {{ t("conn.direct") }}
          </button>
        </div>
        <span class="toolbar-meta">
          {{ t("conn.count", { n: filtered.length, m: sessions.length }) }}
        </span>
      </div>

      <div class="card table-card">
        <table class="tbl">
          <thead>
            <tr>
              <th>{{ t("conn.colHost") }}</th>
              <th>{{ t("conn.colClient") }}</th>
              <th>{{ t("conn.colDecision") }}</th>
              <th>{{ t("conn.colNode") }}</th>
              <th class="right">{{ t("conn.colRate") }}</th>
              <th class="right">{{ t("conn.colAge") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="s in filtered" :key="s.id">
              <td class="mono">
                {{ s.host || "—" }}<span class="dim">:{{ s.port }}</span>
              </td>
              <td class="dim ellipsis">{{ s.client || "—" }}</td>
              <td>
                <span class="tag" :class="s.decision || 'none'">
                  {{ decisionLabel(s.decision) }}
                </span>
              </td>
              <td class="dim ellipsis">{{ s.node || "—" }}</td>
              <td class="right num">
                <span class="up">↑{{ fmtBps(s.upload_bps) }}</span>
                <span class="down">↓{{ fmtBps(s.download_bps) }}</span>
              </td>
              <td class="right num dim">{{ fmtAge(s.age_ms) }}</td>
            </tr>
            <tr v-if="!filtered.length">
              <td colspan="6" class="empty-row">
                {{ sessions.length ? t("conn.noMatch") : t("conn.empty") }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>

    <template v-else-if="tab === 'nodes'">
      <div class="card table-card">
        <table class="tbl">
          <thead>
            <tr>
              <th>{{ t("conn.nodeTitle") }}</th>
              <th class="right">{{ t("conn.colNodeConns") }}</th>
              <th class="right">{{ t("conn.colNodeUp") }}</th>
              <th class="right">{{ t("conn.colNodeDown") }}</th>
              <th class="right">{{ t("conn.colNodeSeen") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="n in nodes" :key="n.name">
              <td class="mono">{{ n.name || t("conn.defaultNode") }}</td>
              <td class="right num">{{ n.conns ?? 0 }}</td>
              <td class="right num">{{ fmtBytes(n.upload_bytes) }}</td>
              <td class="right num">{{ fmtBytes(n.download_bytes) }}</td>
              <td class="right num dim">{{ fmtClock(n.last_seen) }}</td>
            </tr>
            <tr v-if="!nodes.length">
              <td colspan="5" class="empty-row">{{ t("conn.noNodes") }}</td>
            </tr>
            <tr v-else class="sum-row">
              <td>{{ t("conn.sum") }}</td>
              <td></td>
              <td class="right num">{{ fmtBytes(nodeTotals.up) }}</td>
              <td class="right num">{{ fmtBytes(nodeTotals.down) }}</td>
              <td></td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>

    <template v-else>
      <div class="card table-card">
        <table class="tbl">
          <thead>
            <tr>
              <th class="narrow">{{ t("conn.colTime") }}</th>
              <th>{{ t("conn.colHost") }}</th>
              <th class="narrow">{{ t("conn.colScope") }}</th>
              <th>{{ t("conn.colDecision") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(e, i) in errors" :key="`${e.t}-${i}`">
              <td class="num dim">{{ e.time || fmtClock(e.t) }}</td>
              <td class="mono ellipsis">{{ e.host || "—" }}</td>
              <td class="dim">{{ e.scope || "—" }}</td>
              <td class="err-msg">{{ e.message }}</td>
            </tr>
            <tr v-if="!errors.length">
              <td colspan="4" class="empty-row">{{ t("conn.noErrors") }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </section>
</template>

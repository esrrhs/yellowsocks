<script lang="ts" setup>
import { computed, onActivated, onDeactivated, onMounted, onUnmounted, ref } from "vue";
import { load as yamlLoad, dump as yamlDump } from "js-yaml";
import { GetConfig, GetNodes, GetNodeStatuses, SaveConfig } from "../../wailsjs/go/guiapp/App";
import { sppclient, stats } from "../../wailsjs/go/models";
import { fmtBytes, fmtClock } from "../format";
import { errText, t } from "../i18n";

// The upstream node pool. One place edits the config file's `nodes:` list:
// multiple entries form the priority tiers the CLI supports (lower number
// wins, same-tier falls back by bandwidth, first tier preempts when it
// recovers), and each entry is either an SPP channel or a direct SOCKS5
// server with its own credentials.
//
// Saving replaces the whole list and removes the legacy flat upstream keys
// (spp_server, spp_proto, ...): once `nodes` exists the engine ignores the
// flat form, and leaving both is how double bookkeeping starts.

interface NodeForm {
  uid: number; // internal: stable expand/remove key across edits
  name: string;
  type: "spp" | "socks5";
  server: string;
  port: string; // socks5 only; kept as text so an empty field stays empty
  server_proto: string;
  key: string;
  username: string;
  password: string;
  priority: string;
  bandwidth: string;
}

const raw = ref("");
const doc = ref<Record<string, unknown>>({});
const nodes = ref<NodeForm[]>([]);
const message = ref("");
const error = ref("");

let uidSeq = 0;
const nextUID = () => ++uidSeq;
const expanded = ref(new Set<number>());
const toggleNode = (uid: number) => {
  const next = new Set(expanded.value);
  if (next.has(uid)) next.delete(uid);
  else next.add(uid);
  expanded.value = next;
};
const isOpen = (uid: number) => expanded.value.has(uid);
// Live per-node probe state (real request through the node), keyed by node
// name; refreshed on a slower clock than the connections page because each
// probe performs a full SOCKS5 handshake plus an HTTP GET per node.
const statuses = ref<Record<string, sppclient.NodeStatus>>({});
let statusTimer: number | undefined;

// Per-node byte counters, the table that used to live in the connections
// page. Polled alongside the probe statuses: cumulative totals do not need a
// faster clock.
const traffic = ref<stats.NodeStat[]>([]);

async function refreshTraffic() {
  try {
    traffic.value = (await GetNodes()) ?? [];
  } catch {
    // engine stopped: keep the last known numbers
  }
}

async function refreshStatuses() {
  void refreshTraffic();
  try {
    const list = (await GetNodeStatuses()) ?? [];
    const map: Record<string, sppclient.NodeStatus> = {};
    for (const st of list) map[st.name] = st;
    statuses.value = map;
  } catch {
    // engine stopped or binding unavailable: keep the last known state
  }
}

const trafficTotals = computed(() => {
  let up = 0;
  let down = 0;
  for (const n of traffic.value) {
    up += n.upload_bytes ?? 0;
    down += n.download_bytes ?? 0;
  }
  return { up, down };
});

function startStatusPolling() {
  if (statusTimer) return;
  statusTimer = window.setInterval(refreshStatuses, 3000);
}
function stopStatusPolling() {
  if (statusTimer) window.clearInterval(statusTimer);
  statusTimer = undefined;
}

let statusBooted = false;

onMounted(async () => {
  await load();
  statusBooted = true;
  await refreshStatuses();
});

onActivated(() => {
  if (statusBooted) void refreshStatuses();
  startStatusPolling();
});

onDeactivated(stopStatusPolling);

onUnmounted(stopStatusPolling);

function statusOf(name: string): sppclient.NodeStatus | undefined {
  return statuses.value[name];
}

// nodeAddr is the collapsed row's one-line summary of where the node dials.
function nodeAddr(n: NodeForm): string {
  if (n.type === "socks5") {
    return n.port ? `${n.server}:${n.port}` : n.server;
  }
  return n.server;
}

// True once at least one probe round has landed: before that the cards show
// no verdict instead of a misleading "down".
const statusesReady = computed(() => Object.keys(statuses.value).length > 0);

const sppProtos = [
  "tcp", "udp", "rudp", "ricmp", "kcp", "quic", "rhttp", "rdns", "pingtunnel",
];

// The flat upstream keys this page owns once it saves. The engine's
// single-upstream form is migrated into the pool on load, so removing them
// here is what keeps one source of truth.
const flatUpstreamKeys = [
  "spp_server", "spp_proto", "spp_key", "spp_username", "spp_password",
  "spp_encrypt", "spp_compress", "spp_rdnsdomain", "spp_rdnsupstream",
  "spp_rdnspsk", "spp_rdnsquerytype",
];

function splitHostPort(addr: string): [string, number] {
  const a = (addr ?? "").trim();
  if (!a) return ["", 0];
  const i = a.lastIndexOf(":");
  if (i < 0) return [a, 0];
  const host = a.slice(0, i).replace(/^\[|\]$/g, "");
  const port = Number(a.slice(i + 1));
  return [host, Number.isFinite(port) ? port : 0];
}

// First run on a config that still uses the single-upstream form: carry it
// into the pool as tier 1, so the user sees what they had instead of an
// empty list.
function seedFromFlat(d: Record<string, unknown>): NodeForm[] {
  const server = String(d.spp_server ?? "").trim();
  if (!server) return [];
  const proto = String(d.spp_proto ?? "tcp").trim() || "tcp";
  if (proto === "socks5") {
    const [host, port] = splitHostPort(server);
    return [{
      uid: nextUID(),
      name: t("nodes.mainName"),
      type: "socks5",
      server: host,
      port: port ? String(port) : "",
      server_proto: "",
      key: "",
      username: String(d.spp_username ?? ""),
      password: String(d.spp_password ?? ""),
      priority: "1",
      bandwidth: "",
    }];
  }
  return [{
    uid: nextUID(),
    name: t("nodes.mainName"),
    type: "spp",
    server,
    port: "",
    server_proto: proto,
    key: String(d.spp_key ?? ""),
    username: "",
    password: "",
    priority: "1",
    bandwidth: "",
  }];
}

function nodesFromDoc(d: Record<string, unknown>): NodeForm[] {
  const list = d.nodes;
  if (!Array.isArray(list) || list.length === 0) {
    return seedFromFlat(d);
  }
  return list.map((raw: any) => ({
    uid: nextUID(),
    name: String(raw?.name ?? ""),
    type: String(raw?.type ?? "spp").toLowerCase() === "socks5" ? "socks5" : "spp",
    server: String(raw?.server ?? ""),
    port: raw?.port ? String(raw.port) : "",
    server_proto: String(raw?.server_proto ?? "tcp"),
    key: String(raw?.key ?? ""),
    username: String(raw?.username ?? ""),
    password: String(raw?.password ?? ""),
    priority: raw?.priority ? String(raw.priority) : "",
    bandwidth: raw?.bandwidth ? String(raw.bandwidth) : "",
  }));
}

async function load() {
  error.value = "";
  message.value = "";
  try {
    raw.value = await GetConfig();
    doc.value = (yamlLoad(raw.value) as Record<string, unknown>) ?? {};
    nodes.value = nodesFromDoc(doc.value);
  } catch (e: any) {
    error.value = errText(e);
  }
}

// Tiers group the pool the way the scheduler consumes it: lower priority
// number first, bandwidth larger first inside a tier.
const tiers = computed(() => {
  const prio = (n: NodeForm) => {
    const p = Number(n.priority);
    return Number.isFinite(p) && p > 0 ? p : 10;
  };
  const map = new Map<number, { priority: number; items: NodeForm[] }>();
  for (const n of nodes.value) {
    const p = prio(n);
    if (!map.has(p)) map.set(p, { priority: p, items: [] });
    map.get(p)!.items.push(n);
  }
  return [...map.values()]
    .sort((a, b) => a.priority - b.priority)
    .map((g) => ({ ...g, items: [...g.items].sort((a, b) => Number(b.bandwidth || 0) - Number(a.bandwidth || 0)) }));
});

const pristine = ref("");

function serialize(list: NodeForm[]): string {
  return JSON.stringify(list);
}

function addNode() {
  // A new node lands in the next free tier so "add a backup" does the
  // natural thing: it becomes the failover target of everything above it.
  const used = new Set(nodes.value.map((n) => Number(n.priority) || 10));
  let p = 1;
  while (used.has(p)) p++;
  const n: NodeForm = {
    uid: nextUID(),
    name: "",
    type: "socks5",
    server: "",
    port: "",
    server_proto: "tcp",
    key: "",
    username: "",
    password: "",
    priority: String(p),
    bandwidth: "",
  };
  nodes.value.push(n);
  // A fresh node is useless collapsed: open it for editing right away.
  expanded.value = new Set([...expanded.value, n.uid]);
}

function removeNode(uid: number) {
  nodes.value = nodes.value.filter((n) => n.uid !== uid);
  const next = new Set(expanded.value);
  next.delete(uid);
  expanded.value = next;
}

function validate(): string {
  for (const [i, n] of nodes.value.entries()) {
    if (!n.name.trim()) return t("nodes.errName", { n: i + 1 });
    if (!n.server.trim()) return t("nodes.errServer", { n: i + 1 });
    if (n.type === "socks5" && !Number(n.port)) return t("nodes.errPort", { n: i + 1 });
  }
  return "";
}

function toYamlNode(n: NodeForm): Record<string, unknown> {
  const num = (v: string) => {
    const p = Number(v);
    return Number.isFinite(p) && p > 0 ? p : undefined;
  };
  if (n.type === "socks5") {
    const out: Record<string, unknown> = {
      name: n.name.trim(),
      type: "socks5",
      server: n.server.trim(),
      port: num(n.port) ?? 0,
      priority: num(n.priority) ?? 10,
    };
    if (n.username.trim()) out.username = n.username.trim();
    if (n.password) out.password = n.password;
    if (num(n.bandwidth)) out.bandwidth = num(n.bandwidth);
    return out;
  }
  const out: Record<string, unknown> = {
    name: n.name.trim(),
    server: n.server.trim(),
    server_proto: n.server_proto || "tcp",
    priority: num(n.priority) ?? 10,
  };
  if (n.key) out.key = n.key;
  if (n.server_proto === "rdns") {
    // rdns carries its own settings; the node page exposes the essentials
    // and leaves exotic fields to the YAML mode in settings.
  }
  if (num(n.bandwidth)) out.bandwidth = num(n.bandwidth);
  return out;
}

async function save() {
  error.value = "";
  message.value = "";
  const problem = validate();
  if (problem) {
    error.value = problem;
    return;
  }
  const list = nodes.value.map(toYamlNode);
  const out: Record<string, unknown> = { ...doc.value };
  if (list.length > 0) out.nodes = list;
  else delete out.nodes;
  for (const k of flatUpstreamKeys) delete out[k];
  const content = yamlDump(out, { lineWidth: -1 });
  try {
    await SaveConfig(content);
    message.value = t("nodes.saved");
    raw.value = content;
    doc.value = (yamlLoad(content) as Record<string, unknown>) ?? {};
    nodes.value = nodesFromDoc(doc.value);
  } catch (e: any) {
    error.value = errText(e);
  }
}

const dirty = computed(() => pristine.value !== "" && serialize(nodes.value) !== pristine.value);

onMounted(async () => {
  await load();
  pristine.value = serialize(nodes.value);
  statusBooted = true;
});
</script>

<template>
  <section class="page">
    <div class="card chart-card">
      <div class="chart-head">
        <span>{{ t("nodes.title") }}</span>
        <span class="chart-head-actions">
          <button class="btn small" @click="addNode">{{ t("nodes.add") }}</button>
          <button class="btn small primary" :disabled="!dirty" @click="save">
            {{ t("settings.save") }}
          </button>
        </span>
      </div>
      <p class="hint">{{ t("nodes.hint") }}</p>

      <div class="node-tier" v-for="tier in tiers" :key="tier.priority">
        <div class="dest-side-head">
          <span>{{ t("nodes.tier", { n: tier.priority }) }}</span>
          <span class="node-tier-note">{{ t("nodes.tierNote") }}</span>
        </div>
        <div class="node-list">
          <div class="node-item" v-for="n in tier.items" :key="n.uid" :class="{ socks5: n.type === 'socks5', open: isOpen(n.uid) }">
            <button class="node-row" @click="toggleNode(n.uid)">
              <span class="node-caret">{{ isOpen(n.uid) ? "▾" : "▸" }}</span>
              <span class="node-name">{{ n.name || t("nodes.unnamed") }}</span>
              <span class="node-type" :class="n.type === 'socks5' ? 'socks5' : 'spp'">
                {{ n.type === "socks5" ? t("nodes.typeSocks5") : t("nodes.typeSpp") }}
              </span>
              <span class="node-addr">{{ nodeAddr(n) }}</span>
              <span class="node-status" v-if="statusOf(n.name)" :class="statusOf(n.name)!.real_ok ? 'ok' : 'down'">
                <i class="dot" :class="statusOf(n.name)!.real_ok ? 'on' : 'err'"></i>
                <span class="node-ms" v-if="statusOf(n.name)!.real_ok">{{ statusOf(n.name)!.real_ms }}ms</span>
                <span v-else>{{ t("nodes.statusDown") }}</span>
              </span>
              <span class="node-status idle" v-else-if="statusesReady">{{ t("nodes.statusIdle") }}</span>
              <button class="node-del" :title="t('nodes.remove')" @click.stop="removeNode(n.uid)">✕</button>
            </button>
            <div class="node-edit" v-if="isOpen(n.uid)">
              <div class="form-grid">
                <label>{{ t("nodes.name") }}</label>
                <input v-model="n.name" :placeholder="'My-Socks5'" />

                <label>{{ t("nodes.server") }}</label>
                <input v-model="n.server" :placeholder="n.type === 'socks5' ? 'sh.example.com' : '1.2.3.4:8888'" />

                <template v-if="n.type === 'socks5'">
                  <label>{{ t("nodes.port") }}</label>
                  <input v-model="n.port" type="number" placeholder="1080" />
                  <label>{{ t("settings.sppUsername") }}</label>
                  <input v-model="n.username" :placeholder="t('settings.ph.authOptional')" />
                  <label>{{ t("settings.sppPassword") }}</label>
                  <input v-model="n.password" type="password" :placeholder="t('settings.ph.authOptional')" />
                </template>

                <template v-else>
                  <label>{{ t("settings.sppProto") }}</label>
                  <select v-model="n.server_proto">
                    <option v-for="p in sppProtos" :key="p" :value="p">{{ p }}</option>
                  </select>
                  <label>{{ t("settings.sppKey") }}</label>
                  <input v-model="n.key" :placeholder="t('settings.ph.authOptional')" />
                </template>

                <label>{{ t("nodes.priority") }}</label>
                <input v-model="n.priority" type="number" min="1" placeholder="1" />
                <label>{{ t("nodes.bandwidth") }}</label>
                <input v-model="n.bandwidth" type="number" min="1" placeholder="100" />
              </div>
            </div>
          </div>
        </div>
      </div>

      <p class="hint" v-if="nodes.length === 0">{{ t("nodes.noData") }}</p>

      <div v-if="error" class="error-banner">{{ error }}</div>
      <div v-if="message" class="ok-banner">{{ message }}</div>
    </div>

    <div class="card table-card traffic-card">
      <div class="chart-head traffic-head">
        <span>{{ t("nodes.trafficTitle") }}</span>
      </div>
      <table class="tbl">
        <thead>
          <tr>
            <th>{{ t("conn.colNode") }}</th>
            <th class="right">{{ t("nodes.colConns") }}</th>
            <th class="right">{{ t("nodes.colUp") }}</th>
            <th class="right">{{ t("nodes.colDown") }}</th>
            <th class="right">{{ t("nodes.colSeen") }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="n in traffic" :key="n.name">
            <td class="mono">{{ n.name || t("nodes.unnamed") }}</td>
            <td class="right num">{{ n.conns ?? 0 }}</td>
            <td class="right num">{{ fmtBytes(n.upload_bytes) }}</td>
            <td class="right num">{{ fmtBytes(n.download_bytes) }}</td>
            <td class="right num dim">{{ fmtClock(n.last_seen) }}</td>
          </tr>
          <tr v-if="!traffic.length">
            <td colspan="5" class="empty-row">{{ t("nodes.trafficEmpty") }}</td>
          </tr>
          <tr v-else class="sum-row">
            <td>{{ t("nodes.trafficSum") }}</td>
            <td></td>
            <td class="right num">{{ fmtBytes(trafficTotals.up) }}</td>
            <td class="right num">{{ fmtBytes(trafficTotals.down) }}</td>
            <td></td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

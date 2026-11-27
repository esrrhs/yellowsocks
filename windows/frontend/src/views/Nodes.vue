<script lang="ts" setup>
import { computed, onMounted, ref } from "vue";
import { load as yamlLoad, dump as yamlDump } from "js-yaml";
import { GetConfig, SaveConfig } from "../../wailsjs/go/guiapp/App";
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
  nodes.value.push({
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
  });
}

function removeNode(i: number) {
  nodes.value.splice(i, 1);
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
        <div class="node-card" v-for="(n, i) in tier.items" :key="i" :class="{ socks5: n.type === 'socks5' }">
          <div class="node-card-head">
            <span class="node-type" :class="n.type === 'socks5' ? 'socks5' : 'spp'">
              {{ n.type === "socks5" ? t("nodes.typeSocks5") : t("nodes.typeSpp") }}
            </span>
            <button class="node-del" :title="t('nodes.remove')" @click="removeNode(nodes.indexOf(n))">✕</button>
          </div>
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

      <p class="hint" v-if="nodes.length === 0">{{ t("nodes.noData") }}</p>

      <div v-if="error" class="error-banner">{{ error }}</div>
      <div v-if="message" class="ok-banner">{{ message }}</div>
    </div>
  </section>
</template>

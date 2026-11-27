<script lang="ts" setup>
import { computed, onMounted, ref } from "vue";
import { load as yamlLoad, dump as yamlDump } from "js-yaml";
import { GetConfig, SaveConfig } from "../../wailsjs/go/guiapp/App";
import { errText, t } from "../i18n";
import type { MsgKey } from "../i18n";

// The form edits the same flat keys as the CLI config file (FileConfig in
// core/config). Every field not covered by a form section is still editable
// through the YAML source mode, and unknown keys survive form saves because
// saving merges onto the freshly parsed document.
//
// Labels are translated; the config key is shown next to them as code. That is
// the one piece of English that belongs in both languages: it is the literal
// key in config.yaml, not a gloss, so translating it would only hide which
// line of the file the field writes.
//
// The fields are split into tabs, and a save writes only the active tab's
// keys. That is deliberate: someone changing the upstream server should not
// have to read (or silently commit) six other pages of settings, and a
// half-finished edit on another tab stays in the form instead of being
// overwritten by a save from this one.

type Form = Record<string, any>;
type GroupId = "listen" | "dns" | "routing" | "auth" | "api";

interface Group {
  id: GroupId;
  titleKey: MsgKey;
  keys: string[];
}

const groups: Group[] = [
  {
    id: "listen",
    titleKey: "settings.sectionListen",
    keys: ["socks5_listen", "http_listen", "dns_listen", "doh_listen", "dot_listen", "ipv6"],
  },
  { id: "dns", titleKey: "settings.sectionDns", keys: ["direct_dns", "doh_url"] },
  {
    id: "routing",
    titleKey: "settings.sectionRouting",
    keys: ["direct_domains", "direct_cidrs", "skip_country", "geoip_file", "china_domains", "gfw_domains"],
  },
  {
    id: "auth",
    titleKey: "settings.sectionTls",
    keys: ["proxy_username", "proxy_password", "tls_cert", "tls_key"],
  },
  {
    id: "api",
    titleKey: "settings.sectionApi",
    keys: [
      "api_listen", "api_token", "api_readonly_token", "api_dashboard",
      "api_pprof", "stats_file", "stats_history_hours", "loglevel",
    ],
  },
];

const mode = ref<"form" | "yaml">("form");
const group = ref<GroupId>("listen");
const raw = ref(""); // authoritative YAML text (source of truth)
const doc = ref<Record<string, unknown>>({});
const form = ref<Form>({});
const message = ref("");
const error = ref("");

const loglevels = ["debug", "info", "warn", "error"];

const activeGroup = computed(() => groups.find((g) => g.id === group.value) ?? groups[0]);

// "Allow LAN" is not a separate config key: it is the listen address of the
// three inbound services. Exposing it as one switch keeps the user from having
// to edit three strings and get them inconsistent (one bound to 0.0.0.0 and the
// other two still on loopback is a confusing half-open proxy).
function splitHostPort(addr: string): [string, string] {
  if (!addr) return ["", ""];
  const i = addr.lastIndexOf(":");
  if (i < 0) return [addr, ""];
  return [addr.slice(0, i), addr.slice(i)];
}

const allowLAN = computed({
  get: () => {
    const addrs = [form.value.socks5_listen, form.value.http_listen, form.value.dns_listen];
    return addrs.some((a) => typeof a === "string" && a.startsWith("0.0.0.0:"));
  },
  set: (on: boolean) => {
    const host = on ? "0.0.0.0" : "127.0.0.1";
    for (const key of ["socks5_listen", "http_listen", "dns_listen"]) {
      const [h, port] = splitHostPort(String(form.value[key] ?? ""));
      // A field the user left empty stays empty (service disabled) rather than
      // being turned into a listener they never asked for.
      if (h) form.value[key] = host + port;
    }
  },
});

async function load() {
  error.value = "";
  message.value = "";
  try {
    raw.value = await GetConfig();
    parseDoc();
  } catch (e: any) {
    error.value = errText(e);
  }
}

// One place maps document -> form, so "has this tab been edited" can be
// answered by comparing against the same conversion instead of a second,
// drifting copy of the defaults.
function formFromDoc(d: Record<string, unknown>): Form {
  const lines = (v: unknown) => (Array.isArray(v) ? v.join("\n") : (v as string) ?? "");
  return {
    socks5_listen: (d.socks5_listen as string) ?? "",
    http_listen: (d.http_listen as string) ?? "",
    dns_listen: (d.dns_listen as string) ?? "",
    doh_listen: (d.doh_listen as string) ?? "",
    dot_listen: (d.dot_listen as string) ?? "",
    ipv6: !!d.ipv6,
    proxy_username: (d.proxy_username as string) ?? "",
    proxy_password: (d.proxy_password as string) ?? "",
    tls_cert: (d.tls_cert as string) ?? "",
    tls_key: (d.tls_key as string) ?? "",
    direct_dns: (d.direct_dns as string) ?? "",
    doh_url: (d.doh_url as string) ?? "",
    direct_domains: lines(d.direct_domains),
    direct_cidrs: lines(d.direct_cidrs),
    skip_country: (d.skip_country as string) ?? "",
    geoip_file: (d.geoip_file as string) ?? "",
    china_domains: (d.china_domains as string) ?? "",
    gfw_domains: (d.gfw_domains as string) ?? "",
    api_listen: (d.api_listen as string) ?? "",
    api_token: (d.api_token as string) ?? "",
    api_readonly_token: (d.api_readonly_token as string) ?? "",
    api_dashboard: d.api_dashboard as boolean | undefined,
    api_pprof: !!d.api_pprof,
    stats_file: (d.stats_file as string) ?? "",
    stats_history_hours: d.stats_history_hours,
    loglevel: (d.loglevel as string) || "info",
  };
}

function parseDoc() {
  try {
    doc.value = (yamlLoad(raw.value) as Record<string, unknown>) ?? {};
  } catch (e: any) {
    error.value = t("settings.yamlParseFailed", { msg: String(e?.message ?? e) });
    doc.value = {};
  }
  form.value = formFromDoc(doc.value);
}

// Writers turn one form field into one document key. Only a handful need a
// conversion (numbers, line lists, switches); everything else is copied as-is
// and removed when emptied.
const splitLines = (v: string) =>
  v.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);

function setKey(out: Record<string, unknown>, key: string, v: unknown) {
  const empty =
    v === "" || v === undefined || v === null ||
    (Array.isArray(v) && v.length === 0) ||
    v === false;
  if (empty) {
    delete out[key];
  } else {
    out[key] = v;
  }
}

const writers: Record<string, (out: Record<string, unknown>, f: Form) => void> = {
  stats_history_hours: (out, f) => {
    const v = f.stats_history_hours;
    if (v === undefined || v === null || v === "") delete out.stats_history_hours;
    else out.stats_history_hours = Number(v);
  },
  direct_domains: (out, f) => setKey(out, "direct_domains", splitLines(f.direct_domains ?? "")),
  direct_cidrs: (out, f) => setKey(out, "direct_cidrs", splitLines(f.direct_cidrs ?? "")),
  ipv6: (out, f) => setKey(out, "ipv6", f.ipv6 === true),
  api_dashboard: (out, f) => setKey(out, "api_dashboard", f.api_dashboard === true),
  api_pprof: (out, f) => setKey(out, "api_pprof", f.api_pprof === true),
};

// Merge the given keys onto the parsed document: keys outside the list (the
// other tabs' fields, nodes, api_origins, ...) keep the file's current value.
function applyKeys(keys: string[]): string {
  const out: Record<string, unknown> = { ...doc.value };
  for (const key of keys) {
    const writer = writers[key];
    if (writer) writer(out, form.value);
    else setKey(out, key, form.value[key]);
  }
  return yamlDump(out, { lineWidth: -1 });
}

// What the file currently says, in form shape: the baseline for the "unsaved"
// dot on each tab.
const pristine = computed(() => formFromDoc(doc.value));

function sameValue(a: unknown, b: unknown): boolean {
  // An unchecked switch reads as false in one place and undefined (key absent)
  // in the other; both mean "off".
  if (typeof a === "boolean" || typeof b === "boolean") return !!a === !!b;
  return String(a ?? "") === String(b ?? "");
}

function isDirty(g: Group): boolean {
  const base = pristine.value;
  return g.keys.some((k) => !sameValue(base[k], form.value[k]));
}

async function save() {
  error.value = "";
  message.value = "";
  let content: string;
  if (mode.value === "yaml") {
    content = raw.value;
  } else {
    try {
      content = applyKeys(activeGroup.value.keys);
    } catch (e: any) {
      error.value = t("settings.yamlBuildFailed", { msg: String(e?.message ?? e) });
      return;
    }
  }
  try {
    await SaveConfig(content);
    message.value =
      mode.value === "yaml"
        ? t("settings.saved")
        : t("settings.savedTab", { name: t(activeGroup.value.titleKey) });
    raw.value = content;
    doc.value = (yamlLoad(content) as Record<string, unknown>) ?? {};
  } catch (e: any) {
    error.value = errText(e);
  }
}

function switchMode(m: "form" | "yaml") {
  if (m === mode.value) return;
  error.value = "";
  if (m === "yaml") {
    // carry the edits of every tab into the raw view: the editor shows the
    // whole file, so a single tab would be a lie about what is on screen
    try {
      raw.value = applyKeys(groups.flatMap((g) => g.keys));
    } catch (e: any) {
      error.value = t("settings.yamlBuildFailed", { msg: String(e?.message ?? e) });
      return;
    }
  } else {
    parseDoc();
  }
  mode.value = m;
}

onMounted(load);
</script>

<template>
  <section class="page">
    <div class="settings-head">
      <h2>{{ t("settings.title") }}</h2>
      <div class="mode-switch">
        <button class="btn small" :class="{ active: mode === 'form' }" @click="switchMode('form')">
          {{ t("settings.form") }}
        </button>
        <button class="btn small" :class="{ active: mode === 'yaml' }" @click="switchMode('yaml')">
          {{ t("settings.yaml") }}
        </button>
      </div>
    </div>
    <p class="hint">{{ t("settings.hint") }}</p>

    <template v-if="mode === 'form'">
      <div class="subtabs">
        <button
          v-for="g in groups"
          :key="g.id"
          class="subtab"
          :class="{ active: group === g.id }"
          @click="group = g.id"
        >
          {{ t(g.titleKey) }}
          <span v-if="isDirty(g)" class="dirty-dot"></span>
        </button>
      </div>

      <div v-if="group === 'listen'" class="card section">
        <h3>{{ t("settings.sectionListen") }}</h3>
        <div class="form-grid">
          <label>{{ t("settings.allowLan") }}</label>
          <label class="check-line">
            <input type="checkbox" v-model="allowLAN" />
            <span class="check-note">{{ t("settings.allowLanHint") }}</span>
          </label>
          <label>{{ t("settings.socks5Listen") }} <code>socks5_listen</code></label>
          <input v-model="form.socks5_listen" placeholder="127.0.0.1:10808" />
          <label>{{ t("settings.httpListen") }} <code>http_listen</code></label>
          <input v-model="form.http_listen" placeholder="127.0.0.1:8080" />
          <label>{{ t("settings.dnsListen") }} <code>dns_listen</code></label>
          <input v-model="form.dns_listen" placeholder="127.0.0.1:1053" />
          <label>{{ t("settings.dohListen") }} <code>doh_listen</code></label>
          <input v-model="form.doh_listen" :placeholder="t('settings.ph.disabled')" />
          <label>{{ t("settings.dotListen") }} <code>dot_listen</code></label>
          <input v-model="form.dot_listen" :placeholder="t('settings.ph.dotPort')" />
          <label>{{ t("settings.ipv6") }} <code>ipv6</code></label>
          <input type="checkbox" v-model="form.ipv6" />
        </div>
      </div>

      <div v-else-if="group === 'dns'" class="card section">
        <h3>{{ t("settings.sectionDns") }}</h3>
        <div class="form-grid">
          <label>{{ t("settings.directDns") }} <code>direct_dns</code></label>
          <input v-model="form.direct_dns" placeholder="1.1.1.1:53" />
          <label>{{ t("settings.dohUrl") }} <code>doh_url</code></label>
          <input v-model="form.doh_url" placeholder="https://1.1.1.1/dns-query" />
        </div>
      </div>

      <div v-else-if="group === 'routing'" class="card section">
        <h3>{{ t("settings.sectionRouting") }}</h3>
        <div class="form-grid">
          <label>{{ t("settings.directDomains") }} <code>direct_domains</code></label>
          <textarea v-model="form.direct_domains" class="mini-editor" :placeholder="t('settings.ph.domains')"></textarea>
          <label>{{ t("settings.directCidrs") }} <code>direct_cidrs</code></label>
          <textarea v-model="form.direct_cidrs" class="mini-editor" :placeholder="t('settings.ph.cidrs')"></textarea>
          <label>{{ t("settings.skipCountry") }} <code>skip_country</code></label>
          <input v-model="form.skip_country" :placeholder="t('settings.ph.skipCountry')" />
          <label>{{ t("settings.geoipFile") }} <code>geoip_file</code></label>
          <input v-model="form.geoip_file" :placeholder="t('settings.ph.geoipPath')" />
          <label>{{ t("settings.chinaDomains") }} <code>china_domains</code></label>
          <input v-model="form.china_domains" />
          <label>{{ t("settings.gfwDomains") }} <code>gfw_domains</code></label>
          <input v-model="form.gfw_domains" />
        </div>
      </div>

      <div v-else-if="group === 'auth'" class="card section">
        <h3>{{ t("settings.sectionTls") }}</h3>
        <div class="form-grid">
          <label>{{ t("settings.proxyUsername") }} <code>proxy_username</code></label>
          <input v-model="form.proxy_username" :placeholder="t('settings.ph.authOptional')" />
          <label>{{ t("settings.proxyPassword") }} <code>proxy_password</code></label>
          <input v-model="form.proxy_password" type="password" />
          <label>{{ t("settings.tlsCert") }} <code>tls_cert</code></label>
          <input v-model="form.tls_cert" />
          <label>{{ t("settings.tlsKey") }} <code>tls_key</code></label>
          <input v-model="form.tls_key" />
        </div>
      </div>

      <div v-else class="card section">
        <h3>{{ t("settings.sectionApi") }}</h3>
        <div class="form-grid">
          <label>{{ t("settings.apiListen") }} <code>api_listen</code></label>
          <input v-model="form.api_listen" :placeholder="t('settings.ph.apiListen')" />
          <label>{{ t("settings.apiToken") }} <code>api_token</code></label>
          <input v-model="form.api_token" :placeholder="t('settings.ph.apiToken')" />
          <label>{{ t("settings.apiReadonlyToken") }} <code>api_readonly_token</code></label>
          <input v-model="form.api_readonly_token" />
          <label>{{ t("settings.apiDashboard") }} <code>api_dashboard</code></label>
          <input type="checkbox" v-model="form.api_dashboard" />
          <label>{{ t("settings.apiPprof") }} <code>api_pprof</code></label>
          <input type="checkbox" v-model="form.api_pprof" />
          <label>{{ t("settings.statsFile") }} <code>stats_file</code></label>
          <input v-model="form.stats_file" placeholder="stats.json" />
          <label>{{ t("settings.statsHistoryHours") }} <code>stats_history_hours</code></label>
          <input v-model="form.stats_history_hours" type="number" />
          <label>{{ t("settings.loglevel") }} <code>loglevel</code></label>
          <select v-model="form.loglevel">
            <option v-for="l in loglevels" :key="l" :value="l">{{ l }}</option>
          </select>
        </div>
      </div>
    </template>

    <template v-else>
      <textarea
        v-model="raw"
        class="editor"
        spellcheck="false"
        :aria-label="t('settings.editorLabel')"
      ></textarea>
    </template>

    <div class="actions">
      <button class="btn primary" @click="save">
        {{ mode === "yaml" ? t("settings.save") : t("settings.saveTab") }}
      </button>
      <button class="btn" @click="load">{{ t("settings.reload") }}</button>
      <span v-if="message" class="msg ok">{{ message }}</span>
      <span v-if="error" class="msg err">{{ error }}</span>
    </div>
  </section>
</template>

<script lang="ts" setup>
import { computed, onMounted, ref } from "vue";
import { load as yamlLoad, dump as yamlDump } from "js-yaml";
import { GetConfig, SaveConfig } from "../../wailsjs/go/guiapp/App";
import { errText, t } from "../i18n";

// The form edits the same flat keys as the CLI config file (FileConfig in
// core/config). Every field not covered by a form section is still editable
// through the YAML source mode, and unknown keys survive form saves because
// saving merges onto the freshly parsed document.
//
// Labels are translated; the config key is shown next to them as code. That is
// the one piece of English that belongs in both languages: it is the literal
// key in config.yaml, not a gloss, so translating it would only hide which
// line of the file the field writes.

const mode = ref<"form" | "yaml">("form");
const raw = ref(""); // authoritative YAML text (source of truth)
const doc = ref<Record<string, unknown>>({});
const form = ref<Record<string, any>>({});
const message = ref("");
const error = ref("");

const sppProtos = [
  "tcp", "udp", "rudp", "ricmp", "kcp", "quic", "rhttp", "rdns",
  "pingtunnel", "socks5",
];
const loglevels = ["debug", "info", "warn", "error"];

const proto = computed({
  get: () => (form.value.spp_proto as string) || "tcp",
  set: (v: string) => (form.value.spp_proto = v),
});
const isSocks5 = computed(() => proto.value === "socks5");
const isSPP = computed(() => !isSocks5.value && proto.value !== "pingtunnel");
const isRdns = computed(() => proto.value === "rdns");

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

function parseDoc() {
  try {
    doc.value = (yamlLoad(raw.value) as Record<string, unknown>) ?? {};
  } catch (e: any) {
    error.value = t("settings.yamlParseFailed", { msg: String(e?.message ?? e) });
    doc.value = {};
  }
  const d = doc.value;
  const lines = (v: unknown) => (Array.isArray(v) ? v.join("\n") : (v as string) ?? "");
  form.value = {
    spp_proto: (d.spp_proto as string) || "tcp",
    spp_server: (d.spp_server as string) ?? "",
    spp_key: (d.spp_key as string) ?? "",
    spp_username: (d.spp_username as string) ?? "",
    spp_password: (d.spp_password as string) ?? "",
    spp_encrypt: (d.spp_encrypt as string) ?? "",
    spp_compress: d.spp_compress,
    spp_rdnsdomain: (d.spp_rdnsdomain as string) ?? "",
    spp_rdnsupstream: (d.spp_rdnsupstream as string) ?? "",
    spp_rdnspsk: (d.spp_rdnspsk as string) ?? "",
    spp_rdnsquerytype: (d.spp_rdnsquerytype as string) ?? "",
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

// Merge the edited form fields back onto the parsed document: keys not
// present in the form (nodes, api_origins, ...) survive untouched; emptied
// form fields are removed from the document.
function applyForm(): string {
  const out: Record<string, unknown> = { ...doc.value };
  const f = form.value;
  const set = (key: string, v: unknown) => {
    const empty =
      v === "" || v === undefined || v === null ||
      (Array.isArray(v) && v.length === 0) ||
      v === false;
    if (empty) {
      delete out[key];
    } else {
      out[key] = v;
    }
  };
  const splitLines = (v: string) =>
    v.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);

  set("spp_proto", f.spp_proto);
  set("spp_server", f.spp_server);
  set("spp_key", f.spp_key);
  set("spp_username", f.spp_username);
  set("spp_password", f.spp_password);
  set("spp_encrypt", f.spp_encrypt);
  if (f.spp_compress !== undefined && f.spp_compress !== null && f.spp_compress !== "") {
    set("spp_compress", Number(f.spp_compress));
  } else {
    delete out.spp_compress;
  }
  set("spp_rdnsdomain", f.spp_rdnsdomain);
  set("spp_rdnsupstream", f.spp_rdnsupstream);
  set("spp_rdnspsk", f.spp_rdnspsk);
  set("spp_rdnsquerytype", f.spp_rdnsquerytype);
  set("socks5_listen", f.socks5_listen);
  set("http_listen", f.http_listen);
  set("dns_listen", f.dns_listen);
  set("doh_listen", f.doh_listen);
  set("dot_listen", f.dot_listen);
  set("ipv6", f.ipv6 === true);
  set("proxy_username", f.proxy_username);
  set("proxy_password", f.proxy_password);
  set("tls_cert", f.tls_cert);
  set("tls_key", f.tls_key);
  set("direct_dns", f.direct_dns);
  set("doh_url", f.doh_url);
  set("direct_domains", splitLines(f.direct_domains ?? ""));
  set("direct_cidrs", splitLines(f.direct_cidrs ?? ""));
  set("skip_country", f.skip_country);
  set("geoip_file", f.geoip_file);
  set("china_domains", f.china_domains);
  set("gfw_domains", f.gfw_domains);
  set("api_listen", f.api_listen);
  set("api_token", f.api_token);
  set("api_readonly_token", f.api_readonly_token);
  set("api_dashboard", f.api_dashboard === true);
  set("api_pprof", f.api_pprof === true);
  set("stats_file", f.stats_file);
  if (f.stats_history_hours !== undefined && f.stats_history_hours !== null && f.stats_history_hours !== "") {
    set("stats_history_hours", Number(f.stats_history_hours));
  } else {
    delete out.stats_history_hours;
  }
  set("loglevel", f.loglevel);

  return yamlDump(out, { lineWidth: -1 });
}

async function save() {
  error.value = "";
  message.value = "";
  let content: string;
  if (mode.value === "yaml") {
    content = raw.value;
  } else {
    try {
      content = applyForm();
    } catch (e: any) {
      error.value = t("settings.yamlBuildFailed", { msg: String(e?.message ?? e) });
      return;
    }
  }
  try {
    await SaveConfig(content);
    message.value = t("settings.saved");
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
    // carry current form edits into the raw view
    try {
      raw.value = applyForm();
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
      <div class="card section">
        <h3>{{ t("settings.sectionUpstream") }}</h3>
        <div class="form-grid">
          <label>{{ t("settings.sppProto") }} <code>spp_proto</code></label>
          <select v-model="form.spp_proto">
            <option v-for="p in sppProtos" :key="p" :value="p">{{ p }}</option>
          </select>

          <label>{{ t("settings.sppServer") }} <code>spp_server</code></label>
          <input v-model="form.spp_server" :placeholder="isSocks5 ? '192.168.0.105:1080' : '1.2.3.4:8888'" />

          <template v-if="isSocks5">
            <label>{{ t("settings.sppUsername") }} <code>spp_username</code></label>
            <input v-model="form.spp_username" :placeholder="t('settings.ph.authOptional')" />
            <label>{{ t("settings.sppPassword") }} <code>spp_password</code></label>
            <input v-model="form.spp_password" type="password" :placeholder="t('settings.ph.authOptional')" />
          </template>

          <template v-if="!isSocks5">
            <label>{{ t("settings.sppKey") }} <code>spp_key</code></label>
            <input v-model="form.spp_key" />
          </template>

          <template v-if="isSPP">
            <label>{{ t("settings.sppEncrypt") }} <code>spp_encrypt</code></label>
            <input v-model="form.spp_encrypt" placeholder="default" />
            <label>{{ t("settings.sppCompress") }} <code>spp_compress</code></label>
            <input v-model="form.spp_compress" type="number" placeholder="128" />
          </template>

          <template v-if="isRdns">
            <label>{{ t("settings.rdnsDomain") }} <code>spp_rdnsdomain</code></label>
            <input v-model="form.spp_rdnsdomain" placeholder="tunnel.example.com" />
            <label>{{ t("settings.rdnsUpstream") }} <code>spp_rdnsupstream</code></label>
            <input v-model="form.spp_rdnsupstream" />
            <label>{{ t("settings.rdnsPsk") }} <code>spp_rdnspsk</code></label>
            <input v-model="form.spp_rdnspsk" />
            <label>{{ t("settings.rdnsQueryType") }} <code>spp_rdnsquerytype</code></label>
            <select v-model="form.spp_rdnsquerytype">
              <option value="">{{ t("settings.queryDefault") }}</option>
              <option value="TXT">TXT</option>
              <option value="NULL">NULL</option>
            </select>
          </template>
        </div>
      </div>

      <div class="card section">
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

      <div class="card section">
        <h3>{{ t("settings.sectionDns") }}</h3>
        <div class="form-grid">
          <label>{{ t("settings.directDns") }} <code>direct_dns</code></label>
          <input v-model="form.direct_dns" placeholder="1.1.1.1:53" />
          <label>{{ t("settings.dohUrl") }} <code>doh_url</code></label>
          <input v-model="form.doh_url" placeholder="https://1.1.1.1/dns-query" />
        </div>
      </div>

      <div class="card section">
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

      <div class="card section">
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

      <div class="card section">
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
      <button class="btn primary" @click="save">{{ t("settings.save") }}</button>
      <button class="btn" @click="load">{{ t("settings.reload") }}</button>
      <span v-if="message" class="msg ok">{{ message }}</span>
      <span v-if="error" class="msg err">{{ error }}</span>
    </div>
  </section>
</template>

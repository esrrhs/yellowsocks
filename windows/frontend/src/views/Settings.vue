<script lang="ts" setup>
import { computed, onMounted, ref } from "vue";
import { load as yamlLoad, dump as yamlDump } from "js-yaml";
import { GetConfig, SaveConfig } from "../../wailsjs/go/guiapp/App";

// The form edits the same flat keys as the CLI config file (FileConfig in
// core/config). Every field not covered by a form section is still editable
// through the YAML source mode, and unknown keys survive form saves because
// saving merges onto the freshly parsed document.

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

async function load() {
  error.value = "";
  message.value = "";
  try {
    raw.value = await GetConfig();
    parseDoc();
  } catch (e: any) {
    error.value = String(e?.message ?? e);
  }
}

function parseDoc() {
  try {
    doc.value = (yamlLoad(raw.value) as Record<string, unknown>) ?? {};
  } catch (e: any) {
    error.value = `YAML 解析失败: ${String(e?.message ?? e)}`;
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
      error.value = `生成 YAML 失败: ${String(e?.message ?? e)}`;
      return;
    }
  }
  try {
    await SaveConfig(content);
    message.value = "已保存,启动或重启引擎后生效";
    raw.value = content;
    doc.value = (yamlLoad(content) as Record<string, unknown>) ?? {};
  } catch (e: any) {
    error.value = String(e?.message ?? e);
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
      error.value = `生成 YAML 失败: ${String(e?.message ?? e)}`;
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
      <h2>设置</h2>
      <div class="mode-switch">
        <button class="btn small" :class="{ active: mode === 'form' }" @click="switchMode('form')">表单</button>
        <button class="btn small" :class="{ active: mode === 'yaml' }" @click="switchMode('yaml')">YAML 源码</button>
      </div>
    </div>
    <p class="hint">
      配置与 CLI 的 config.yaml 完全同格式,可互相拷贝。表单模式保存时会合并回原文档,
      未在表单中出现的高级字段(nodes、api_origins 等)不会丢失;YAML 源码模式保留注释。
      全部字段说明见仓库根目录 config.example.yaml。
    </p>

    <template v-if="mode === 'form'">
      <div class="card section">
        <h3>上游服务器</h3>
        <div class="form-grid">
          <label>协议 spp_proto</label>
          <select v-model="form.spp_proto">
            <option v-for="p in sppProtos" :key="p" :value="p">{{ p }}</option>
          </select>

          <label>地址 spp_server</label>
          <input v-model="form.spp_server" :placeholder="isSocks5 ? '192.168.0.105:1080' : '1.2.3.4:8888'" />

          <template v-if="isSocks5">
            <label>用户名 spp_username</label>
            <input v-model="form.spp_username" placeholder="无认证则留空" />
            <label>密码 spp_password</label>
            <input v-model="form.spp_password" type="password" placeholder="无认证则留空" />
          </template>

          <template v-if="!isSocks5">
            <label>密钥 spp_key</label>
            <input v-model="form.spp_key" />
          </template>

          <template v-if="isSPP">
            <label>加密 spp_encrypt</label>
            <input v-model="form.spp_encrypt" placeholder="default" />
            <label>压缩 spp_compress</label>
            <input v-model="form.spp_compress" type="number" placeholder="128" />
          </template>

          <template v-if="isRdns">
            <label>隧道域名 spp_rdnsdomain</label>
            <input v-model="form.spp_rdnsdomain" placeholder="tunnel.example.com" />
            <label>上游 DNS spp_rdnsupstream</label>
            <input v-model="form.spp_rdnsupstream" />
            <label>PSK spp_rdnspsk</label>
            <input v-model="form.spp_rdnspsk" />
            <label>查询类型 spp_rdnsquerytype</label>
            <select v-model="form.spp_rdnsquerytype">
              <option value="">默认 (TXT)</option>
              <option value="TXT">TXT</option>
              <option value="NULL">NULL</option>
            </select>
          </template>
        </div>
      </div>

      <div class="card section">
        <h3>本地监听</h3>
        <div class="form-grid">
          <label>SOCKS5 socks5_listen</label>
          <input v-model="form.socks5_listen" placeholder="127.0.0.1:10808" />
          <label>HTTP http_listen</label>
          <input v-model="form.http_listen" placeholder="127.0.0.1:8080" />
          <label>DNS dns_listen</label>
          <input v-model="form.dns_listen" placeholder="127.0.0.1:1053" />
          <label>DoH doh_listen</label>
          <input v-model="form.doh_listen" placeholder="留空禁用" />
          <label>DoT dot_listen</label>
          <input v-model="form.dot_listen" placeholder="留空禁用,如 :853" />
          <label>IPv6 出站 ipv6</label>
          <input type="checkbox" v-model="form.ipv6" />
        </div>
      </div>

      <div class="card section">
        <h3>DNS 上游</h3>
        <div class="form-grid">
          <label>直连 DNS direct_dns</label>
          <input v-model="form.direct_dns" placeholder="1.1.1.1:53" />
          <label>远程 DoH doh_url</label>
          <input v-model="form.doh_url" placeholder="https://1.1.1.1/dns-query" />
        </div>
      </div>

      <div class="card section">
        <h3>路由分流</h3>
        <div class="form-grid">
          <label>直连域名 direct_domains</label>
          <textarea v-model="form.direct_domains" class="mini-editor" placeholder="每行一个域名,如 cn.example.com"></textarea>
          <label>直连网段 direct_cidrs</label>
          <textarea v-model="form.direct_cidrs" class="mini-editor" placeholder="每行一个 CIDR,如 192.168.0.0/16"></textarea>
          <label>跳过国家 skip_country</label>
          <input v-model="form.skip_country" placeholder="CN, US 或 none" />
          <label>GeoIP 数据库 geoip_file</label>
          <input v-model="form.geoip_file" placeholder="GeoLite2-Country.mmdb 路径" />
          <label>国内域名表 china_domains</label>
          <input v-model="form.china_domains" />
          <label>GFW 域名表 gfw_domains</label>
          <input v-model="form.gfw_domains" />
        </div>
      </div>

      <div class="card section">
        <h3>认证与 TLS</h3>
        <div class="form-grid">
          <label>代理用户名 proxy_username</label>
          <input v-model="form.proxy_username" placeholder="入站认证,留空禁用" />
          <label>代理密码 proxy_password</label>
          <input v-model="form.proxy_password" type="password" />
          <label>DoT 证书 tls_cert</label>
          <input v-model="form.tls_cert" />
          <label>DoT 私钥 tls_key</label>
          <input v-model="form.tls_key" />
        </div>
      </div>

      <div class="card section">
        <h3>API 与统计</h3>
        <div class="form-grid">
          <label>API 监听 api_listen</label>
          <input v-model="form.api_listen" placeholder="留空禁用,如 127.0.0.1:9090" />
          <label>API Token api_token</label>
          <input v-model="form.api_token" placeholder="none 表示信任网络下免认证" />
          <label>只读 Token api_readonly_token</label>
          <input v-model="form.api_readonly_token" />
          <label>内嵌面板 api_dashboard</label>
          <input type="checkbox" v-model="form.api_dashboard" />
          <label>pprof api_pprof</label>
          <input type="checkbox" v-model="form.api_pprof" />
          <label>统计文件 stats_file</label>
          <input v-model="form.stats_file" placeholder="stats.json" />
          <label>历史时长(时) stats_history_hours</label>
          <input v-model="form.stats_history_hours" type="number" />
          <label>日志级别 loglevel</label>
          <select v-model="form.loglevel">
            <option v-for="l in loglevels" :key="l" :value="l">{{ l }}</option>
          </select>
        </div>
      </div>
    </template>

    <template v-else>
      <textarea v-model="raw" class="editor" spellcheck="false" aria-label="配置内容"></textarea>
    </template>

    <div class="actions">
      <button class="btn primary" @click="save">保存</button>
      <button class="btn" @click="load">重新加载</button>
      <span v-if="message" class="msg ok">{{ message }}</span>
      <span v-if="error" class="msg err">{{ error }}</span>
    </div>
  </section>
</template>

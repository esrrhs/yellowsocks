import { ref } from "vue";

// Minimal i18n: a flat key table per language, no plural rules and no
// interpolation helpers. Everything the two languages need here is a label.
//
// Coverage is deliberate: the navigation, the dashboard, the connection view
// and the log view are translated; the settings form keeps Chinese labels
// because those labels are the config file's key names (socks5_listen,
// spp_server, ...) with a short Chinese gloss, and translating the gloss while
// the key stays English helps nobody.
export type Locale = "zh" | "en";

type Table = Record<string, string>;

const zh: Table = {
  "nav.home": "主页",
  "nav.connections": "连接",
  "nav.logs": "日志",
  "nav.settings": "设置",
  "nav.about": "关于",

  "engine.running": "代理运行中",
  "engine.stopped": "代理已停止",
  "engine.upstreamPending": "上游 {v},启动后生效",
  "engine.noUpstream": "在设置中配置上游服务器",
  "engine.start": "启动代理",
  "engine.stop": "停止代理",
  "engine.working": "处理中...",

  "chip.systemProxy": "系统透明代理",
  "chip.autoConnect": "自动连接",
  "chip.launchAtLogin": "开机启动",
  "chip.tun": "TUN 模式",
  "chip.guard": "系统代理守卫",
  "chip.on": "已开启",
  "chip.off": "已关闭",
  "chip.connectOnStart": "启动时连接",
  "chip.connectManual": "手动连接",
  "chip.loginAuto": "登录后自动运行",
  "chip.disabled": "未启用",
  "chip.tunActive": "全局接管中",
  "chip.tunRetry": "失败,点重试",
  "chip.tunPending": "授权后生效",
  "chip.tunIdle": "零配置全局代理",
  "chip.guardOn": "被改动后自动恢复",

  "mode.label": "分流模式",
  "mode.rule": "规则",
  "mode.global": "全局",
  "mode.direct": "直连",
  "mode.ruleHint": "国内直连、国外走代理",
  "mode.globalHint": "除局域网/本机外全部走代理",
  "mode.directHint": "全部直连,不经过代理",

  "stat.conns": "活动连接",
  "stat.upRate": "上传速率",
  "stat.downRate": "下载速率",
  "stat.upTotal": "上传总量",
  "stat.downTotal": "下载总量",
  "stat.uptime": "运行时长",

  "probe.title": "网络自检",
  "probe.run": "开始检测",
  "probe.running": "检测中...",
  "probe.fail": "失败",
  "probe.exitIP": "出口 IP",
  "probe.hint": "一次并行检查四条链路:国内直连、国外经代理、上游 SOCKS5、出口 IP",

  "lifetime.title": "累计流量(永久记录,重启与重置后仍保留)",
  "lifetime.reset": "清零",
  "lifetime.direct": "国内",
  "lifetime.proxy": "国外",
  "lifetime.total": "合计",

  "chart.title": "近 30 分钟流量",
  "chart.up": "上传",
  "chart.down": "下载",
  "chart.empty": "暂无数据",
  "chart.noEngine": "引擎未运行",

  "conn.tabConns": "连接",
  "conn.tabNodes": "节点",
  "conn.tabErrors": "错误",
  "conn.search": "搜索域名 / 客户端 / 节点",
  "conn.all": "全部",
  "conn.proxy": "代理",
  "conn.direct": "直连",
  "conn.count": "{n} / {m} 条",
  "conn.colHost": "域名",
  "conn.colClient": "客户端",
  "conn.colDecision": "决策",
  "conn.colNode": "节点",
  "conn.colRate": "速率",
  "conn.colAge": "时长",
  "conn.empty": "当前没有活动连接",
  "conn.noMatch": "没有匹配的连接",
  "conn.nodeTitle": "上游节点",
  "conn.colNodeConns": "连接数",
  "conn.colNodeUp": "上传",
  "conn.colNodeDown": "下载",
  "conn.colNodeSeen": "最后活动",
  "conn.sum": "合计",
  "conn.defaultNode": "默认",
  "conn.noNodes": "还没有节点流量记录",
  "conn.colTime": "时间",
  "conn.colScope": "来源",
  "conn.noErrors": "没有错误记录",

  "logs.search": "在日志里搜索",
  "logs.rows": "{n} 行",
  "logs.empty": "没有日志(引擎未运行、或所选级别今天还没写过)",
  "logs.dir": "日志目录:{path} · 每秒自动刷新 · 向上滚动即暂停跟随",

  "about.title": "关于",
  "about.version": "版本",
  "about.commit": "提交",
  "about.buildTime": "构建时间",
  "about.env": "运行环境",
  "about.github": "GitHub 项目主页",
};

const en: Table = {
  "nav.home": "Home",
  "nav.connections": "Connections",
  "nav.logs": "Logs",
  "nav.settings": "Settings",
  "nav.about": "About",

  "engine.running": "Proxy running",
  "engine.stopped": "Proxy stopped",
  "engine.upstreamPending": "Upstream {v}, applies on start",
  "engine.noUpstream": "Configure an upstream server in Settings",
  "engine.start": "Start proxy",
  "engine.stop": "Stop proxy",
  "engine.working": "Working...",

  "chip.systemProxy": "System proxy",
  "chip.autoConnect": "Auto connect",
  "chip.launchAtLogin": "Launch at login",
  "chip.tun": "TUN mode",
  "chip.guard": "Proxy guard",
  "chip.on": "On",
  "chip.off": "Off",
  "chip.connectOnStart": "Connect on start",
  "chip.connectManual": "Manual",
  "chip.loginAuto": "Runs after login",
  "chip.disabled": "Disabled",
  "chip.tunActive": "Capturing all traffic",
  "chip.tunRetry": "Failed, click to retry",
  "chip.tunPending": "Needs authorisation",
  "chip.tunIdle": "Zero-config global proxy",
  "chip.guardOn": "Restored if changed",

  "mode.label": "Routing",
  "mode.rule": "Rule",
  "mode.global": "Global",
  "mode.direct": "Direct",
  "mode.ruleHint": "Domestic direct, foreign proxied",
  "mode.globalHint": "Everything proxied except LAN and loopback",
  "mode.directHint": "Everything direct, proxy bypassed",

  "stat.conns": "Active connections",
  "stat.upRate": "Upload rate",
  "stat.downRate": "Download rate",
  "stat.upTotal": "Uploaded",
  "stat.downTotal": "Downloaded",
  "stat.uptime": "Uptime",

  "probe.title": "Connectivity check",
  "probe.run": "Run check",
  "probe.running": "Checking...",
  "probe.fail": "Failed",
  "probe.exitIP": "Exit IP",
  "probe.hint": "Four legs in parallel: domestic direct, foreign via proxy, upstream SOCKS5, exit IP",

  "lifetime.title": "Lifetime traffic (kept across restarts and resets)",
  "lifetime.reset": "Reset",
  "lifetime.direct": "Domestic",
  "lifetime.proxy": "Foreign",
  "lifetime.total": "Total",

  "chart.title": "Last 30 minutes",
  "chart.up": "Up",
  "chart.down": "Down",
  "chart.empty": "No data yet",
  "chart.noEngine": "Engine not running",

  "conn.tabConns": "Connections",
  "conn.tabNodes": "Nodes",
  "conn.tabErrors": "Errors",
  "conn.search": "Search host / client / node",
  "conn.all": "All",
  "conn.proxy": "Proxy",
  "conn.direct": "Direct",
  "conn.count": "{n} / {m}",
  "conn.colHost": "Host",
  "conn.colClient": "Client",
  "conn.colDecision": "Route",
  "conn.colNode": "Node",
  "conn.colRate": "Rate",
  "conn.colAge": "Age",
  "conn.empty": "No active connections",
  "conn.noMatch": "No matching connections",
  "conn.nodeTitle": "Upstream node",
  "conn.colNodeConns": "Conns",
  "conn.colNodeUp": "Up",
  "conn.colNodeDown": "Down",
  "conn.colNodeSeen": "Last seen",
  "conn.sum": "Total",
  "conn.defaultNode": "default",
  "conn.noNodes": "No node traffic recorded yet",
  "conn.colTime": "Time",
  "conn.colScope": "Scope",
  "conn.noErrors": "No errors recorded",

  "logs.search": "Search the log",
  "logs.rows": "{n} rows",
  "logs.empty": "No lines (engine stopped, or this level has not been written today)",
  "logs.dir": "Log directory: {path} · refreshes every second · scrolling up pauses follow",

  "about.title": "About",
  "about.version": "Version",
  "about.commit": "Commit",
  "about.buildTime": "Built",
  "about.env": "Environment",
  "about.github": "GitHub repository",
};

const tables: Record<Locale, Table> = { zh, en };

const stored = (localStorage.getItem("ys-lang") as Locale) || "zh";

// Reactive so every t() inside a template re-renders on switch.
export const locale = ref<Locale>(stored === "en" ? "en" : "zh");

export function setLocale(l: Locale) {
  locale.value = l;
  localStorage.setItem("ys-lang", l);
  document.documentElement.lang = l === "en" ? "en" : "zh-CN";
}

// t looks the key up in the current language, then Chinese, then the key
// itself. The fallback matters while a new label is added: a missing
// translation should show Chinese, never a raw identifier.
export function t(key: string, vars?: Record<string, string | number>): string {
  const table = tables[locale.value] ?? zh;
  let s = table[key] ?? zh[key] ?? key;
  if (vars) {
    for (const [k, v] of Object.entries(vars)) {
      s = s.replaceAll(`{${k}}`, String(v));
    }
  }
  return s;
}

setLocale(locale.value);

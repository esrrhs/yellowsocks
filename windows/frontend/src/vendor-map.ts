// Destination categorisation for the stats page: which company a domain
// belongs to. Pure display logic — the backend only reports per-domain
// traffic with its domestic/foreign byte split, and the page groups those
// rows by vendor here.
//
// Matching is by domain suffix, longest first: "mail.google.com" matches the
// google.com entry, and a suffix must own the whole rightmost labels, so
// "notgoogle.com" never matches. The lists are deliberately short — the big
// names a person would look for — and anything unrecognised falls into the
// page's 其他 bucket rather than being guessed at.

export interface Vendor {
  id: string;
  zh: string;
  en: string;
  suffixes: string[];
}

// Entries are listed foreign-first for reading, but lookup is side-agnostic:
// a vendor reachable both ways (Microsoft is the common case) shows on both
// sides of the split with its own numbers.
export const vendors: Vendor[] = [
  {
    id: "google",
    zh: "谷歌",
    en: "Google",
    suffixes: [
      "google.com", "googleapis.com", "gstatic.com", "googlevideo.com",
      "youtube.com", "ytimg.com", "googleusercontent.com", "goog",
      "android.com", "withgoogle.com", "googleadservices.com",
      "doubleclick.net", "ggpht.com", "google-analytics.com", "recaptcha.net",
      "firebaseio.com",
    ],
  },
  {
    id: "apple",
    zh: "苹果",
    en: "Apple",
    suffixes: [
      "apple.com", "icloud.com", "mzstatic.com", "me.com",
      "apple-cloudkit.com", "apple-dns.net",
    ],
  },
  {
    id: "microsoft",
    zh: "微软",
    en: "Microsoft",
    suffixes: [
      "microsoft.com", "windows.com", "office.com", "live.com", "msedge.net",
      "bing.com", "microsoftonline.com", "windowsupdate.com", "azure.com",
      "azureedge.net", "visualstudio.com", "xbox.com", "skype.com", "msn.com",
      "msftconnecttest.com", "outlook.com", "onedrive.com",
    ],
  },
  {
    id: "cloudflare",
    zh: "Cloudflare",
    en: "Cloudflare",
    suffixes: ["cloudflare.com", "cloudflarestorage.com", "pages.dev", "workers.dev", "cloudflareinsights.com"],
  },
  {
    id: "github",
    zh: "GitHub",
    en: "GitHub",
    suffixes: ["github.com", "githubusercontent.com", "githubassets.com", "github.io", "ghcr.io"],
  },
  {
    id: "telegram",
    zh: "Telegram",
    en: "Telegram",
    suffixes: ["telegram.org", "t.me", "telesco.pe"],
  },
  {
    id: "openai",
    zh: "OpenAI",
    en: "OpenAI",
    suffixes: ["openai.com", "oaistatic.com", "oaiusercontent.com", "chatgpt.com"],
  },
  {
    id: "anthropic",
    zh: "Anthropic",
    en: "Anthropic",
    suffixes: ["anthropic.com", "claude.ai"],
  },
  {
    id: "amazon",
    zh: "亚马逊",
    en: "Amazon",
    suffixes: ["amazon.com", "amazonaws.com", "cloudfront.net", "awsstatic.com", "primevideo.com"],
  },
  {
    id: "meta",
    zh: "Meta",
    en: "Meta",
    suffixes: ["facebook.com", "fbcdn.net", "instagram.com", "whatsapp.com", "whatsapp.net", "meta.com", "threads.net"],
  },
  {
    id: "x",
    zh: "X (推特)",
    en: "X (Twitter)",
    suffixes: ["twitter.com", "x.com", "twimg.com", "t.co"],
  },
  {
    id: "netflix",
    zh: "网飞",
    en: "Netflix",
    suffixes: ["netflix.com", "nflxvideo.net", "nflximg.net", "nflxso.net"],
  },
  {
    id: "steam",
    zh: "Steam",
    en: "Steam",
    suffixes: ["steampowered.com", "steamcommunity.com", "steamstatic.com", "steamcontent.com", "steamserver.net"],
  },
  {
    id: "reddit",
    zh: "Reddit",
    en: "Reddit",
    suffixes: ["reddit.com", "redditstatic.com", "redd.it", "redditmedia.com"],
  },
  {
    id: "wikipedia",
    zh: "维基百科",
    en: "Wikipedia",
    suffixes: ["wikipedia.org", "wikimedia.org"],
  },
  {
    id: "tencent",
    zh: "腾讯",
    en: "Tencent",
    suffixes: ["qq.com", "tencent.com", "gtimg.com", "qpic.cn", "qcloud.com", "myqcloud.com", "wechat.com"],
  },
  {
    id: "alibaba",
    zh: "阿里巴巴",
    en: "Alibaba",
    suffixes: [
      "taobao.com", "tmall.com", "alibaba.com", "aliyun.com", "aliyuncs.com",
      "alipay.com", "alicdn.com", "tbcdn.cn", "dingtalk.com", "amap.com",
      "1688.com", "antfin.com",
    ],
  },
  {
    id: "netease",
    zh: "网易",
    en: "NetEase",
    suffixes: ["163.com", "126.com", "126.net", "netease.com", "netease.im", "neteasegames.com", "youdao.com"],
  },
  {
    id: "baidu",
    zh: "百度",
    en: "Baidu",
    suffixes: ["baidu.com", "bdstatic.com", "bdimg.com", "bcebos.com", "baidubce.com"],
  },
  {
    id: "bytedance",
    zh: "字节跳动",
    en: "ByteDance",
    suffixes: [
      "bytedance.com", "douyin.com", "douyinstatic.com", "douyinpic.com",
      "toutiao.com", "toutiaoimg.com", "pstatp.com", "snssdk.com", "zjcdn.com",
      "ixigua.com",
    ],
  },
  {
    id: "xiaomi",
    zh: "小米",
    en: "Xiaomi",
    suffixes: ["mi.com", "xiaomi.com", "miui.com", "xiaomi.net"],
  },
  {
    id: "huawei",
    zh: "华为",
    en: "Huawei",
    suffixes: ["huawei.com", "huaweicloud.com", "hicloud.com", "vmall.com"],
  },
  {
    id: "jd",
    zh: "京东",
    en: "JD",
    suffixes: ["jd.com", "360buyimg.com", "jdcloud.com", "jcloud.com"],
  },
  {
    id: "bilibili",
    zh: "哔哩哔哩",
    en: "Bilibili",
    suffixes: ["bilibili.com", "bilibili.tv", "biliapi.com", "biliapi.net", "hdslb.com", "bilivideo.com", "acg.tv"],
  },
  {
    id: "meituan",
    zh: "美团",
    en: "Meituan",
    suffixes: ["meituan.com", "meituan.net", "dianping.com"],
  },
  {
    id: "kuaishou",
    zh: "快手",
    en: "Kuaishou",
    suffixes: ["kuaishou.com", "gifshow.com", "yximgs.com"],
  },
  {
    id: "sina",
    zh: "新浪微博",
    en: "Sina Weibo",
    suffixes: ["sina.com.cn", "sina.cn", "weibo.com", "weibo.cn", "sinaimg.cn", "sinajs.cn"],
  },
  {
    id: "qihoo",
    zh: "360",
    en: "Qihoo 360",
    suffixes: ["360.cn", "360.com", "so.com", "qihoo.com", "qhimg.com", "qhres.com", "qhres2.com"],
  },
  {
    id: "zhihu",
    zh: "知乎",
    en: "Zhihu",
    suffixes: ["zhihu.com", "zhimg.com"],
  },
];

// Longest-suffix table built once: each (suffix, vendor) pair, sorted so the
// most specific suffix wins a lookup.
const bySuffix: [string, Vendor][] = vendors
  .flatMap((v) => v.suffixes.map((s) => [s, v] as [string, Vendor]))
  .sort((a, b) => b[0].length - a[0].length);

// matchVendor resolves a domain (already lowercased by the collector) to its
// vendor, or undefined when the table does not know it.
export function matchVendor(domain: string): Vendor | undefined {
  const d = domain.toLowerCase();
  for (const [suffix, vendor] of bySuffix) {
    if (d === suffix || d.endsWith("." + suffix)) return vendor;
  }
  return undefined;
}

// The collector keys UDP datagrams and not-yet-named connections by their
// literal address, so a domain entry whose key parses as an IP carries no
// vendor; the page groups those under 未识别地址.
export function isAddrLiteral(key: string): boolean {
  return /^\d{1,3}(\.\d{1,3}){3}$/.test(key) || key.includes(":");
}

// ---- base-domain grouping for the "other" bucket ----

// Multi-label public suffixes the base-domain split must keep together.
// A plain last-two-labels cut would turn www.baidu.com.cn into com.cn;
// these are the second-level suffixes that actually show up in traffic.
// Anything not listed splits at the last dot pair.
const multiSuffixes = new Set([
  // China
  "com.cn", "net.cn", "org.cn", "gov.cn", "edu.cn", "ac.cn", "mil.cn",
  "com.hk", "org.hk", "net.hk", "edu.hk", "gov.hk",
  "com.tw", "org.tw", "net.tw", "edu.tw", "gov.tw", "idv.tw",
  // Asia
  "co.jp", "or.jp", "ne.jp", "ac.jp", "ad.jp", "ed.jp", "go.jp", "gr.jp", "lg.jp",
  "co.kr", "or.kr", "ne.kr", "re.kr", "go.kr", "pe.kr",
  "com.sg", "net.sg", "org.sg", "edu.sg", "gov.sg", "per.sg",
  "com.my", "net.my", "org.my", "edu.my", "gov.my",
  "co.th", "or.th", "in.th", "ac.th", "go.th", "net.th",
  "com.vn", "net.vn", "org.vn", "edu.vn", "gov.vn",
  "co.id", "web.id", "or.id", "net.id", "go.id", "ac.id", "sch.id",
  "com.ph", "net.ph", "org.ph", "edu.ph", "gov.ph",
  "co.in", "net.in", "org.in", "firm.in", "gen.in", "ind.in", "ac.in", "gov.in",
  "com.hk", "edu.hk", "gov.hk",
  // Western
  "co.uk", "org.uk", "me.uk", "net.uk", "ltd.uk", "plc.uk", "gov.uk", "ac.uk",
  "com.au", "net.au", "org.au", "edu.au", "gov.au", "id.au", "asn.au",
  "co.nz", "net.nz", "org.nz", "govt.nz", "ac.nz", "school.nz",
  "com.br", "net.br", "org.br", "gov.br", "edu.br",
  "com.ar", "com.mx", "com.tr", "com.sa", "com.ua",
  "co.il", "org.il", "net.il", "ac.il", "gov.il",
  "com.ru", "net.ru", "org.ru",
]);

// baseDomain cuts a host down to its registrable domain: the last two
// labels, or the last three when the tail is a multi-label suffix —
// "mail.google.com" → "google.com", "www.baidu.com.cn" → "baidu.com.cn".
// This is what the stats page groups unlisted domains under.
export function baseDomain(domain: string): string {
  const d = domain.toLowerCase().replace(/\.$/, "");
  const parts = d.split(".").filter(Boolean);
  if (parts.length <= 2) return d;
  const last2 = parts.slice(-2).join(".");
  if (multiSuffixes.has(last2)) {
    return parts.slice(-3).join(".");
  }
  return last2;
}

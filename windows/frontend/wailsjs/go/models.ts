export namespace guiapp {
	
	export class EngineStatus {
	    running: boolean;
	    system_proxy: boolean;
	    tun: boolean;
	    tun_error: string;
	    socks5: string;
	    http: string;
	    dns: string;
	    spp_server: string;
	    spp_proto: string;
	    proxy_mode: string;
	    error: string;
	
	    static createFrom(source: any = {}) {
	        return new EngineStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.system_proxy = source["system_proxy"];
	        this.tun = source["tun"];
	        this.tun_error = source["tun_error"];
	        this.socks5 = source["socks5"];
	        this.http = source["http"];
	        this.dns = source["dns"];
	        this.spp_server = source["spp_server"];
	        this.spp_proto = source["spp_proto"];
	        this.proxy_mode = source["proxy_mode"];
	        this.error = source["error"];
	    }
	}
	export class ProbeItem {
	    key: string;
	    name: string;
	    target: string;
	    ok: boolean;
	    ms: number;
	    status: number;
	    detail: string;
	
	    static createFrom(source: any = {}) {
	        return new ProbeItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.name = source["name"];
	        this.target = source["target"];
	        this.ok = source["ok"];
	        this.ms = source["ms"];
	        this.status = source["status"];
	        this.detail = source["detail"];
	    }
	}
	export class ProbeReport {
	    at: number;
	    exit_ip: string;
	    items: ProbeItem[];
	
	    static createFrom(source: any = {}) {
	        return new ProbeReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.at = source["at"];
	        this.exit_ip = source["exit_ip"];
	        this.items = this.convertValues(source["items"], ProbeItem);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace stats {
	
	export class Counters {
	    upload_bytes: number;
	    download_bytes: number;
	    conns: number;
	    errors: number;
	
	    static createFrom(source: any = {}) {
	        return new Counters(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.upload_bytes = source["upload_bytes"];
	        this.download_bytes = source["download_bytes"];
	        this.conns = source["conns"];
	        this.errors = source["errors"];
	    }
	}
	export class DomainStat {
	    domain: string;
	    upload_bytes: number;
	    download_bytes: number;
	    conns: number;
	    direct_conns: number;
	    proxy_conns: number;
	    errors: number;
	    last_seen: number;
	
	    static createFrom(source: any = {}) {
	        return new DomainStat(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.domain = source["domain"];
	        this.upload_bytes = source["upload_bytes"];
	        this.download_bytes = source["download_bytes"];
	        this.conns = source["conns"];
	        this.direct_conns = source["direct_conns"];
	        this.proxy_conns = source["proxy_conns"];
	        this.errors = source["errors"];
	        this.last_seen = source["last_seen"];
	    }
	}
	export class DNSStat {
	    queries: number;
	    cache_hits: number;
	    failures: number;
	    direct: number;
	    doh: number;
	    by_type: Record<string, number>;
	    by_upstream: Record<string, number>;
	    top_domains: DomainStat[];
	
	    static createFrom(source: any = {}) {
	        return new DNSStat(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.queries = source["queries"];
	        this.cache_hits = source["cache_hits"];
	        this.failures = source["failures"];
	        this.direct = source["direct"];
	        this.doh = source["doh"];
	        this.by_type = source["by_type"];
	        this.by_upstream = source["by_upstream"];
	        this.top_domains = this.convertValues(source["top_domains"], DomainStat);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class ErrorEntry {
	    t: number;
	    time: string;
	    scope: string;
	    host: string;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new ErrorEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.t = source["t"];
	        this.time = source["time"];
	        this.scope = source["scope"];
	        this.host = source["host"];
	        this.message = source["message"];
	    }
	}
	export class LifetimeStats {
	    totals: Counters;
	    direct: Counters;
	    proxy: Counters;
	
	    static createFrom(source: any = {}) {
	        return new LifetimeStats(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.totals = this.convertValues(source["totals"], Counters);
	        this.direct = this.convertValues(source["direct"], Counters);
	        this.proxy = this.convertValues(source["proxy"], Counters);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class NodeStat {
	    name: string;
	    upload_bytes: number;
	    download_bytes: number;
	    conns: number;
	    last_seen: number;
	
	    static createFrom(source: any = {}) {
	        return new NodeStat(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.upload_bytes = source["upload_bytes"];
	        this.download_bytes = source["download_bytes"];
	        this.conns = source["conns"];
	        this.last_seen = source["last_seen"];
	    }
	}
	export class Rate {
	    upload_bps: number;
	    download_bps: number;
	
	    static createFrom(source: any = {}) {
	        return new Rate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.upload_bps = source["upload_bps"];
	        this.download_bps = source["download_bps"];
	    }
	}
	export class Overview {
	    now: number;
	    start_time: number;
	    uptime_seconds: number;
	    active_conns: number;
	    totals: Counters;
	    direct: Counters;
	    proxy: Counters;
	    lifetime: Counters;
	    lifetime_direct: Counters;
	    lifetime_proxy: Counters;
	    rate: Rate;
	    rate_1m: Rate;
	    peak: Rate;
	    dns: DNSStat;
	    top_domains: DomainStat[];
	
	    static createFrom(source: any = {}) {
	        return new Overview(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.now = source["now"];
	        this.start_time = source["start_time"];
	        this.uptime_seconds = source["uptime_seconds"];
	        this.active_conns = source["active_conns"];
	        this.totals = this.convertValues(source["totals"], Counters);
	        this.direct = this.convertValues(source["direct"], Counters);
	        this.proxy = this.convertValues(source["proxy"], Counters);
	        this.lifetime = this.convertValues(source["lifetime"], Counters);
	        this.lifetime_direct = this.convertValues(source["lifetime_direct"], Counters);
	        this.lifetime_proxy = this.convertValues(source["lifetime_proxy"], Counters);
	        this.rate = this.convertValues(source["rate"], Rate);
	        this.rate_1m = this.convertValues(source["rate_1m"], Rate);
	        this.peak = this.convertValues(source["peak"], Rate);
	        this.dns = this.convertValues(source["dns"], DNSStat);
	        this.top_domains = this.convertValues(source["top_domains"], DomainStat);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class SeriesPoint {
	    t: number;
	    time: string;
	    upload_bytes: number;
	    download_bytes: number;
	    conns: number;
	    errors: number;
	    direct_upload_bytes: number;
	    direct_download_bytes: number;
	    proxy_upload_bytes: number;
	    proxy_download_bytes: number;
	    dns_queries: number;
	
	    static createFrom(source: any = {}) {
	        return new SeriesPoint(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.t = source["t"];
	        this.time = source["time"];
	        this.upload_bytes = source["upload_bytes"];
	        this.download_bytes = source["download_bytes"];
	        this.conns = source["conns"];
	        this.errors = source["errors"];
	        this.direct_upload_bytes = source["direct_upload_bytes"];
	        this.direct_download_bytes = source["direct_download_bytes"];
	        this.proxy_upload_bytes = source["proxy_upload_bytes"];
	        this.proxy_download_bytes = source["proxy_download_bytes"];
	        this.dns_queries = source["dns_queries"];
	    }
	}
	export class SessionView {
	    id: number;
	    proto: string;
	    client: string;
	    host: string;
	    port: number;
	    decision: string;
	    node: string;
	    start: number;
	    age_ms: number;
	    upload_bytes: number;
	    download_bytes: number;
	    upload_bps: number;
	    download_bps: number;
	
	    static createFrom(source: any = {}) {
	        return new SessionView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.proto = source["proto"];
	        this.client = source["client"];
	        this.host = source["host"];
	        this.port = source["port"];
	        this.decision = source["decision"];
	        this.node = source["node"];
	        this.start = source["start"];
	        this.age_ms = source["age_ms"];
	        this.upload_bytes = source["upload_bytes"];
	        this.download_bytes = source["download_bytes"];
	        this.upload_bps = source["upload_bps"];
	        this.download_bps = source["download_bps"];
	    }
	}

}

export namespace version {
	
	export class Info {
	    version: string;
	    git_commit: string;
	    build_time: string;
	    go_version: string;
	    os: string;
	    arch: string;
	
	    static createFrom(source: any = {}) {
	        return new Info(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.git_commit = source["git_commit"];
	        this.build_time = source["build_time"];
	        this.go_version = source["go_version"];
	        this.os = source["os"];
	        this.arch = source["arch"];
	    }
	}

}


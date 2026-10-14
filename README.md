# yellowsocks

[<img src="https://img.shields.io/github/license/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/languages/top/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/v/release/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks/releases)
[<img src="https://img.shields.io/github/downloads/esrrhs/yellowsocks/total">](https://github.com/esrrhs/yellowsocks/releases)

Linux proxy, version **2.0.0**. It serves DNS, DoT, DoH, SOCKS5, and HTTP on the local machine, and forwards proxied traffic to a remote [SPP](https://github.com/esrrhs/spp) server.

## Features

- **DNS**: standard UDP queries
- **DoH**: TCP `/dns-query` (RFC 8484, GET and POST)
- **DoT**: optional DNS-over-TLS (RFC 7858)
- **SOCKS5**: TCP CONNECT and UDP ASSOCIATE
- **HTTP proxy**: plain HTTP requests and HTTPS CONNECT
- **SPP upstream**: `tcp`, `udp`, `kcp`, or `quic`, with multiple nodes and automatic failover
- **Routing**: domain, CIDR, and GeoIP rules choose a direct connection or SPP
- **Statistics API**: live and historical traffic, connections, routing, DNS and errors over HTTP

Domestic domains and private addresses go direct. Everything else goes through SPP.

## Run

Copy `config.example.yaml` to `config.yaml`, set the SPP server, then:

```bash
sudo ./yellowsocks-cli -config config.yaml
```

Or pass the server on the command line:

```bash
sudo ./yellowsocks-cli \
  -spp-server "your_spp_server:8888" \
  -spp-proto "tcp" \
  -spp-key "123456"
```

Listening on port `53` requires root. Press `Ctrl+C` to stop.

### Flags

| Flag | Description | Default |
| :--- | :--- | :--- |
| `-config` | YAML or JSON config file | none |
| `-spp-server` | Remote SPP address | none |
| `-spp-proto` | `tcp`, `udp`, `kcp`, or `quic` | `tcp` |
| `-spp-key` | SPP key | `123456` |
| `-socks5` | Inbound SOCKS5 | `127.0.0.1:1080` |
| `-http` | Inbound HTTP proxy | `127.0.0.1:8080` |
| `-dns` | DNS UDP | `127.0.0.1:53` |
| `-doh` | DoH TCP | `127.0.0.1:8053` |
| `-dot` | DoT listen address | empty, disabled |
| `-tls-cert` | DoT certificate PEM | empty |
| `-tls-key` | DoT private key PEM | empty |
| `-china-domains` | Domestic domain list | none |
| `-gfw-domains` | Proxied domain list | none |
| `-geoip` | `GeoLite2-Country.mmdb` | none |
| `-loglevel` | `debug`, `info`, `warn`, `error` | `info` |
| `-api` | Statistics HTTP API listen address | empty, disabled |
| `-api-token` | Statistics API token | random, printed at startup |
| `-stats-file` | Path persisting statistics history | `stats.json` when the API is on |
| `-v`, `-version` | Print the version and exit | |

Values in the config file override the matching flags. Multiple nodes, inbound authentication, encryption, and compression are documented in `config.example.yaml`.

### Example

```yaml
spp_server: "1.2.3.4:8888"
spp_proto: "tcp"
spp_key: "123456"

socks5_listen: "127.0.0.1:1080"
http_listen: "127.0.0.1:8080"

dns_listen: "127.0.0.1:53"
doh_listen: "127.0.0.1:8053"
direct_dns: "114.114.114.114:53"
doh_url: "https://1.1.1.1/dns-query"
```

To enable DoT, set `dot_listen`, `tls_cert`, and `tls_key` together.

## Statistics API

Turn the API on with `api_listen` (or `-api`):

```yaml
api_listen: "127.0.0.1:9090"
api_token: "change-me"
api_dashboard: true      # serves a built-in page at http://127.0.0.1:9090/
stats_file: "stats.json" # counters and history survive a restart
stats_history_hours: 24  # minute-resolution history kept (max 720)
```

Every request except `/` and `/api/v1/health` needs the token, sent as
`Authorization: Bearer <token>`, as `X-API-Token: <token>`, or as a `token`
query parameter. When `api_token` is empty a random token is generated and
printed in the log at startup.

```bash
curl -H "Authorization: Bearer change-me" http://127.0.0.1:9090/api/v1/overview
```

| Endpoint | What it returns |
| :--- | :--- |
| `GET /api/v1/health` | Liveness and version (no auth) |
| `GET /api/v1/status` | Version, uptime, listeners, routing rules, SPP nodes, history window |
| `GET /api/v1/overview` | Active connections, cumulative and per-path counters, current and peak speed |
| `GET /api/v1/connections` | Live sessions: client, target, decision, bytes, age |
| `GET /api/v1/traffic` | History series, `range=30m\|1h\|6h\|24h\|7d`, `step=1m\|5m\|15m\|1h` |
| `GET /api/v1/domains` | Domain distribution, `limit`, `order=bytes\|conns` |
| `GET /api/v1/routes` | Direct vs SPP split with the busiest domains on each side |
| `GET /api/v1/dns` | Query counts, cache hits, upstream usage, failures, top queried domains |
| `GET /api/v1/errors` | Rolling error log, `limit`, `scope=dns\|socks5\|http\|spp` |
| `GET /api/v1/nodes` | Per-SPP-node liveness and traffic |
| `GET /api/v1/snapshot` | Everything at once, for export |
| `POST /api/v1/reset` | Clears counters and history |

Byte direction is named from the relay's point of view:

- `upload_bytes` — client to target (what the client sends out)
- `download_bytes` — target to client (what the client receives)

`connections` accepts `proto`, `decision` and `host` filters plus `limit`
(<=1000). CORS is enabled for browser callers; restrict it with
`api_origins`.

### Dashboard page

The proxy ships a single-page dashboard (`core/api/dashboard.html`) and
serves it at `http://<api_listen>/` when `api_dashboard` is true.

The page does not have to be served by the proxy. Copy the HTML anywhere
(a static host, another machine, even `file://`) and fill in the two boxes
at the top:

- **服务器** — the API address of the relay, e.g. `http://1.2.3.4:9090`.
  Leave it empty to talk to whoever serves the page (same origin).
- **API token** — the token configured on that relay.

Both are kept in `localStorage` and can be pre-filled from the URL, which
is handy for a bookmark or a shared link:

```
http://dashboard-host/index.html?server=http://1.2.3.4:9090&token=change-me
```

A page served from another origin needs CORS on the relay side: keep
`api_origins: ["*"]`, or list the page's origin explicitly.

## Build

```bash
./pack.sh
```

Archives are written to `pack/`:

- `yellowsocks_linux_amd64.zip`
- `yellowsocks_linux_arm64.zip`
- `yellowsocks_linux_arm.zip`

Each archive contains `yellowsocks-cli` and `config.example.yaml`.

## License

MIT

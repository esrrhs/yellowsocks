package api

import (
	"time"

	"github.com/esrrhs/yellowsocks/core/stats"
)

// Listeners are the addresses each inbound service is bound to. Empty means
// the service is not running.
type Listeners struct {
	DNS    string `json:"dns"`
	DoH    string `json:"doh"`
	DoT    string `json:"dot"`
	Socks5 string `json:"socks5"`
	HTTP   string `json:"http"`
	API    string `json:"api"`
}

// RouterInfo summarizes the loaded routing rules.
type RouterInfo struct {
	DirectDomains int    `json:"direct_domains"`
	ProxyDomains  int    `json:"proxy_domains"`
	DirectCIDRs   int    `json:"direct_cidrs"`
	GeoIP         bool   `json:"geoip"`
	SkipCountry   string `json:"skip_country"`
}

// NodeInfo is one configured SPP node: identity, liveness and the traffic it
// has carried (zero when the node has not been used yet).
type NodeInfo struct {
	Name     string `json:"name"`
	Server   string `json:"server"`
	Proto    string `json:"proto"`
	Alive    bool   `json:"alive"`
	Active   bool   `json:"active"`
	Conns    int64  `json:"conns"`
	Upload   int64  `json:"upload_bytes"`
	Download int64  `json:"download_bytes"`
}

// HistoryInfo describes what history the collector keeps.
type HistoryInfo struct {
	BucketSeconds   int64  `json:"bucket_seconds"`
	RetentionBucket int    `json:"retention_buckets"`
	Oldest          int64  `json:"oldest"` // unix seconds, 0 when empty
	Newest          int64  `json:"newest"`
	PersistFile     string `json:"persist_file"`
}

// SystemInfo is proxy state the statistics collector cannot know about:
// version, listeners, routing rules and SPP nodes.
type SystemInfo struct {
	Version   string     `json:"version"`
	Running   bool       `json:"running"`
	IPv6      bool       `json:"ipv6"`
	Listeners Listeners  `json:"listeners"`
	Router    RouterInfo `json:"router"`
	Nodes     []NodeInfo `json:"nodes"`
}

// InfoProvider supplies SystemInfo. Implemented by the engine.
type InfoProvider interface {
	APIInfo() SystemInfo
}

// StatusResponse is the /status payload.
type StatusResponse struct {
	SystemInfo
	Now     int64       `json:"now"`
	Uptime  int64       `json:"uptime_seconds"`
	Started int64       `json:"start_time"`
	History HistoryInfo `json:"history"`
}

// ConnectionsResponse is the /connections payload.
type ConnectionsResponse struct {
	Now         int64               `json:"now"`
	Count       int                 `json:"count"`
	Connections []stats.SessionView `json:"connections"`
}

// TrafficResponse is the /traffic payload.
type TrafficResponse struct {
	Range      string              `json:"range"`
	StepSecond int64               `json:"step_seconds"`
	From       int64               `json:"from"`
	To         int64               `json:"to"`
	Points     []stats.SeriesPoint `json:"points"`
}

// DomainsResponse is the /domains payload.
type DomainsResponse struct {
	Now     int64              `json:"now"`
	Count   int                `json:"count"`
	Domains []stats.DomainStat `json:"domains"`
}

// RoutesResponse is the /routes payload: how traffic splits between the
// direct path and SPP.
type RoutesResponse struct {
	Now              int64              `json:"now"`
	Totals           stats.Counters     `json:"totals"`
	Direct           stats.Counters     `json:"direct"`
	Proxy            stats.Counters     `json:"proxy"`
	DirectConnRatio  float64            `json:"direct_conn_ratio"`
	ProxyConnRatio   float64            `json:"proxy_conn_ratio"`
	DirectBytesRatio float64            `json:"direct_bytes_ratio"`
	ProxyBytesRatio  float64            `json:"proxy_bytes_ratio"`
	TopDirectDomains []stats.DomainStat `json:"top_direct_domains"`
	TopProxyDomains  []stats.DomainStat `json:"top_proxy_domains"`
}

// NodesResponse is the /nodes payload.
type NodesResponse struct {
	Now   int64      `json:"now"`
	Count int        `json:"count"`
	Nodes []NodeInfo `json:"nodes"`
}

// ErrorsResponse is the /errors payload.
type ErrorsResponse struct {
	Now    int64              `json:"now"`
	Count  int                `json:"count"`
	Errors []stats.ErrorEntry `json:"errors"`
}

// SnapshotResponse is the /snapshot payload: everything at once, for export
// or for a page that prefers one request.
type SnapshotResponse struct {
	Status      StatusResponse      `json:"status"`
	Overview    stats.Overview      `json:"overview"`
	Traffic     TrafficResponse     `json:"traffic"`
	Domains     []stats.DomainStat  `json:"domains"`
	Routes      RoutesResponse      `json:"routes"`
	Nodes       []NodeInfo          `json:"nodes"`
	Errors      []stats.ErrorEntry  `json:"errors"`
	Connections []stats.SessionView `json:"connections"`
	DNS         stats.DNSStat       `json:"dns"`
	GeneratedAt time.Time           `json:"generated_at"`
}

// OverviewResponse is the /overview payload.
type OverviewResponse struct {
	stats.Overview
	Version string `json:"version"`
}

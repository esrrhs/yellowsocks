// Package api exposes the proxy's live and historical statistics over HTTP,
// so an external page can watch traffic, connections, routing, DNS and
// errors without shell access to the host.
//
// Every endpoint except / and /api/v1/health requires the configured token,
// sent as "Authorization: Bearer <token>", as "X-API-Token: <token>" or as a
// "token" query parameter.
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core/stats"
)

// Defaults for Config.
const (
	DefaultListenAddr   = "127.0.0.1:9090"
	DefaultReadTimeout  = 15 * time.Second
	DefaultWriteTimeout = 30 * time.Second
	DefaultIdleTimeout  = 60 * time.Second

	// Caps keep a hostile or buggy query from making the server build a huge
	// response.
	MaxLimit       = 1000
	MaxDomainLimit = 500
	MaxErrorLimit  = 500
	MaxPoints      = 720
)

// Config configures the API server.
type Config struct {
	// ListenAddr is the HTTP listen address (default 127.0.0.1:9090).
	ListenAddr string
	// Token is the shared secret every request must present. When empty a
	// random token is generated and logged at startup.
	Token string
	// AllowedOrigins lists CORS origins allowed to call the API from a
	// browser. Empty or ["*"] allows any origin.
	AllowedOrigins []string
	// Dashboard serves the built-in single page dashboard at "/".
	Dashboard    bool
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
}

// Deps are the runtime dependencies of the API server.
type Deps struct {
	Stats   *stats.Collector
	Info    InfoProvider
	Version string
}

// Server serves the statistics API.
type Server struct {
	cfg    Config
	deps   Deps
	start  time.Time
	srv    *http.Server
	ln     net.Listener
	mu     sync.Mutex
	closed bool
}

// New builds an API server. It binds nothing until Start.
func New(cfg Config, deps Deps) (*Server, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListenAddr
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = DefaultReadTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if len(cfg.AllowedOrigins) == 0 {
		cfg.AllowedOrigins = []string{"*"}
	}
	generated := false
	if cfg.Token == "" {
		cfg.Token = randomToken()
		generated = true
	}
	if deps.Stats == nil {
		deps.Stats = stats.New(stats.Config{})
	}

	s := &Server{cfg: cfg, deps: deps, start: time.Now()}
	s.srv = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: cfg.ReadTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
	if generated {
		loggo.Warn("[API] No api_token configured; generated one: %s", cfg.Token)
	}
	return s, nil
}

// Token returns the token clients must present.
func (s *Server) Token() string { return s.cfg.Token }

// Addr returns the bound address, or nil before Start.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Start binds the listener and begins serving.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("api listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.ln = ln
	s.closed = false
	s.start = time.Now()
	loggo.Info("[API] Statistics API listening on %s (token auth required)", ln.Addr())
	go func() {
		if err := s.srv.Serve(ln); err != nil && !strings.Contains(err.Error(), "use of closed") {
			loggo.Info("[API] Server stopped: %v", err)
		}
	}()
	return nil
}

// Stop shuts the server down.
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.ln
	s.ln = nil
	s.mu.Unlock()

	if ln == nil {
		return nil
	}
	loggo.Info("[API] Stopping statistics API...")
	return ln.Close()
}

func randomToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read never fails on the platforms we support; fall back to a
		// time-derived value rather than serving with an empty token.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// handler builds the route table.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	// Patterns carry no method: OPTIONS preflight and method rejection are
	// handled inside route. A "GET /" pattern would conflict with these, so
	// the dashboard is the method-less root that catches everything else.
	if s.cfg.Dashboard {
		mux.HandleFunc("/", s.handleDashboard)
		mux.HandleFunc("/dashboard", s.handleDashboard)
	} else {
		mux.HandleFunc("/", s.handleNotFound)
	}

	// Unauthenticated liveness probe: it exposes nothing but the version.
	mux.HandleFunc("/api/v1/health", s.route(false, s.handleHealth, http.MethodGet, http.MethodHead))

	mux.HandleFunc("/api/v1/status", s.route(true, s.handleStatus, http.MethodGet))
	mux.HandleFunc("/api/v1/overview", s.route(true, s.handleOverview, http.MethodGet))
	mux.HandleFunc("/api/v1/connections", s.route(true, s.handleConnections, http.MethodGet))
	mux.HandleFunc("/api/v1/traffic", s.route(true, s.handleTraffic, http.MethodGet))
	mux.HandleFunc("/api/v1/domains", s.route(true, s.handleDomains, http.MethodGet))
	mux.HandleFunc("/api/v1/routes", s.route(true, s.handleRoutes, http.MethodGet))
	mux.HandleFunc("/api/v1/dns", s.route(true, s.handleDNS, http.MethodGet))
	mux.HandleFunc("/api/v1/errors", s.route(true, s.handleErrors, http.MethodGet))
	mux.HandleFunc("/api/v1/nodes", s.route(true, s.handleNodes, http.MethodGet))
	mux.HandleFunc("/api/v1/snapshot", s.route(true, s.handleSnapshot, http.MethodGet))
	mux.HandleFunc("/api/v1/reset", s.route(true, s.handleReset, http.MethodPost))

	// Any other /api path: a JSON 404 rather than the dashboard or a
	// plain-text page. It is registered after the real routes, which are
	// more specific and therefore win.
	mux.HandleFunc("/api/", s.handleNotFound)
	return mux
}

// route answers CORS preflight, rejects unexpected methods and, when auth is
// required, rejects requests that do not present the token.
func (s *Server) route(auth bool, next http.HandlerFunc, methods ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.applyCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		allowed := false
		for _, m := range methods {
			if r.Method == m {
				allowed = true
				break
			}
		}
		if !allowed {
			w.Header().Set("Allow", strings.Join(methods, ", "))
			writeError(w, http.StatusMethodNotAllowed, "method "+r.Method+" not allowed")
			return
		}
		if auth && s.cfg.Token != "" && !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	given := requestToken(r)
	if given == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(s.cfg.Token)) == 1
}

// requestToken extracts the token from Authorization, X-API-Token or the
// query string. The query parameter exists so a browser page (or curl) can
// call the API without setting headers.
func requestToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	if v := strings.TrimSpace(r.Header.Get("X-API-Token")); v != "" {
		return v
	}
	return r.URL.Query().Get("token")
}

// applyCORS writes the CORS headers a browser page needs to call the API
// from another origin.
func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	allowed := "*"
	for _, o := range s.cfg.AllowedOrigins {
		if o == "*" {
			allowed = "*"
			break
		}
		if strings.EqualFold(o, origin) {
			allowed = origin
			break
		}
		allowed = ""
	}
	if allowed != "" && origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", allowed)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-API-Token, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal response: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]interface{}{
		"error": msg,
		"now":   time.Now().Unix(),
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "unknown endpoint "+r.URL.Path)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"version": s.deps.Version,
		"now":     time.Now().Unix(),
		"uptime":  int64(time.Since(s.start) / time.Second),
	})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method "+r.Method+" not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML())
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	info := SystemInfo{Version: s.deps.Version}
	if s.deps.Info != nil {
		info = s.deps.Info.APIInfo()
		if info.Version == "" {
			info.Version = s.deps.Version
		}
	}
	cfg := s.deps.Stats.Config()
	oldest, newest := s.deps.Stats.HistoryWindow()

	// Merge per-node traffic into the node list from the engine.
	traffic := s.deps.Stats.Nodes()
	byName := make(map[string]stats.NodeStat, len(traffic))
	for _, n := range traffic {
		byName[n.Name] = n
	}
	for i := range info.Nodes {
		if t, ok := byName[info.Nodes[i].Name]; ok {
			info.Nodes[i].Conns = t.Conns
			info.Nodes[i].Upload = t.Upload
			info.Nodes[i].Download = t.Download
		}
	}
	// Nodes that carried traffic but are no longer configured still count.
	for _, t := range traffic {
		known := false
		for i := range info.Nodes {
			if info.Nodes[i].Name == t.Name {
				known = true
				break
			}
		}
		if !known {
			info.Nodes = append(info.Nodes, NodeInfo{
				Name:     t.Name,
				Conns:    t.Conns,
				Upload:   t.Upload,
				Download: t.Download,
			})
		}
	}

	start := s.deps.Stats.StartTime()
	writeJSON(w, http.StatusOK, StatusResponse{
		SystemInfo: info,
		Now:        time.Now().Unix(),
		Started:    start.Unix(),
		Uptime:     int64(time.Since(start) / time.Second),
		History: HistoryInfo{
			BucketSeconds:   int64(cfg.BucketInterval / time.Second),
			RetentionBucket: cfg.HistoryMinutes,
			Oldest:          oldest.Unix(),
			Newest:          newest.Unix(),
			PersistFile:     cfg.File,
		},
	})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, OverviewResponse{
		Overview: s.deps.Stats.Overview(),
		Version:  s.deps.Version,
	})
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opt := stats.ActiveOptions{
		Limit:    intParam(q.Get("limit"), 100, 1, MaxLimit),
		Proto:    q.Get("proto"),
		Decision: q.Get("decision"),
		Host:     q.Get("host"),
	}
	conns := s.deps.Stats.ActiveSessions(opt)
	writeJSON(w, http.StatusOK, ConnectionsResponse{
		Now:         time.Now().Unix(),
		Count:       len(conns),
		Connections: conns,
	})
}

func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	span := parseSpan(q.Get("range"), time.Hour)
	step := parseStep(q.Get("step"), span)

	now := time.Now()
	points := s.deps.Stats.Series(now.Add(-span), step)
	if len(points) > MaxPoints {
		points = points[len(points)-MaxPoints:]
	}
	writeJSON(w, http.StatusOK, TrafficResponse{
		Range:      fmt.Sprintf("%ds", int(span/time.Second)),
		StepSecond: int64(step / time.Second),
		From:       now.Add(-span).Unix(),
		To:         now.Unix(),
		Points:     points,
	})
}

func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 50, 1, MaxDomainLimit)
	order := q.Get("order")
	if order != "conns" && order != "bytes" {
		order = "bytes"
	}
	domains := s.deps.Stats.Domains(limit, order)
	writeJSON(w, http.StatusOK, DomainsResponse{
		Now:     time.Now().Unix(),
		Count:   len(domains),
		Domains: domains,
	})
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	top := intParam(q.Get("limit"), 10, 1, MaxDomainLimit)

	ov := s.deps.Stats.Overview()
	resp := RoutesResponse{
		Now:              time.Now().Unix(),
		Totals:           ov.Totals,
		Direct:           ov.Direct,
		Proxy:            ov.Proxy,
		TopDirectDomains: s.deps.Stats.TopDomainsByDecision(top, stats.DecisionDirect, "bytes"),
		TopProxyDomains:  s.deps.Stats.TopDomainsByDecision(top, stats.DecisionProxy, "bytes"),
	}
	if n := ov.Direct.Conns + ov.Proxy.Conns; n > 0 {
		resp.DirectConnRatio = float64(ov.Direct.Conns) / float64(n)
		resp.ProxyConnRatio = float64(ov.Proxy.Conns) / float64(n)
	}
	if n := ov.Direct.Total() + ov.Proxy.Total(); n > 0 {
		resp.DirectBytesRatio = float64(ov.Direct.Total()) / float64(n)
		resp.ProxyBytesRatio = float64(ov.Proxy.Total()) / float64(n)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDNS(w http.ResponseWriter, r *http.Request) {
	top := intParam(r.URL.Query().Get("limit"), 20, 1, MaxDomainLimit)
	writeJSON(w, http.StatusOK, s.deps.Stats.DNSStats(top))
}

func (s *Server) handleErrors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 100, 1, MaxErrorLimit)
	scope := strings.ToLower(q.Get("scope"))

	all := s.deps.Stats.Errors(stats.DefaultErrorLimit)
	if scope != "" {
		filtered := make([]stats.ErrorEntry, 0, len(all))
		for _, e := range all {
			if strings.EqualFold(e.Scope, scope) {
				filtered = append(filtered, e)
			}
		}
		all = filtered
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	writeJSON(w, http.StatusOK, ErrorsResponse{
		Now:    time.Now().Unix(),
		Count:  len(all),
		Errors: all,
	})
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	// Reuse the status handler's merge logic by building a status response.
	info := SystemInfo{Version: s.deps.Version}
	if s.deps.Info != nil {
		info = s.deps.Info.APIInfo()
	}
	traffic := s.deps.Stats.Nodes()
	byName := make(map[string]stats.NodeStat, len(traffic))
	for _, n := range traffic {
		byName[n.Name] = n
	}
	for i := range info.Nodes {
		if t, ok := byName[info.Nodes[i].Name]; ok {
			info.Nodes[i].Conns = t.Conns
			info.Nodes[i].Upload = t.Upload
			info.Nodes[i].Download = t.Download
		}
	}
	writeJSON(w, http.StatusOK, NodesResponse{
		Now:   time.Now().Unix(),
		Count: len(info.Nodes),
		Nodes: info.Nodes,
	})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	span := parseSpan(r.URL.Query().Get("range"), 24*time.Hour)
	step := parseStep(r.URL.Query().Get("step"), span)
	now := time.Now()

	points := s.deps.Stats.Series(now.Add(-span), step)
	if len(points) > MaxPoints {
		points = points[len(points)-MaxPoints:]
	}

	snap := SnapshotResponse{
		Overview:    s.deps.Stats.Overview(),
		Traffic:     TrafficResponse{Range: fmt.Sprintf("%ds", int(span/time.Second)), StepSecond: int64(step / time.Second), From: now.Add(-span).Unix(), To: now.Unix(), Points: points},
		Domains:     s.deps.Stats.Domains(200, "bytes"),
		Errors:      s.deps.Stats.Errors(100),
		Connections: s.deps.Stats.ActiveSessions(stats.ActiveOptions{Limit: 200}),
		DNS:         s.deps.Stats.DNSStats(50),
		GeneratedAt: now,
	}

	// Status carries the engine info; routes fill in the split.
	info := SystemInfo{Version: s.deps.Version}
	if s.deps.Info != nil {
		info = s.deps.Info.APIInfo()
	}
	start := s.deps.Stats.StartTime()
	cfg := s.deps.Stats.Config()
	oldest, newest := s.deps.Stats.HistoryWindow()
	snap.Status = StatusResponse{
		SystemInfo: info,
		Now:        now.Unix(),
		Started:    start.Unix(),
		Uptime:     int64(now.Sub(start) / time.Second),
		History:    HistoryInfo{BucketSeconds: int64(cfg.BucketInterval / time.Second), RetentionBucket: cfg.HistoryMinutes, Oldest: oldest.Unix(), Newest: newest.Unix(), PersistFile: cfg.File},
	}
	snap.Nodes = snap.Status.Nodes

	ov := snap.Overview
	snap.Routes = RoutesResponse{
		Now:              now.Unix(),
		Totals:           ov.Totals,
		Direct:           ov.Direct,
		Proxy:            ov.Proxy,
		TopDirectDomains: s.deps.Stats.TopDomainsByDecision(20, stats.DecisionDirect, "bytes"),
		TopProxyDomains:  s.deps.Stats.TopDomainsByDecision(20, stats.DecisionProxy, "bytes"),
	}
	if n := ov.Direct.Conns + ov.Proxy.Conns; n > 0 {
		snap.Routes.DirectConnRatio = float64(ov.Direct.Conns) / float64(n)
		snap.Routes.ProxyConnRatio = float64(ov.Proxy.Conns) / float64(n)
	}
	if n := ov.Direct.Total() + ov.Proxy.Total(); n > 0 {
		snap.Routes.DirectBytesRatio = float64(ov.Direct.Total()) / float64(n)
		snap.Routes.ProxyBytesRatio = float64(ov.Proxy.Total()) / float64(n)
	}

	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.deps.Stats.Reset()
	loggo.Info("[API] Statistics reset by %s", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":   true,
		"now":  time.Now().Unix(),
		"note": "counters cleared; active sessions keep counting",
	})
}

// intParam parses a bounded integer query parameter.
func intParam(raw string, def, min, max int) int {
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// parseSpan parses a duration like "30m", "6h", "24h" or "7d". It falls back
// to def for anything unparsable, and clamps to one year.
func parseSpan(raw string, def time.Duration) time.Duration {
	if raw == "" {
		return def
	}
	d, err := parseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	if d > 365*24*time.Hour {
		return 365 * 24 * time.Hour
	}
	return d
}

// parseDuration accepts a bare number of seconds as well as Go duration
// strings and the compact forms used by dashboards (30m, 12h, 7d).
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if v, err := strconv.Atoi(s); err == nil {
		return time.Duration(v) * time.Second, nil
	}
	mult := time.Second
	switch {
	case strings.HasSuffix(s, "d"):
		mult, s = 24*time.Hour, strings.TrimSuffix(s, "d")
	case strings.HasSuffix(s, "h"):
		mult, s = time.Hour, strings.TrimSuffix(s, "h")
	case strings.HasSuffix(s, "m"):
		mult, s = time.Minute, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "s"):
		mult, s = time.Second, strings.TrimSuffix(s, "s")
	default:
		return time.ParseDuration(s)
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return time.Duration(v) * mult, nil
}

// parseStep picks the aggregation interval: the requested one when valid,
// otherwise one that keeps the chart to roughly 240 points.
func parseStep(raw string, span time.Duration) time.Duration {
	if raw != "" {
		if d, err := parseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	steps := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}
	for _, s := range steps {
		if span/s <= 240 {
			return s
		}
	}
	return 24 * time.Hour
}

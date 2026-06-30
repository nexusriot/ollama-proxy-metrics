package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nexusriot/ollama-proxy-metrics/internal/api"
	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
	"github.com/nexusriot/ollama-proxy-metrics/internal/events"
	"github.com/nexusriot/ollama-proxy-metrics/internal/logging"
	"github.com/nexusriot/ollama-proxy-metrics/internal/pricing"
	"github.com/nexusriot/ollama-proxy-metrics/internal/proxy"
	"github.com/nexusriot/ollama-proxy-metrics/internal/ratelimit"
)

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	var (
		listenAddr  string
		upstreamRaw string
		dbPath      string
		logPath     string
		staticDir   string
		pricingPath string
		rateLimit   int
		logMaxMB    int
		logBackups  int
	)

	flag.StringVar(&listenAddr, "listen", getEnv("LISTEN_ADDR", ":8080"),
		"listen address (env: LISTEN_ADDR)")
	flag.StringVar(&upstreamRaw, "upstream", getEnv("OLLAMA_UPSTREAM", "http://127.0.0.1:11434"),
		"Ollama upstream base URL(s), comma-separated for round-robin (env: OLLAMA_UPSTREAM)")
	flag.StringVar(&dbPath, "db", getEnv("DB_PATH", "/data/db.sqlite"),
		"SQLite database path (env: DB_PATH)")
	flag.StringVar(&logPath, "log", getEnv("LOG_PATH", "/data/logs/proxy.log"),
		"structured JSON log file path (env: LOG_PATH)")
	flag.StringVar(&staticDir, "static", getEnv("STATIC_DIR", ""),
		"directory of frontend static files to serve at / (env: STATIC_DIR)")
	flag.StringVar(&pricingPath, "pricing", getEnv("PRICING_PATH", ""),
		"path to a JSON pricing table for cost estimation (env: PRICING_PATH)")
	flag.IntVar(&rateLimit, "rate-limit", getEnvInt("RATE_LIMIT_RPM", 0),
		"max requests per session per minute; 0 disables (env: RATE_LIMIT_RPM)")
	flag.IntVar(&logMaxMB, "log-max-mb", getEnvInt("LOG_MAX_MB", 50),
		"rotate the log file once it exceeds this many MB; 0 disables rotation (env: LOG_MAX_MB)")
	flag.IntVar(&logBackups, "log-backups", getEnvInt("LOG_MAX_BACKUPS", 5),
		"number of rotated log files to retain (env: LOG_MAX_BACKUPS)")
	flag.Parse()

	logger, logCloser := buildLogger(logPath, logMaxMB, logBackups)
	if logCloser != nil {
		defer func() { _ = logCloser.Close() }()
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}
	store, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer func() { _ = store.Close() }()

	upstreams, err := parseUpstreams(upstreamRaw)
	if err != nil {
		log.Fatalf("invalid upstream: %v", err)
	}

	prices, err := pricing.Load(pricingPath)
	if err != nil {
		log.Fatalf("load pricing: %v", err)
	}

	broker := events.NewBroker()
	limiter := ratelimit.New(rateLimit, time.Minute)

	reg := prometheus.NewRegistry()
	metrics := proxy.NewMetrics(reg)

	proxyHandler := proxy.New(upstreams, store, logger, metrics, prices, broker, limiter)

	mux := http.NewServeMux()

	// Prometheus metrics
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	// Liveness / readiness
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", readyHandler(upstreams))

	// Admin REST API (feeds the React dashboard)
	apiHandler := api.New(store, prices, broker)
	apiHandler.Register(mux, "/admin/api")

	// Optional: serve compiled React frontend from staticDir
	if staticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(staticDir)))
	} else {
		mux.HandleFunc("/", infoPage)
	}

	// Ollama native API + OpenAI-compatible API
	mux.Handle("/api/", proxyHandler)
	mux.Handle("/v1/", proxyHandler)

	upstreamList := make([]string, len(upstreams))
	for i, u := range upstreams {
		upstreamList[i] = u.String()
	}
	log.Printf("starting ollama-proxy on %s  upstreams=%s  db=%s  log=%s  rate-limit-rpm=%d  pricing=%q",
		listenAddr, strings.Join(upstreamList, ","), dbPath, logPath, rateLimit, pricingPath)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// parseUpstreams splits a comma-separated upstream list and validates each URL.
func parseUpstreams(raw string) ([]*url.URL, error) {
	parts := strings.Split(raw, ",")
	var out []*url.URL
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		u, err := url.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("parse upstream %q: %w", p, err)
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no upstream URLs configured")
	}
	return out, nil
}

// readyHandler returns 200 if at least one upstream answers /api/version quickly,
// otherwise 503. Useful as a container/orchestrator readiness probe.
func readyHandler(upstreams []*url.URL) http.HandlerFunc {
	client := &http.Client{Timeout: 2 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, u := range upstreams {
			target := *u
			target.Path = strings.TrimRight(target.Path, "/") + "/api/version"
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				w.WriteHeader(http.StatusOK)
				fmt.Fprintln(w, "ready")
				return
			}
		}
		http.Error(w, "no upstream ready", http.StatusServiceUnavailable)
	}
}

func infoPage(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Ollama metrics proxy")
	fmt.Fprintln(w, "  /api/*       — Ollama native proxy")
	fmt.Fprintln(w, "  /v1/*        — OpenAI-compatible proxy")
	fmt.Fprintln(w, "  /metrics     — Prometheus metrics")
	fmt.Fprintln(w, "  /healthz     — liveness")
	fmt.Fprintln(w, "  /readyz      — readiness (pings upstream)")
	fmt.Fprintln(w, "  /admin/api/* — metrics REST API")
}

// buildLogger creates a slog.Logger that writes JSON to both stdout and a
// (rotating) log file. The returned io.Closer, when non-nil, must be closed on
// shutdown.
func buildLogger(logPath string, maxMB, backups int) (*slog.Logger, io.Closer) {
	writers := []io.Writer{os.Stdout}
	var closer io.Closer

	if logPath != "" {
		rw, err := logging.NewRotatingWriter(logPath, int64(maxMB)*1024*1024, backups)
		if err != nil {
			log.Printf("warn: cannot open log file %s: %v", logPath, err)
		} else {
			writers = append(writers, rw)
			closer = rw
		}
	}

	w := io.MultiWriter(writers...)
	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	return logger, closer
}

package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nexusriot/ollama-proxy-metrics/internal/api"
	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
	"github.com/nexusriot/ollama-proxy-metrics/internal/proxy"
)

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
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
	)

	flag.StringVar(&listenAddr, "listen", getEnv("LISTEN_ADDR", ":8080"),
		"listen address (env: LISTEN_ADDR)")
	flag.StringVar(&upstreamRaw, "upstream", getEnv("OLLAMA_UPSTREAM", "http://127.0.0.1:11434"),
		"Ollama upstream base URL (env: OLLAMA_UPSTREAM)")
	flag.StringVar(&dbPath, "db", getEnv("DB_PATH", "/data/db.sqlite"),
		"SQLite database path (env: DB_PATH)")
	flag.StringVar(&logPath, "log", getEnv("LOG_PATH", "/data/logs/proxy.log"),
		"structured JSON log file path (env: LOG_PATH)")
	flag.StringVar(&staticDir, "static", getEnv("STATIC_DIR", ""),
		"directory of frontend static files to serve at / (env: STATIC_DIR)")
	flag.Parse()

	logger := buildLogger(logPath)

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("create db dir: %v", err)
	}
	store, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer func() { _ = store.Close() }()

	upstreamURL, err := url.Parse(upstreamRaw)
	if err != nil {
		log.Fatalf("invalid upstream URL %q: %v", upstreamRaw, err)
	}

	reg := prometheus.NewRegistry()
	metrics := proxy.NewMetrics(reg)

	proxyHandler := proxy.New(upstreamURL, store, logger, metrics)

	mux := http.NewServeMux()

	// Prometheus metrics
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	// Admin REST API (feeds the React dashboard)
	apiHandler := api.New(store)
	apiHandler.Register(mux, "/admin/api")

	// Optional: serve compiled React frontend from staticDir
	if staticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(staticDir)))
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintln(w, "Ollama metrics proxy")
			fmt.Fprintln(w, "  /api/*       — Ollama proxy")
			fmt.Fprintln(w, "  /metrics     — Prometheus metrics")
			fmt.Fprintln(w, "  /admin/api/* — metrics REST API")
		})
	}

	// All Ollama API endpoints
	mux.Handle("/api/", proxyHandler)

	log.Printf("starting ollama-proxy on %s  upstream=%s  db=%s  log=%s",
		listenAddr, upstreamURL, dbPath, logPath)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// buildLogger creates a slog.Logger that writes JSON to both stdout and logPath.
func buildLogger(logPath string) *slog.Logger {
	writers := []io.Writer{os.Stdout}

	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			log.Printf("warn: cannot create log dir %s: %v", filepath.Dir(logPath), err)
		} else {
			f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				log.Printf("warn: cannot open log file %s: %v", logPath, err)
			} else {
				writers = append(writers, f)
			}
		}
	}

	w := io.MultiWriter(writers...)
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

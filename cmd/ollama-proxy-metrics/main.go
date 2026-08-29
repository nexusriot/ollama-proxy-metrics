package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nexusriot/ollama-proxy-metrics/internal/api"
	"github.com/nexusriot/ollama-proxy-metrics/internal/budget"
	"github.com/nexusriot/ollama-proxy-metrics/internal/cache"
	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
	"github.com/nexusriot/ollama-proxy-metrics/internal/events"
	"github.com/nexusriot/ollama-proxy-metrics/internal/gate"
	"github.com/nexusriot/ollama-proxy-metrics/internal/logging"
	"github.com/nexusriot/ollama-proxy-metrics/internal/modelname"
	"github.com/nexusriot/ollama-proxy-metrics/internal/pricing"
	"github.com/nexusriot/ollama-proxy-metrics/internal/proxy"
	"github.com/nexusriot/ollama-proxy-metrics/internal/ratelimit"
	"github.com/nexusriot/ollama-proxy-metrics/internal/upstream"
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

func getEnvInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// config holds every resolved setting, so main stays a wiring list.
type config struct {
	listenAddr  string
	upstreamRaw string
	dbPath      string
	logPath     string
	staticDir   string
	pricingPath string
	rateLimit   int
	logMaxMB    int
	logBackups  int

	maxBodyMB     int64
	maxConcurrent int
	maxQueue      int

	cacheTTL   time.Duration
	cacheMaxMB int64

	budgetTokens int64
	budgetCost   float64

	normalizeModels   bool
	modelAliasesRaw   string
	normalizeBackfill bool
	injectUsage       bool
	estimateTokens    bool

	upstreamPoll    time.Duration
	circuitFailures int
	circuitCooldown time.Duration
	retryStatus     bool

	shutdownGrace time.Duration
}

func parseFlags() config {
	var c config

	flag.StringVar(&c.listenAddr, "listen", getEnv("LISTEN_ADDR", ":8080"),
		"listen address (env: LISTEN_ADDR)")
	flag.StringVar(&c.upstreamRaw, "upstream", getEnv("OLLAMA_UPSTREAM", "http://127.0.0.1:11434"),
		"Ollama upstream base URL(s), comma-separated for round-robin (env: OLLAMA_UPSTREAM)")
	flag.StringVar(&c.dbPath, "db", getEnv("DB_PATH", "/data/db.sqlite"),
		"SQLite database path (env: DB_PATH)")
	flag.StringVar(&c.logPath, "log", getEnv("LOG_PATH", "/data/logs/proxy.log"),
		"structured JSON log file path (env: LOG_PATH)")
	flag.StringVar(&c.staticDir, "static", getEnv("STATIC_DIR", ""),
		"directory of frontend static files to serve at / (env: STATIC_DIR)")
	flag.StringVar(&c.pricingPath, "pricing", getEnv("PRICING_PATH", ""),
		"path to a JSON pricing table for cost estimation (env: PRICING_PATH)")
	flag.IntVar(&c.rateLimit, "rate-limit", getEnvInt("RATE_LIMIT_RPM", 0),
		"max requests per session per minute; 0 disables (env: RATE_LIMIT_RPM)")
	flag.IntVar(&c.logMaxMB, "log-max-mb", getEnvInt("LOG_MAX_MB", 50),
		"rotate the log file once it exceeds this many MB; 0 disables rotation (env: LOG_MAX_MB)")
	flag.IntVar(&c.logBackups, "log-backups", getEnvInt("LOG_MAX_BACKUPS", 5),
		"number of rotated log files to retain (env: LOG_MAX_BACKUPS)")

	flag.Int64Var(&c.maxBodyMB, "max-body-mb", getEnvInt64("MAX_BODY_MB", 32),
		"reject request bodies larger than this many MB; 0 disables the cap (env: MAX_BODY_MB)")
	flag.IntVar(&c.maxConcurrent, "max-concurrent", getEnvInt("MAX_CONCURRENT", 0),
		"max requests forwarded upstream at once; 0 disables queueing (env: MAX_CONCURRENT)")
	flag.IntVar(&c.maxQueue, "max-queue", getEnvInt("MAX_QUEUE", 0),
		"max requests waiting for a slot before returning 503; 0 is unbounded (env: MAX_QUEUE)")

	flag.DurationVar(&c.cacheTTL, "cache-ttl", getEnvDuration("CACHE_TTL", 0),
		"cache identical non-streaming responses for this long; 0 disables (env: CACHE_TTL)")
	flag.Int64Var(&c.cacheMaxMB, "cache-max-mb", getEnvInt64("CACHE_MAX_MB", 64),
		"memory budget for the response cache in MB (env: CACHE_MAX_MB)")

	flag.Int64Var(&c.budgetTokens, "budget-tokens", getEnvInt64("BUDGET_TOKENS_PER_DAY", 0),
		"per-session daily token ceiling; 0 disables (env: BUDGET_TOKENS_PER_DAY)")
	flag.Float64Var(&c.budgetCost, "budget-cost", getEnvFloat("BUDGET_COST_PER_DAY", 0),
		"per-session daily cost ceiling; 0 disables (env: BUDGET_COST_PER_DAY)")

	flag.BoolVar(&c.normalizeModels, "normalize-models", getEnvBool("NORMALIZE_MODELS", true),
		"record \"llama3\" as \"llama3:latest\" so one model is one series (env: NORMALIZE_MODELS)")
	flag.StringVar(&c.modelAliasesRaw, "model-aliases", getEnv("MODEL_ALIASES", ""),
		"comma-separated alias=target list folding model names together (env: MODEL_ALIASES)")
	flag.BoolVar(&c.normalizeBackfill, "normalize-backfill", getEnvBool("NORMALIZE_BACKFILL", false),
		"rewrite existing rows to normalized model names at startup (env: NORMALIZE_BACKFILL)")
	flag.BoolVar(&c.injectUsage, "inject-usage", getEnvBool("INJECT_USAGE", true),
		"ask OpenAI-compatible streams to report token usage (env: INJECT_USAGE)")
	flag.BoolVar(&c.estimateTokens, "estimate-tokens", getEnvBool("ESTIMATE_TOKENS", false),
		"estimate token counts from text when the upstream reports none (env: ESTIMATE_TOKENS)")

	flag.BoolVar(&c.retryStatus, "retry-5xx", getEnvBool("RETRY_5XX", true),
		"retry a 5xx response on the next upstream (env: RETRY_5XX)")
	flag.DurationVar(&c.upstreamPoll, "upstream-poll", getEnvDuration("UPSTREAM_POLL", 30*time.Second),
		"how often to poll each upstream's model list for routing; 0 disables (env: UPSTREAM_POLL)")
	flag.IntVar(&c.circuitFailures, "circuit-failures", getEnvInt("CIRCUIT_FAILURES", 3),
		"consecutive failures before an upstream is skipped (env: CIRCUIT_FAILURES)")
	flag.DurationVar(&c.circuitCooldown, "circuit-cooldown", getEnvDuration("CIRCUIT_COOLDOWN", 30*time.Second),
		"how long a failing upstream is skipped for (env: CIRCUIT_COOLDOWN)")

	flag.DurationVar(&c.shutdownGrace, "shutdown-grace", getEnvDuration("SHUTDOWN_GRACE", 30*time.Second),
		"how long in-flight requests may finish after a shutdown signal (env: SHUTDOWN_GRACE)")

	flag.Parse()
	return c
}

func main() {
	if err := run(parseFlags()); err != nil {
		log.Fatal(err)
	}
}

// run owns the whole process lifetime. Everything it opens is closed on the way
// out, which is why it returns an error instead of calling log.Fatal: a
// log.Fatal deeper in the wiring would skip every deferred close.
func run(c config) error {
	logger, logCloser := buildLogger(c.logPath, c.logMaxMB, c.logBackups)
	if logCloser != nil {
		defer func() { _ = logCloser.Close() }()
	}

	if err := os.MkdirAll(filepath.Dir(c.dbPath), 0o755); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}
	store, err := db.Open(c.dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = store.Close() }()

	upstreams, err := parseUpstreams(c.upstreamRaw)
	if err != nil {
		return fmt.Errorf("invalid upstream: %w", err)
	}

	prices, err := pricing.Load(c.pricingPath)
	if err != nil {
		return fmt.Errorf("load pricing: %w", err)
	}

	aliases, err := modelname.ParseAliases(c.modelAliasesRaw)
	if err != nil {
		return fmt.Errorf("model aliases: %w", err)
	}
	if c.normalizeBackfill {
		n, err := backfillModelNames(store, aliases)
		if err != nil {
			return fmt.Errorf("normalize backfill: %w", err)
		}
		log.Printf("normalized model names on %d existing rows", n)
	}

	broker := events.NewBroker()
	limiter := ratelimit.New(c.rateLimit, time.Minute)
	budgets := budget.New(budget.Limits{
		TokensPerDay: c.budgetTokens,
		CostPerDay:   c.budgetCost,
	})
	if err := seedBudgets(store, budgets); err != nil {
		return fmt.Errorf("seed budgets: %w", err)
	}
	slots := gate.New(c.maxConcurrent, c.maxQueue)
	responses := cache.New(c.cacheTTL, c.cacheMaxMB*1024*1024)

	reg := prometheus.NewRegistry()
	metrics := proxy.NewMetrics(reg)
	registerCacheGauges(reg, responses)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool := upstream.New(upstream.Options{
		URLs:             upstreams,
		PollInterval:     c.upstreamPoll,
		FailureThreshold: c.circuitFailures,
		Cooldown:         c.circuitCooldown,
		Logger:           logger,
		OnRefresh:        metrics.ObserveUpstreams,
	})
	pool.Start(ctx)

	proxyHandler := proxy.New(proxy.Options{
		Pool:            pool,
		Store:           store,
		Logger:          logger,
		Metrics:         metrics,
		Prices:          prices,
		Events:          broker,
		Limiter:         limiter,
		Budgets:         budgets,
		Gate:            slots,
		Cache:           responses,
		MaxBodyBytes:    c.maxBodyMB * 1024 * 1024,
		NormalizeModels: c.normalizeModels,
		ModelAliases:    aliases,
		InjectUsage:     c.injectUsage,
		EstimateTokens:  c.estimateTokens,
		RetryStatus:     c.retryStatus,
	})

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
	apiHandler := api.New(api.Options{
		Store:   store,
		Prices:  prices,
		Events:  broker,
		Budgets: budgets,
	})
	apiHandler.Register(mux, "/admin/api")

	// Optional: serve compiled React frontend from staticDir
	if c.staticDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(c.staticDir)))
	} else {
		mux.HandleFunc("/", infoPage)
	}

	// Ollama native API + OpenAI-compatible API
	mux.Handle("/api/", proxyHandler)
	mux.Handle("/v1/", proxyHandler)

	ln, err := net.Listen("tcp", c.listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", c.listenAddr, err)
	}

	upstreamList := make([]string, len(upstreams))
	for i, u := range upstreams {
		upstreamList[i] = u.String()
	}
	log.Printf("starting ollama-proxy on %s  upstreams=%s  db=%s  log=%s  rate-limit-rpm=%d  pricing=%q",
		c.listenAddr, strings.Join(upstreamList, ","), c.dbPath, c.logPath, c.rateLimit, c.pricingPath)

	srv := &http.Server{Handler: mux}
	if err := serve(ctx, srv, ln, c.shutdownGrace); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	log.Print("shutdown complete")
	return nil
}

// serve runs srv until ctx is cancelled, then stops accepting new connections
// and gives in-flight requests up to grace to finish. Without this a SIGTERM
// would kill open streams mid-generation and skip the final DB write and log
// flush that the deferred closes in run perform.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration) error {
	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Printf("shutdown signal received, finishing in-flight requests (grace %s)", grace)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-errCh
}

// seedBudgets primes the in-memory budget tracker with today's usage already in
// SQLite, so a restart resumes the day's accounting instead of handing every
// session a fresh allowance.
func seedBudgets(store *db.Store, tracker *budget.Tracker) error {
	if !tracker.Enabled() {
		return nil
	}
	day := tracker.Day()
	rows, err := store.UsageSince(day + "T00:00:00Z")
	if err != nil {
		return err
	}
	usage := make(map[string]budget.Usage, len(rows))
	for _, r := range rows {
		usage[r.SessionID] = budget.Usage{Tokens: r.Tokens, Cost: r.Cost}
	}
	tracker.Seed(day, usage)
	return nil
}

// backfillModelNames rewrites historical rows to normalized model names so that
// statistics recorded before normalization was enabled merge with the new ones.
func backfillModelNames(store *db.Store, aliases map[string]string) (int64, error) {
	models, err := store.Models()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, m := range models {
		normalized := modelname.Normalize(m, aliases)
		if normalized == m {
			continue
		}
		n, err := store.RenameModel(m, normalized)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// registerCacheGauges exports the response cache's occupancy, when it is enabled.
func registerCacheGauges(reg prometheus.Registerer, c *cache.Cache) {
	if !c.Enabled() {
		return
	}
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "ollama_proxy_cache_entries",
		Help: "Responses currently held in the proxy response cache.",
	}, func() float64 { return float64(c.Len()) }))
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "ollama_proxy_cache_bytes",
		Help: "Bytes currently held in the proxy response cache.",
	}, func() float64 { return float64(c.Bytes()) }))
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

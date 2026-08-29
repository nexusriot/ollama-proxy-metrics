// Package proxy implements the Ollama reverse-proxy with Prometheus metrics,
// structured request logging, and per-request SQLite persistence. It transparently
// proxies both Ollama's native /api/* surface and the OpenAI-compatible /v1/*
// surface, extracting token counts from either wire format.
package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nexusriot/ollama-proxy-metrics/internal/budget"
	"github.com/nexusriot/ollama-proxy-metrics/internal/cache"
	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
	"github.com/nexusriot/ollama-proxy-metrics/internal/events"
	"github.com/nexusriot/ollama-proxy-metrics/internal/gate"
	"github.com/nexusriot/ollama-proxy-metrics/internal/modelname"
	"github.com/nexusriot/ollama-proxy-metrics/internal/pricing"
	"github.com/nexusriot/ollama-proxy-metrics/internal/ratelimit"
	"github.com/nexusriot/ollama-proxy-metrics/internal/tokens"
	"github.com/nexusriot/ollama-proxy-metrics/internal/upstream"
)

// StatusClientClosed is the non-standard status nginx uses for "the client hung
// up before the response was ready". Nothing is sent to the client — it exists
// so an abandoned request is recorded as abandoned rather than as a success.
const StatusClientClosed = 499

// drainLimit is how much of a discarded upstream response is read before the
// connection is returned to the pool. Reading a little keeps the connection
// reusable; reading all of a large error page would waste time on a failover.
const drainLimit = 64 << 10

// requestPayload is the minimal incoming JSON shape we care about. The fields
// overlap across Ollama native and OpenAI-compatible requests.
type requestPayload struct {
	Model    string          `json:"model"`
	Stream   *bool           `json:"stream,omitempty"`
	Prompt   string          `json:"prompt,omitempty"`   // /api/generate, /v1/completions
	Messages []chatMessage   `json:"messages,omitempty"` // /api/chat, /v1/chat/completions
	Input    json.RawMessage `json:"input,omitempty"`    // /api/embed, /v1/embeddings: string or []string
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ollamaChunk covers both final non-stream responses and every streaming chunk
// of Ollama's native API.
type ollamaChunk struct {
	Done            bool         `json:"done"`
	Response        string       `json:"response,omitempty"` // /api/generate
	Message         *chatMessage `json:"message,omitempty"`  // /api/chat
	EvalCount       *int64       `json:"eval_count,omitempty"`
	PromptEvalCount *int64       `json:"prompt_eval_count,omitempty"`
}

// openAIChunk covers OpenAI-compatible responses (both the single non-stream
// object and each `data:` SSE event for streaming).
type openAIChunk struct {
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage"`
}

type openAIChoice struct {
	Delta   *openAIContent `json:"delta"`   // streaming
	Message *openAIContent `json:"message"` // non-stream chat
	Text    string         `json:"text"`    // legacy completions
}

type openAIContent struct {
	Content string `json:"content"`
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func openAIText(c openAIChunk) string {
	if len(c.Choices) == 0 {
		return ""
	}
	ch := c.Choices[0]
	if ch.Delta != nil {
		return ch.Delta.Content
	}
	if ch.Message != nil {
		return ch.Message.Content
	}
	return ch.Text
}

// sseData extracts the JSON payload from an SSE `data:` line, reporting false for
// blank lines, comments, the `[DONE]` sentinel, and non-data fields.
func sseData(line []byte) ([]byte, bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return nil, false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	return payload, true
}

// extractPromptText returns the user-facing prompt from the parsed request.
// For generate/completions it uses "prompt"; for chat the content of the last
// "user" message; for embeddings the "input" (string or []string).
func extractPromptText(p requestPayload) string {
	if p.Prompt != "" {
		return p.Prompt
	}
	if len(p.Input) > 0 {
		var s string
		if json.Unmarshal(p.Input, &s) == nil {
			return s
		}
		var ss []string
		if json.Unmarshal(p.Input, &ss) == nil {
			return strings.Join(ss, " ")
		}
	}
	for i := len(p.Messages) - 1; i >= 0; i-- {
		if p.Messages[i].Role == "user" {
			return p.Messages[i].Content
		}
	}
	return ""
}

// responseText returns the assistant's text from a parsed native Ollama chunk.
func responseText(c ollamaChunk) string {
	if c.Response != "" {
		return c.Response
	}
	if c.Message != nil {
		return c.Message.Content
	}
	return ""
}

// Metrics bundles all Prometheus counters/histograms for the proxy.
type Metrics struct {
	ReqTotal    *prometheus.CounterVec
	ReqDuration *prometheus.HistogramVec
	TTFT        *prometheus.HistogramVec
	BytesIn     *prometheus.CounterVec
	BytesOut    *prometheus.CounterVec
	TokensIn    *prometheus.CounterVec
	TokensOut   *prometheus.CounterVec
	CostTotal   *prometheus.CounterVec
	RateLimited prometheus.Counter

	EstimatedTokens  *prometheus.CounterVec
	BudgetDenied     *prometheus.CounterVec
	Inflight         prometheus.Gauge
	QueueWait        prometheus.Histogram
	QueueRejected    prometheus.Counter
	CacheHits        *prometheus.CounterVec
	CacheMisses      *prometheus.CounterVec
	CacheSavedTokens *prometheus.CounterVec
	UpstreamUp       *prometheus.GaugeVec
	UpstreamRequests *prometheus.CounterVec
	Retries          *prometheus.CounterVec
}

// NewMetrics creates and registers a fresh set of Prometheus metrics using reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ReqTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_requests_total",
			Help: "Total requests handled by the Ollama proxy.",
		}, []string{"endpoint", "model", "status", "stream"}),

		ReqDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ollama_proxy_request_duration_seconds",
			Help:    "Duration of Ollama requests handled by the proxy.",
			Buckets: prometheus.DefBuckets,
		}, []string{"endpoint", "model", "stream"}),

		TTFT: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ollama_proxy_time_to_first_token_seconds",
			Help:    "Time to first streamed token (streaming requests only).",
			Buckets: prometheus.DefBuckets,
		}, []string{"endpoint", "model"}),

		BytesIn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_request_bytes_in_total",
			Help: "Total bytes received in request bodies.",
		}, []string{"endpoint", "model", "stream"}),

		BytesOut: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_response_bytes_out_total",
			Help: "Total bytes sent in response bodies.",
		}, []string{"endpoint", "model", "stream"}),

		TokensIn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_prompt_tokens_total",
			Help: "Total prompt tokens (from model eval stats).",
		}, []string{"endpoint", "model"}),

		TokensOut: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_completion_tokens_total",
			Help: "Total completion tokens (from model eval stats).",
		}, []string{"endpoint", "model"}),

		CostTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_cost_total",
			Help: "Total estimated cost from the configured pricing table.",
		}, []string{"model"}),

		RateLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ollama_proxy_rate_limited_total",
			Help: "Total requests rejected by the per-session rate limiter.",
		}),

		// Estimated tokens are kept out of the prompt/completion counters on
		// purpose: those are sourced from the model's own eval stats and stay
		// exact, so a dashboard can show measured and guessed side by side.
		EstimatedTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_estimated_tokens_total",
			Help: "Total tokens estimated from text when the upstream reported none.",
		}, []string{"endpoint", "model", "kind"}),

		BudgetDenied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_budget_denied_total",
			Help: "Total requests refused because a session exhausted its daily budget.",
		}, []string{"reason"}),

		Inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ollama_proxy_inflight_requests",
			Help: "Requests currently being proxied.",
		}),

		QueueWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "ollama_proxy_queue_wait_seconds",
			Help:    "Time spent waiting for a concurrency slot before forwarding.",
			Buckets: prometheus.DefBuckets,
		}),

		QueueRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ollama_proxy_queue_rejected_total",
			Help: "Total requests refused because the concurrency queue was full.",
		}),

		CacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_cache_hits_total",
			Help: "Total responses served from the proxy response cache.",
		}, []string{"endpoint", "model"}),

		CacheMisses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_cache_misses_total",
			Help: "Total cacheable requests that had to be forwarded upstream.",
		}, []string{"endpoint", "model"}),

		CacheSavedTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_cache_saved_tokens_total",
			Help: "Total tokens not regenerated thanks to a cache hit.",
		}, []string{"endpoint", "model"}),

		UpstreamUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ollama_proxy_upstream_up",
			Help: "1 when an upstream answered its last inventory poll, 0 otherwise.",
		}, []string{"upstream"}),

		UpstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_upstream_requests_total",
			Help: "Total requests forwarded, by upstream and response status.",
		}, []string{"upstream", "status"}),

		Retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_upstream_retries_total",
			Help: "Total forwarding attempts retried on another upstream.",
		}, []string{"reason"}),
	}
	reg.MustRegister(
		m.ReqTotal, m.ReqDuration, m.TTFT, m.BytesIn, m.BytesOut,
		m.TokensIn, m.TokensOut, m.CostTotal, m.RateLimited,
		m.EstimatedTokens, m.BudgetDenied, m.Inflight, m.QueueWait, m.QueueRejected,
		m.CacheHits, m.CacheMisses, m.CacheSavedTokens,
		m.UpstreamUp, m.UpstreamRequests, m.Retries,
	)
	return m
}

// ObserveUpstreams mirrors a pool snapshot into the upstream health gauge.
func (m *Metrics) ObserveUpstreams(states []upstream.State) {
	for _, s := range states {
		up := 0.0
		if s.Up {
			up = 1
		}
		m.UpstreamUp.WithLabelValues(s.URL).Set(up)
	}
}

// Handler is the proxy HTTP handler.
type Handler struct {
	pool       *upstream.Pool
	httpClient *http.Client
	store      *db.Store
	logger     *slog.Logger
	metrics    *Metrics
	prices     *pricing.Table
	events     *events.Broker
	limiter    *ratelimit.Limiter
	budgets    *budget.Tracker
	gate       *gate.Gate
	cache      *cache.Cache

	maxBodyBytes    int64
	normalizeModels bool
	modelAliases    map[string]string
	injectUsage     bool
	estimateTokens  bool
	retryStatus     bool
}

// Options configures a proxy Handler. Only Store, Logger, Metrics and one of
// Upstreams/Pool are required; every other field is optional and disables the
// corresponding feature when left at its zero value.
type Options struct {
	// Upstreams are the Ollama base URLs. Ignored when Pool is set.
	Upstreams []*url.URL
	// Pool orders the upstreams per request and tracks their health. When nil,
	// one is built from Upstreams with health polling disabled.
	Pool *upstream.Pool

	Store   *db.Store
	Logger  *slog.Logger
	Metrics *Metrics
	Prices  *pricing.Table
	Events  *events.Broker
	Limiter *ratelimit.Limiter
	Budgets *budget.Tracker
	Gate    *gate.Gate
	Cache   *cache.Cache

	// MaxBodyBytes caps the request body the proxy will buffer. 0 is unlimited.
	MaxBodyBytes int64
	// NormalizeModels folds "llama3" into "llama3:latest" before recording.
	NormalizeModels bool
	// ModelAliases renames models before recording; requires NormalizeModels.
	ModelAliases map[string]string
	// InjectUsage asks OpenAI-compatible streams to report their token usage.
	InjectUsage bool
	// EstimateTokens fills in token counts from text when the upstream reports none.
	EstimateTokens bool
	// RetryStatus retries a 5xx response on the next upstream.
	RetryStatus bool
}

// New creates a new proxy Handler from opts.
func New(opts Options) *Handler {
	pool := opts.Pool
	if pool == nil {
		pool = upstream.New(upstream.Options{URLs: opts.Upstreams, Logger: opts.Logger})
	}
	return &Handler{
		pool: pool,
		httpClient: &http.Client{
			// No overall timeout – long/streaming requests need an open connection.
			Timeout: 0,
		},
		store:           opts.Store,
		logger:          opts.Logger,
		metrics:         opts.Metrics,
		prices:          opts.Prices,
		events:          opts.Events,
		limiter:         opts.Limiter,
		budgets:         opts.Budgets,
		gate:            opts.Gate,
		cache:           opts.Cache,
		maxBodyBytes:    opts.MaxBodyBytes,
		normalizeModels: opts.NormalizeModels,
		modelAliases:    opts.ModelAliases,
		injectUsage:     opts.InjectUsage,
		estimateTokens:  opts.EstimateTokens,
		retryStatus:     opts.RetryStatus,
	}
}

// Pool exposes the upstream pool backing this handler.
func (h *Handler) Pool() *upstream.Pool { return h.pool }

// reqCtx carries the per-request context shared between the stream and
// non-stream response handlers.
type reqCtx struct {
	reqID       string
	sessionID   string
	clientIP    string
	endpoint    string
	method      string
	model       string
	promptText  string
	streamLabel string
	reqBytes    int64
	isOpenAI    bool
	start       time.Time
	cacheKey    string
}

// ServeHTTP implements http.Handler; proxies /api/* and /v1/* to an upstream.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	sessionID := extractSessionID(r)
	clientIP := extractClientIP(r)
	endpoint := r.URL.Path

	h.metrics.Inflight.Inc()
	defer h.metrics.Inflight.Dec()

	// Per-session rate limiting (optional).
	if h.limiter.Enabled() && !h.limiter.Allow(sessionID) {
		h.metrics.RateLimited.Inc()
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limit exceeded for session", http.StatusTooManyRequests)
		h.recordError(reqID, sessionID, endpoint, r, start, clientIP,
			http.StatusTooManyRequests, 0, 0, "rate limit exceeded")
		return
	}

	// Daily token/cost budget (optional). Checked before the body is read: an
	// exhausted session should not get to spend memory either.
	if ok, reason := h.budgets.Check(sessionID); !ok {
		h.metrics.BudgetDenied.WithLabelValues(string(reason)).Inc()
		retryAfter := h.budgets.RetryAfter()
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
		http.Error(w, "daily budget exceeded for session: "+string(reason), http.StatusTooManyRequests)
		h.recordError(reqID, sessionID, endpoint, r, start, clientIP,
			http.StatusTooManyRequests, 0, 0, "budget exceeded: "+string(reason))
		return
	}

	bodyBuf, err := h.readBody(w, r)
	if err != nil {
		status := http.StatusBadRequest
		msg := "read body: " + err.Error()
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
			msg = fmt.Sprintf("request body exceeds the %d byte limit", h.maxBodyBytes)
		}
		http.Error(w, msg, status)
		h.recordError(reqID, sessionID, endpoint, r, start, clientIP,
			status, int64(len(bodyBuf)), 0, msg)
		return
	}

	var payload requestPayload
	_ = json.Unmarshal(bodyBuf, &payload) // best-effort

	isOpenAI := strings.HasPrefix(endpoint, "/v1/")
	promptText := extractPromptText(payload)
	model := h.resolveModel(payload.Model)
	stream := determineStream(r.Method, endpoint, isOpenAI, payload.Stream)
	streamLabel := strconv.FormatBool(stream)

	h.metrics.BytesIn.WithLabelValues(endpoint, model, streamLabel).Add(float64(len(bodyBuf)))

	rc := reqCtx{
		reqID:       reqID,
		sessionID:   sessionID,
		clientIP:    clientIP,
		endpoint:    endpoint,
		method:      r.Method,
		model:       model,
		promptText:  promptText,
		streamLabel: streamLabel,
		reqBytes:    int64(len(bodyBuf)),
		isOpenAI:    isOpenAI,
		start:       start,
	}

	// Cache lookup happens before the concurrency gate: a hit costs no upstream
	// work, so it has no business queueing behind requests that do.
	if h.cacheable(r.Method, endpoint, stream) {
		rc.cacheKey = cache.Key(endpoint, r.Method, string(bodyBuf))
		if entry, ok := h.cache.Get(rc.cacheKey); ok {
			h.metrics.CacheHits.WithLabelValues(endpoint, model).Inc()
			h.serveFromCache(w, r, rc, entry)
			return
		}
		h.metrics.CacheMisses.WithLabelValues(endpoint, model).Inc()
	}

	if r.Method == http.MethodPost && h.gate.Enabled() {
		release, err := h.acquireSlot(w, r, rc)
		if err != nil {
			return
		}
		defer release()
	}

	forwardBody := bodyBuf
	if h.injectUsage && isOpenAI && stream {
		forwardBody = injectStreamUsage(bodyBuf)
	}

	resp, err := h.forward(r, endpoint, model, forwardBody)
	if err != nil {
		statusCode := http.StatusBadGateway
		h.metrics.ReqTotal.WithLabelValues(endpoint, model, strconv.Itoa(statusCode), streamLabel).Inc()
		h.metrics.ReqDuration.WithLabelValues(endpoint, model, streamLabel).Observe(time.Since(start).Seconds())
		http.Error(w, "upstream error", statusCode)
		h.recordError(reqID, sessionID, endpoint, r, start, clientIP,
			statusCode, int64(len(bodyBuf)), 0, "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Copy response headers.
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if rc.cacheKey != "" {
		w.Header().Set("X-Proxy-Cache", "miss")
	}
	w.WriteHeader(resp.StatusCode)

	if stream {
		h.serveStream(w, resp, r, rc)
	} else {
		h.serveNonStream(w, resp, r, rc)
	}
}

// readBody buffers the request body, enforcing the configured size cap. The body
// must be buffered because it is both parsed for metadata and replayed to the
// upstream (possibly more than once, on failover).
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	body := r.Body
	if h.maxBodyBytes > 0 {
		body = http.MaxBytesReader(w, body, h.maxBodyBytes)
	}
	return io.ReadAll(body)
}

// resolveModel returns the name a request should be recorded and routed under.
func (h *Handler) resolveModel(model string) string {
	if model == "" {
		return modelname.Unknown
	}
	if h.normalizeModels {
		return modelname.Normalize(model, h.modelAliases)
	}
	return model
}

// cacheable reports whether this request is a candidate for the response cache.
// Only non-streaming POSTs qualify: a stream has no single body to replay.
func (h *Handler) cacheable(method, endpoint string, stream bool) bool {
	return h.cache.Enabled() && method == http.MethodPost && !stream &&
		!strings.HasSuffix(endpoint, "/api/tags")
}

// acquireSlot waits for a concurrency slot, answering the client itself when the
// queue is full or the client goes away. A non-nil error means the request is
// already finished and the caller must return.
func (h *Handler) acquireSlot(w http.ResponseWriter, r *http.Request, rc reqCtx) (func(), error) {
	waitStart := time.Now()
	release, err := h.gate.Acquire(r.Context())
	wait := time.Since(waitStart)

	switch {
	case err == nil:
		h.metrics.QueueWait.Observe(wait.Seconds())
		return release, nil

	case errors.Is(err, gate.ErrQueueFull):
		h.metrics.QueueRejected.Inc()
		w.Header().Set("Retry-After", "1")
		http.Error(w, "proxy is at capacity, retry shortly", http.StatusServiceUnavailable)
		h.recordError(rc.reqID, rc.sessionID, rc.endpoint, r, rc.start, rc.clientIP,
			http.StatusServiceUnavailable, rc.reqBytes, 0, "concurrency queue full")
		return nil, err

	default:
		// The client hung up while queued: nothing to write, but the abandoned
		// request is still worth recording.
		h.recordError(rc.reqID, rc.sessionID, rc.endpoint, r, rc.start, rc.clientIP,
			StatusClientClosed, rc.reqBytes, 0, "client cancelled while queued: "+err.Error())
		return nil, err
	}
}

// serveFromCache replays a stored response to the client and records the request
// as a cache hit. No upstream work happened, so the tokens it would have cost are
// reported as saved rather than spent.
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, rc reqCtx, entry *cache.Entry) {
	for k, vals := range entry.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Proxy-Cache", "hit")
	w.WriteHeader(entry.Status)
	h.completeNonStream(w, r, rc, entry.Status, entry.Body, "", true)
}

// forward sends the buffered request to an upstream, walking the pool's ordered
// candidate list. It moves on to the next upstream when one cannot be reached,
// when it answers 5xx, or when it answers 404 for a model another upstream is
// known to serve.
func (h *Handler) forward(r *http.Request, endpoint, model string, body []byte) (*http.Response, error) {
	candidates := h.pool.Pick(model)
	if len(candidates) == 0 {
		return nil, errors.New("no upstreams configured")
	}

	var lastErr error
	for i, base := range candidates {
		last := i == len(candidates)-1

		upReq, err := h.buildUpstreamRequest(r, base, endpoint, body)
		if err != nil {
			lastErr = err
			continue
		}

		resp, err := h.httpClient.Do(upReq)
		if err != nil {
			lastErr = err
			h.pool.Failure(base)
			if !last {
				h.metrics.Retries.WithLabelValues("transport").Inc()
				h.logger.Warn("upstream failed, trying next",
					"upstream", base.String(), "error", err)
			}
			continue
		}
		h.metrics.UpstreamRequests.WithLabelValues(base.String(), strconv.Itoa(resp.StatusCode)).Inc()

		if reason := h.retryReason(base, resp.StatusCode, model, r.Method); reason != "" && !last {
			if reason == "status_5xx" {
				h.pool.Failure(base)
			}
			h.metrics.Retries.WithLabelValues(reason).Inc()
			h.logger.Warn("upstream response not usable, trying next",
				"upstream", base.String(), "status", resp.StatusCode, "reason", reason)
			drain(resp)
			lastErr = fmt.Errorf("upstream %s: %s", base, resp.Status)
			continue
		}

		h.pool.Success(base)
		return resp, nil
	}
	return nil, lastErr
}

// buildUpstreamRequest clones the client request onto one upstream base URL.
func (h *Handler) buildUpstreamRequest(r *http.Request, base *url.URL, endpoint string, body []byte) (*http.Request, error) {
	up := *base
	up.Path = strings.TrimRight(up.Path, "/") + endpoint
	up.RawQuery = r.URL.RawQuery

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, up.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vals := range r.Header {
		for _, v := range vals {
			upReq.Header.Add(k, v)
		}
	}
	// The body may have been rewritten (or simply re-read); let net/http set the
	// length rather than trusting the client's header.
	upReq.Header.Del("Content-Length")
	if upReq.Header.Get("Content-Type") == "" {
		upReq.Header.Set("Content-Type", "application/json")
	}
	return upReq, nil
}

// retryReason names why a response should be retried on another upstream, or
// returns "" to accept it. A 404 is only worth retrying when the pool knows the
// model lives somewhere else — otherwise every upstream would answer the same.
func (h *Handler) retryReason(base *url.URL, status int, model, method string) string {
	if h.retryStatus && status >= http.StatusInternalServerError {
		return "status_5xx"
	}
	if status == http.StatusNotFound && method == http.MethodPost &&
		h.pool.ModelElsewhere(base, model) {
		return "model_not_found"
	}
	return ""
}

// drain reads and closes a response being discarded on failover, so its
// connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
}

// injectStreamUsage sets stream_options.include_usage on an OpenAI-compatible
// streaming request. Without it the upstream omits the usage object entirely and
// the whole /v1 streaming surface records zero tokens. A body that is not a JSON
// object is passed through untouched.
func injectStreamUsage(body []byte) []byte {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	if _, ok := doc["stream"]; !ok {
		return body
	}

	opts := map[string]json.RawMessage{}
	if raw, ok := doc["stream_options"]; ok {
		if json.Unmarshal(raw, &opts) != nil {
			return body // caller sent something we should not rewrite
		}
		if _, set := opts["include_usage"]; set {
			return body // an explicit client choice wins
		}
	}
	opts["include_usage"] = json.RawMessage("true")

	encoded, err := json.Marshal(opts)
	if err != nil {
		return body
	}
	doc["stream_options"] = encoded

	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// serveNonStream reads the full upstream response, stores it in the cache when
// eligible, and hands it to completeNonStream.
func (h *Handler) serveNonStream(w http.ResponseWriter, resp *http.Response, r *http.Request, rc reqCtx) {
	respBuf, err := io.ReadAll(resp.Body)
	errMsg := ""
	if err != nil {
		errMsg = "read response: " + err.Error()
		h.logger.Error("reading non-stream response", "request_id", rc.reqID, "error", err)
	}

	if err == nil && rc.cacheKey != "" && cache.Cacheable(resp.StatusCode, resp.Header.Get("Content-Type")) {
		h.cache.Put(rc.cacheKey, &cache.Entry{
			Status: resp.StatusCode,
			Header: resp.Header,
			Body:   respBuf,
		})
	}

	h.completeNonStream(w, r, rc, resp.StatusCode, respBuf, errMsg, false)
}

// completeNonStream writes a complete response body to the client (its headers
// are already sent), extracts tokens and text from it, updates the metrics, and
// records the request. It serves both a freshly proxied response and one replayed
// from the cache.
func (h *Handler) completeNonStream(
	w http.ResponseWriter,
	r *http.Request,
	rc reqCtx,
	status int,
	respBuf []byte,
	errMsg string,
	cached bool,
) {
	_, _ = w.Write(respBuf)

	var promptTokens, completionTokens int64
	var respText string
	var hasTokens bool

	if rc.isOpenAI {
		var c openAIChunk
		if json.Unmarshal(respBuf, &c) == nil {
			respText = openAIText(c)
			if c.Usage != nil {
				promptTokens = c.Usage.PromptTokens
				completionTokens = c.Usage.CompletionTokens
				hasTokens = true
			}
		}
	} else {
		respText, promptTokens, completionTokens, hasTokens = parseNativeNonStream(respBuf)
	}

	if !hasTokens && !cached {
		h.logger.Warn("no token counts in non-stream response",
			"request_id", rc.reqID, "endpoint", rc.endpoint, "model", rc.model,
			"openai", rc.isOpenAI, "response_bytes", len(respBuf))
	}

	var estimated bool
	promptTokens, completionTokens, estimated = h.applyTokens(
		rc, hasTokens, cached, promptTokens, completionTokens, respText)

	duration := time.Since(rc.start)
	h.metrics.BytesOut.WithLabelValues(rc.endpoint, rc.model, rc.streamLabel).Add(float64(len(respBuf)))
	h.metrics.ReqTotal.WithLabelValues(rc.endpoint, rc.model, strconv.Itoa(status), rc.streamLabel).Inc()
	h.metrics.ReqDuration.WithLabelValues(rc.endpoint, rc.model, rc.streamLabel).Observe(duration.Seconds())

	cost := h.cost(rc.model, promptTokens, completionTokens, cached)
	h.persistAndLog(db.RequestRecord{
		RequestID:        rc.reqID,
		SessionID:        rc.sessionID,
		Timestamp:        rc.start,
		Endpoint:         rc.endpoint,
		Method:           rc.method,
		Model:            rc.model,
		Stream:           false,
		StatusCode:       status,
		DurationMS:       duration.Milliseconds(),
		RequestBytes:     rc.reqBytes,
		ResponseBytes:    int64(len(respBuf)),
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		Cost:             cost,
		ErrorMessage:     errMsg,
		ClientIP:         rc.clientIP,
		UserAgent:        r.UserAgent(),
		PromptText:       rc.promptText,
		ResponseText:     respText,
		TokensEstimated:  estimated,
		Cached:           cached,
	})
}

// applyTokens books the token counters for one finished request and fills in an
// estimate when the upstream reported nothing. A cache hit spent no tokens at
// all, so its counts are booked as savings instead.
func (h *Handler) applyTokens(
	rc reqCtx,
	hasTokens, cached bool,
	promptTokens, completionTokens int64,
	respText string,
) (int64, int64, bool) {
	if cached {
		saved := promptTokens + completionTokens
		if saved > 0 {
			h.metrics.CacheSavedTokens.WithLabelValues(rc.endpoint, rc.model).Add(float64(saved))
		}
		return promptTokens, completionTokens, false
	}

	estimated := false
	if !hasTokens && h.estimateTokens {
		promptTokens = tokens.Estimate(rc.promptText)
		completionTokens = tokens.Estimate(respText)
		estimated = promptTokens > 0 || completionTokens > 0
	}

	if estimated {
		if promptTokens > 0 {
			h.metrics.EstimatedTokens.WithLabelValues(rc.endpoint, rc.model, "prompt").Add(float64(promptTokens))
		}
		if completionTokens > 0 {
			h.metrics.EstimatedTokens.WithLabelValues(rc.endpoint, rc.model, "completion").Add(float64(completionTokens))
		}
		return promptTokens, completionTokens, true
	}

	if promptTokens > 0 {
		h.metrics.TokensIn.WithLabelValues(rc.endpoint, rc.model).Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		h.metrics.TokensOut.WithLabelValues(rc.endpoint, rc.model).Add(float64(completionTokens))
	}
	return promptTokens, completionTokens, false
}

// serveStream relays the upstream response line-by-line (preserving both NDJSON
// and SSE framing), forwarding each line immediately, while accumulating text,
// token counts, and time-to-first-token.
func (h *Handler) serveStream(w http.ResponseWriter, resp *http.Response, r *http.Request, rc reqCtx) {
	flusher, canFlush := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // up to 1 MB per line

	var totalBytes, promptTokens, completionTokens int64
	var respBuilder strings.Builder
	var firstTokenAt time.Time
	errMsg := ""

	for scanner.Scan() {
		line := scanner.Bytes()
		totalBytes += int64(len(line)) + 1 // +1 for the newline we re-add below

		_, writeErr := w.Write(line)
		_, _ = w.Write([]byte("\n"))
		if writeErr != nil {
			errMsg = "write to client: " + writeErr.Error()
			break
		}
		if canFlush {
			flusher.Flush()
		}

		text, lpt, lct, hasTok := parseStreamLine(rc.isOpenAI, line)
		if text != "" {
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
			respBuilder.WriteString(text)
		}
		if hasTok {
			if lpt > 0 {
				promptTokens = lpt
			}
			if lct > 0 {
				completionTokens = lct
			}
		}
	}
	if err := scanner.Err(); err != nil && errMsg == "" {
		errMsg = "scan stream: " + err.Error()
	}

	var ttft time.Duration
	if !firstTokenAt.IsZero() {
		ttft = firstTokenAt.Sub(rc.start)
		h.metrics.TTFT.WithLabelValues(rc.endpoint, rc.model).Observe(ttft.Seconds())
	}

	respText := respBuilder.String()
	hasTokens := promptTokens > 0 || completionTokens > 0
	promptTokens, completionTokens, estimated := h.applyTokens(
		rc, hasTokens, false, promptTokens, completionTokens, respText)

	duration := time.Since(rc.start)
	h.metrics.BytesOut.WithLabelValues(rc.endpoint, rc.model, rc.streamLabel).Add(float64(totalBytes))
	h.metrics.ReqTotal.WithLabelValues(rc.endpoint, rc.model, strconv.Itoa(resp.StatusCode), rc.streamLabel).Inc()
	h.metrics.ReqDuration.WithLabelValues(rc.endpoint, rc.model, rc.streamLabel).Observe(duration.Seconds())

	cost := h.cost(rc.model, promptTokens, completionTokens, false)
	h.persistAndLog(db.RequestRecord{
		RequestID:        rc.reqID,
		SessionID:        rc.sessionID,
		Timestamp:        rc.start,
		Endpoint:         rc.endpoint,
		Method:           rc.method,
		Model:            rc.model,
		Stream:           true,
		StatusCode:       resp.StatusCode,
		DurationMS:       duration.Milliseconds(),
		TTFTMs:           ttft.Milliseconds(),
		RequestBytes:     rc.reqBytes,
		ResponseBytes:    totalBytes,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		Cost:             cost,
		ErrorMessage:     errMsg,
		ClientIP:         rc.clientIP,
		UserAgent:        r.UserAgent(),
		PromptText:       rc.promptText,
		ResponseText:     respText,
		TokensEstimated:  estimated,
	})
}

// parseNativeNonStream extracts text and token counts from a native non-stream
// response, falling back to NDJSON scanning for Ollama versions that return
// newline-delimited chunks even when stream=false.
func parseNativeNonStream(body []byte) (text string, promptTokens, completionTokens int64, hasTokens bool) {
	var chunk ollamaChunk
	if json.Unmarshal(body, &chunk) == nil {
		text = responseText(chunk)
		if chunk.PromptEvalCount != nil {
			promptTokens = *chunk.PromptEvalCount
			hasTokens = true
		}
		if chunk.EvalCount != nil {
			completionTokens = *chunk.EvalCount
			hasTokens = true
		}
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c ollamaChunk
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue
		}
		text += responseText(c)
		if c.Done {
			if c.PromptEvalCount != nil {
				promptTokens = *c.PromptEvalCount
				hasTokens = true
			}
			if c.EvalCount != nil {
				completionTokens = *c.EvalCount
				hasTokens = true
			}
		}
	}
	return
}

// parseStreamLine extracts text and token counts from one streamed line, handling
// both native NDJSON and OpenAI SSE (`data: {...}`) framing.
func parseStreamLine(isOpenAI bool, line []byte) (text string, promptTokens, completionTokens int64, hasTokens bool) {
	if isOpenAI {
		payload, ok := sseData(line)
		if !ok {
			return
		}
		var c openAIChunk
		if json.Unmarshal(payload, &c) == nil {
			text = openAIText(c)
			if c.Usage != nil {
				promptTokens = c.Usage.PromptTokens
				completionTokens = c.Usage.CompletionTokens
				hasTokens = true
			}
		}
		return
	}
	var c ollamaChunk
	if json.Unmarshal(line, &c) == nil {
		text = responseText(c)
		if c.Done {
			if c.PromptEvalCount != nil {
				promptTokens = *c.PromptEvalCount
				hasTokens = true
			}
			if c.EvalCount != nil {
				completionTokens = *c.EvalCount
				hasTokens = true
			}
		}
	}
	return
}

// cost returns the estimated cost for the given model/token counts, or 0 when no
// pricing table is configured. A cache hit still carries the cost the client
// would have paid — the row is what the client consumed — but it is not added to
// the spend counter, which tracks money actually burned upstream.
func (h *Handler) cost(model string, promptTokens, completionTokens int64, cached bool) float64 {
	c := h.prices.Cost(model, promptTokens, completionTokens)
	if c > 0 && !cached {
		h.metrics.CostTotal.WithLabelValues(model).Add(c)
	}
	return c
}

// determineStream resolves whether the response will be streamed, matching the
// defaults of each API surface: native generate/chat default to streaming;
// embeddings never stream; OpenAI endpoints default to non-streaming; and any
// non-POST request (e.g. GET /api/tags, GET /v1/models) is treated as non-stream.
func determineStream(method, endpoint string, isOpenAI bool, streamFlag *bool) bool {
	if method != http.MethodPost {
		return false
	}
	if isEmbedEndpoint(endpoint) {
		return false // embeddings return a single JSON document, never a stream
	}
	if isOpenAI {
		return streamFlag != nil && *streamFlag
	}
	return streamFlag == nil || *streamFlag // native generate/chat default: true
}

func isEmbedEndpoint(endpoint string) bool {
	return strings.HasSuffix(endpoint, "/api/embed") ||
		strings.HasSuffix(endpoint, "/api/embeddings") ||
		strings.HasSuffix(endpoint, "/v1/embeddings")
}

// persistAndLog writes the record to SQLite, emits a structured log line, and
// publishes the record to any live (SSE) subscribers.
func (h *Handler) persistAndLog(rec db.RequestRecord) {
	if err := h.store.InsertRequest(rec); err != nil {
		h.logger.Error("failed to persist request record",
			"request_id", rec.RequestID, "error", err)
	}

	// A cache hit cost nothing upstream, so it is not charged to the budget.
	if !rec.Cached {
		h.budgets.Add(rec.SessionID, rec.TotalTokens, rec.Cost)
	}

	h.logger.Info("request",
		"request_id", rec.RequestID,
		"session_id", rec.SessionID,
		"endpoint", rec.Endpoint,
		"method", rec.Method,
		"model", rec.Model,
		"stream", rec.Stream,
		"status_code", rec.StatusCode,
		"duration_ms", rec.DurationMS,
		"ttft_ms", rec.TTFTMs,
		"request_bytes", rec.RequestBytes,
		"response_bytes", rec.ResponseBytes,
		"prompt_tokens", rec.PromptTokens,
		"completion_tokens", rec.CompletionTokens,
		"total_tokens", rec.TotalTokens,
		"cost", rec.Cost,
		"tokens_estimated", rec.TokensEstimated,
		"cached", rec.Cached,
		"client_ip", rec.ClientIP,
		"user_agent", rec.UserAgent,
		"error", rec.ErrorMessage,
	)

	h.publishEvent(rec)
}

// publishEvent fans the record out to live SSE subscribers (if a broker exists).
func (h *Handler) publishEvent(rec db.RequestRecord) {
	if h.events == nil {
		return
	}
	row := db.RequestRow{
		RequestID:        rec.RequestID,
		SessionID:        rec.SessionID,
		Timestamp:        rec.Timestamp,
		Endpoint:         rec.Endpoint,
		Method:           rec.Method,
		Model:            rec.Model,
		Stream:           rec.Stream,
		StatusCode:       rec.StatusCode,
		DurationMS:       rec.DurationMS,
		TTFTMs:           rec.TTFTMs,
		RequestBytes:     rec.RequestBytes,
		ResponseBytes:    rec.ResponseBytes,
		PromptTokens:     rec.PromptTokens,
		CompletionTokens: rec.CompletionTokens,
		TotalTokens:      rec.TotalTokens,
		Cost:             rec.Cost,
		ErrorMessage:     rec.ErrorMessage,
		ClientIP:         rec.ClientIP,
		UserAgent:        rec.UserAgent,
		PromptText:       rec.PromptText,
		ResponseText:     rec.ResponseText,
		TokensEstimated:  rec.TokensEstimated,
		Cached:           rec.Cached,
	}
	if b, err := json.Marshal(row); err == nil {
		h.events.Publish(b)
	}
}

// recordError is a convenience helper for early-exit error paths.
func (h *Handler) recordError(
	reqID, sessionID, endpoint string,
	r *http.Request,
	start time.Time,
	clientIP string,
	statusCode int,
	reqBytes, respBytes int64,
	errMsg string,
) {
	duration := time.Since(start)
	h.persistAndLog(db.RequestRecord{
		RequestID:     reqID,
		SessionID:     sessionID,
		Timestamp:     start,
		Endpoint:      endpoint,
		Method:        r.Method,
		Model:         modelname.Unknown,
		Stream:        false,
		StatusCode:    statusCode,
		DurationMS:    duration.Milliseconds(),
		RequestBytes:  reqBytes,
		ResponseBytes: respBytes,
		ErrorMessage:  errMsg,
		ClientIP:      clientIP,
		UserAgent:     r.UserAgent(),
	})
}

// extractSessionID returns the X-Session-ID header value, falling back to the
// client IP address so that requests from the same host are grouped together.
func extractSessionID(r *http.Request) string {
	if sid := r.Header.Get("X-Session-ID"); sid != "" {
		return sid
	}
	return extractClientIP(r)
}

// extractClientIP strips the port from RemoteAddr and prefers X-Forwarded-For
// when the request passes through a reverse proxy.
func extractClientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// May be a comma-separated list; take the first (original client).
		parts := strings.SplitN(fwd, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// newRequestID returns a random 32-char hex string suitable for use as a
// request identifier.
func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

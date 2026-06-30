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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
	"github.com/nexusriot/ollama-proxy-metrics/internal/events"
	"github.com/nexusriot/ollama-proxy-metrics/internal/pricing"
	"github.com/nexusriot/ollama-proxy-metrics/internal/ratelimit"
)

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
	}
	reg.MustRegister(
		m.ReqTotal, m.ReqDuration, m.TTFT, m.BytesIn, m.BytesOut,
		m.TokensIn, m.TokensOut, m.CostTotal, m.RateLimited,
	)
	return m
}

// Handler is the proxy HTTP handler.
type Handler struct {
	upstreams  []*url.URL
	rr         atomic.Uint64 // round-robin cursor
	httpClient *http.Client
	store      *db.Store
	logger     *slog.Logger
	metrics    *Metrics
	prices     *pricing.Table
	events     *events.Broker
	limiter    *ratelimit.Limiter
}

// New creates a new proxy Handler. upstreams must contain at least one URL;
// requests are round-robined across them with failover on transport errors.
// prices, broker and limiter may be nil (cost 0, no live events, no limiting).
func New(
	upstreams []*url.URL,
	store *db.Store,
	logger *slog.Logger,
	metrics *Metrics,
	prices *pricing.Table,
	broker *events.Broker,
	limiter *ratelimit.Limiter,
) *Handler {
	return &Handler{
		upstreams: upstreams,
		httpClient: &http.Client{
			// No overall timeout – long/streaming requests need an open connection.
			Timeout: 0,
		},
		store:   store,
		logger:  logger,
		metrics: metrics,
		prices:  prices,
		events:  broker,
		limiter: limiter,
	}
}

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
}

// ServeHTTP implements http.Handler; proxies /api/* and /v1/* to an upstream.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	sessionID := extractSessionID(r)
	clientIP := extractClientIP(r)
	endpoint := r.URL.Path

	// Per-session rate limiting (optional).
	if h.limiter.Enabled() && !h.limiter.Allow(sessionID) {
		h.metrics.RateLimited.Inc()
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limit exceeded for session", http.StatusTooManyRequests)
		h.recordError(reqID, sessionID, endpoint, r, start, clientIP,
			http.StatusTooManyRequests, 0, 0, "rate limit exceeded")
		return
	}

	var bodyBuf []byte
	if r.Body != nil {
		defer r.Body.Close()
		var err error
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			h.recordError(reqID, sessionID, endpoint, r, start, clientIP,
				http.StatusBadRequest, int64(len(bodyBuf)), 0, "read body: "+err.Error())
			return
		}
	}

	var payload requestPayload
	_ = json.Unmarshal(bodyBuf, &payload) // best-effort

	isOpenAI := strings.HasPrefix(endpoint, "/v1/")
	promptText := extractPromptText(payload)
	model := payload.Model
	if model == "" {
		model = "unknown"
	}
	stream := determineStream(r.Method, endpoint, isOpenAI, payload.Stream)
	streamLabel := strconv.FormatBool(stream)

	h.metrics.BytesIn.WithLabelValues(endpoint, model, streamLabel).Add(float64(len(bodyBuf)))

	resp, err := h.forward(r, endpoint, bodyBuf)
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
	w.WriteHeader(resp.StatusCode)

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

	if stream {
		h.serveStream(w, resp, r, rc)
	} else {
		h.serveNonStream(w, resp, r, rc)
	}
}

// forward sends the buffered request to an upstream, round-robining across the
// configured upstreams and failing over to the next on a transport error.
func (h *Handler) forward(r *http.Request, endpoint string, body []byte) (*http.Response, error) {
	n := len(h.upstreams)
	start := int(h.rr.Add(1)-1) % n
	var lastErr error
	for i := 0; i < n; i++ {
		up := *h.upstreams[(start+i)%n]
		up.Path = strings.TrimRight(up.Path, "/") + endpoint
		up.RawQuery = r.URL.RawQuery

		upReq, err := http.NewRequestWithContext(r.Context(), r.Method, up.String(), bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		for k, vals := range r.Header {
			for _, v := range vals {
				upReq.Header.Add(k, v)
			}
		}
		if upReq.Header.Get("Content-Type") == "" {
			upReq.Header.Set("Content-Type", "application/json")
		}

		resp, err := h.httpClient.Do(upReq)
		if err != nil {
			lastErr = err
			if n > 1 {
				h.logger.Warn("upstream failed, trying next",
					"upstream", h.upstreams[(start+i)%n].String(), "error", err)
			}
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// serveNonStream reads the full upstream response, extracts tokens/text, writes
// it back unchanged, and records the request.
func (h *Handler) serveNonStream(w http.ResponseWriter, resp *http.Response, r *http.Request, rc reqCtx) {
	respBuf, err := io.ReadAll(resp.Body)
	errMsg := ""
	if err != nil {
		errMsg = "read response: " + err.Error()
		h.logger.Error("reading non-stream response", "request_id", rc.reqID, "error", err)
	}

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

	if !hasTokens {
		h.logger.Warn("no token counts in non-stream response",
			"request_id", rc.reqID, "endpoint", rc.endpoint, "model", rc.model,
			"openai", rc.isOpenAI, "response_bytes", len(respBuf))
	}
	if promptTokens > 0 {
		h.metrics.TokensIn.WithLabelValues(rc.endpoint, rc.model).Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		h.metrics.TokensOut.WithLabelValues(rc.endpoint, rc.model).Add(float64(completionTokens))
	}

	_, _ = w.Write(respBuf)

	duration := time.Since(rc.start)
	h.metrics.BytesOut.WithLabelValues(rc.endpoint, rc.model, rc.streamLabel).Add(float64(len(respBuf)))
	h.metrics.ReqTotal.WithLabelValues(rc.endpoint, rc.model, strconv.Itoa(resp.StatusCode), rc.streamLabel).Inc()
	h.metrics.ReqDuration.WithLabelValues(rc.endpoint, rc.model, rc.streamLabel).Observe(duration.Seconds())

	cost := h.cost(rc.model, promptTokens, completionTokens)
	h.persistAndLog(db.RequestRecord{
		RequestID:        rc.reqID,
		SessionID:        rc.sessionID,
		Timestamp:        rc.start,
		Endpoint:         rc.endpoint,
		Method:           rc.method,
		Model:            rc.model,
		Stream:           false,
		StatusCode:       resp.StatusCode,
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
	})
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
	if promptTokens > 0 {
		h.metrics.TokensIn.WithLabelValues(rc.endpoint, rc.model).Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		h.metrics.TokensOut.WithLabelValues(rc.endpoint, rc.model).Add(float64(completionTokens))
	}

	duration := time.Since(rc.start)
	h.metrics.BytesOut.WithLabelValues(rc.endpoint, rc.model, rc.streamLabel).Add(float64(totalBytes))
	h.metrics.ReqTotal.WithLabelValues(rc.endpoint, rc.model, strconv.Itoa(resp.StatusCode), rc.streamLabel).Inc()
	h.metrics.ReqDuration.WithLabelValues(rc.endpoint, rc.model, rc.streamLabel).Observe(duration.Seconds())

	cost := h.cost(rc.model, promptTokens, completionTokens)
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
		ResponseText:     respBuilder.String(),
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
// pricing table is configured.
func (h *Handler) cost(model string, promptTokens, completionTokens int64) float64 {
	c := h.prices.Cost(model, promptTokens, completionTokens)
	if c > 0 {
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
		Model:         "unknown",
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

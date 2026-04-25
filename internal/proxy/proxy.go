// Package proxy implements the Ollama reverse-proxy with Prometheus metrics,
// structured request logging, and per-request SQLite persistence.
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
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
)

// requestPayload is the minimal incoming JSON shape we care about.
type requestPayload struct {
	Model    string          `json:"model"`
	Stream   *bool           `json:"stream,omitempty"`
	Prompt   string          `json:"prompt,omitempty"`   // /api/generate
	Messages []chatMessage   `json:"messages,omitempty"` // /api/chat
	Input    json.RawMessage `json:"input,omitempty"`    // /api/embed: string or []string
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ollamaChunk covers both final non-stream responses and every streaming chunk.
type ollamaChunk struct {
	Done            bool         `json:"done"`
	Response        string       `json:"response,omitempty"`  // /api/generate
	Message         *chatMessage `json:"message,omitempty"`   // /api/chat
	EvalCount       *int64       `json:"eval_count,omitempty"`
	PromptEvalCount *int64       `json:"prompt_eval_count,omitempty"`
}

// extractPromptText returns the user-facing prompt from the parsed request.
// For /api/generate it uses the "prompt" field; for /api/chat it uses the
// content of the last message with role "user"; for /api/embed it uses "input".
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

// responseText returns the assistant's text from a parsed Ollama chunk/response.
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
	BytesIn     *prometheus.CounterVec
	BytesOut    *prometheus.CounterVec
	TokensIn    *prometheus.CounterVec
	TokensOut   *prometheus.CounterVec
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
			Help: "Total prompt tokens (from Ollama eval stats).",
		}, []string{"endpoint", "model"}),

		TokensOut: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ollama_proxy_completion_tokens_total",
			Help: "Total completion tokens (from Ollama eval stats).",
		}, []string{"endpoint", "model"}),
	}
	reg.MustRegister(m.ReqTotal, m.ReqDuration, m.BytesIn, m.BytesOut, m.TokensIn, m.TokensOut)
	return m
}

// Handler is the proxy HTTP handler.
type Handler struct {
	upstream   *url.URL
	httpClient *http.Client
	store      *db.Store
	logger     *slog.Logger
	metrics    *Metrics
}

// New creates a new proxy Handler.
func New(upstream *url.URL, store *db.Store, logger *slog.Logger, metrics *Metrics) *Handler {
	return &Handler{
		upstream: upstream,
		httpClient: &http.Client{
			// No overall timeout – long/streaming requests need an open connection.
			Timeout: 0,
		},
		store:   store,
		logger:  logger,
		metrics: metrics,
	}
}

// ServeHTTP implements http.Handler; proxies /api/* to the upstream Ollama.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	sessionID := extractSessionID(r)
	clientIP := extractClientIP(r)
	endpoint := r.URL.Path

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

	promptText := extractPromptText(payload)
	model := payload.Model
	if model == "" {
		model = "unknown"
	}
	// /api/embed and /api/embeddings never stream; default to false for those.
	isEmbedEndpoint := strings.HasSuffix(endpoint, "/api/embed") || strings.HasSuffix(endpoint, "/api/embeddings")
	var stream bool
	if isEmbedEndpoint {
		stream = payload.Stream != nil && *payload.Stream
	} else {
		stream = payload.Stream == nil || *payload.Stream // default: true
	}
	streamLabel := strconv.FormatBool(stream)

	h.metrics.BytesIn.WithLabelValues(endpoint, model, streamLabel).Add(float64(len(bodyBuf)))

	up := *h.upstream
	up.Path = strings.TrimRight(up.Path, "/") + endpoint
	up.RawQuery = r.URL.RawQuery

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, up.String(), bytes.NewReader(bodyBuf))
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		h.recordError(reqID, sessionID, endpoint, r, start, clientIP,
			http.StatusInternalServerError, int64(len(bodyBuf)), 0, "create upstream req: "+err.Error())
		return
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

	statusLabel := strconv.Itoa(resp.StatusCode)

	if !stream {
		respBuf, err := io.ReadAll(resp.Body)
		errMsg := ""
		if err != nil {
			errMsg = "read response: " + err.Error()
			h.logger.Error("reading non-stream response", "request_id", reqID, "error", err)
		}

		var promptTokens, completionTokens int64
		var respText string
		var chunk ollamaChunk
		if json.Unmarshal(respBuf, &chunk) == nil {
			respText = responseText(chunk)
			if chunk.PromptEvalCount != nil {
				promptTokens = *chunk.PromptEvalCount
				h.metrics.TokensIn.WithLabelValues(endpoint, model).Add(float64(promptTokens))
			}
			if chunk.EvalCount != nil {
				completionTokens = *chunk.EvalCount
				h.metrics.TokensOut.WithLabelValues(endpoint, model).Add(float64(completionTokens))
			}
			if chunk.Done && chunk.PromptEvalCount == nil && chunk.EvalCount == nil {
				h.logger.Warn("no token counts in response",
					"request_id", reqID, "endpoint", endpoint, "model", model)
			}
		} else {
			// Fallback: some Ollama versions return NDJSON even for stream=false.
			sc := bufio.NewScanner(bytes.NewReader(respBuf))
			var sawPrompt, sawCompletion bool
			for sc.Scan() {
				var c ollamaChunk
				if json.Unmarshal(sc.Bytes(), &c) != nil {
					continue
				}
				respText += responseText(c)
				if c.Done {
					if c.PromptEvalCount != nil {
						promptTokens = *c.PromptEvalCount
						sawPrompt = true
					}
					if c.EvalCount != nil {
						completionTokens = *c.EvalCount
						sawCompletion = true
					}
				}
			}
			if sawPrompt {
				h.metrics.TokensIn.WithLabelValues(endpoint, model).Add(float64(promptTokens))
			}
			if sawCompletion {
				h.metrics.TokensOut.WithLabelValues(endpoint, model).Add(float64(completionTokens))
			}
			if !sawPrompt && !sawCompletion {
				h.logger.Warn("could not extract token counts from non-stream response",
					"request_id", reqID, "endpoint", endpoint, "model", model, "response_bytes", len(respBuf))
			}
		}

		_, _ = w.Write(respBuf)

		duration := time.Since(start)
		h.metrics.BytesOut.WithLabelValues(endpoint, model, streamLabel).Add(float64(len(respBuf)))
		h.metrics.ReqTotal.WithLabelValues(endpoint, model, statusLabel, streamLabel).Inc()
		h.metrics.ReqDuration.WithLabelValues(endpoint, model, streamLabel).Observe(duration.Seconds())

		rec := db.RequestRecord{
			RequestID:        reqID,
			SessionID:        sessionID,
			Timestamp:        start,
			Endpoint:         endpoint,
			Method:           r.Method,
			Model:            model,
			Stream:           false,
			StatusCode:       resp.StatusCode,
			DurationMS:       duration.Milliseconds(),
			RequestBytes:     int64(len(bodyBuf)),
			ResponseBytes:    int64(len(respBuf)),
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
			ErrorMessage:     errMsg,
			ClientIP:         clientIP,
			UserAgent:        r.UserAgent(),
			PromptText:       promptText,
			ResponseText:     respText,
		}
		h.persistAndLog(rec)
		return
	}

	// Read line-by-line so we can:
	//  - forward each chunk to the client immediately (true streaming), and
	//  - extract token counts from the final chunk (done=true).
	flusher, canFlush := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // up to 1 MB per line

	var totalBytes int64
	var promptTokens, completionTokens int64
	var respBuilder strings.Builder
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

		// Accumulate response text and token counts from every chunk.
		var chunk ollamaChunk
		if json.Unmarshal(line, &chunk) == nil {
			respBuilder.WriteString(responseText(chunk))
			if chunk.Done {
				if chunk.PromptEvalCount != nil {
					promptTokens = *chunk.PromptEvalCount
				}
				if chunk.EvalCount != nil {
					completionTokens = *chunk.EvalCount
				}
			}
		}
	}
	if err := scanner.Err(); err != nil && errMsg == "" {
		errMsg = "scan stream: " + err.Error()
	}

	if promptTokens > 0 {
		h.metrics.TokensIn.WithLabelValues(endpoint, model).Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		h.metrics.TokensOut.WithLabelValues(endpoint, model).Add(float64(completionTokens))
	}

	duration := time.Since(start)
	h.metrics.BytesOut.WithLabelValues(endpoint, model, streamLabel).Add(float64(totalBytes))
	h.metrics.ReqTotal.WithLabelValues(endpoint, model, statusLabel, streamLabel).Inc()
	h.metrics.ReqDuration.WithLabelValues(endpoint, model, streamLabel).Observe(duration.Seconds())

	rec := db.RequestRecord{
		RequestID:        reqID,
		SessionID:        sessionID,
		Timestamp:        start,
		Endpoint:         endpoint,
		Method:           r.Method,
		Model:            model,
		Stream:           true,
		StatusCode:       resp.StatusCode,
		DurationMS:       duration.Milliseconds(),
		RequestBytes:     int64(len(bodyBuf)),
		ResponseBytes:    totalBytes,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		ErrorMessage:     errMsg,
		ClientIP:         clientIP,
		UserAgent:        r.UserAgent(),
		PromptText:       promptText,
		ResponseText:     respBuilder.String(),
	}
	h.persistAndLog(rec)
}

// persistAndLog writes the record to SQLite and emits a structured log line.
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
		"request_bytes", rec.RequestBytes,
		"response_bytes", rec.ResponseBytes,
		"prompt_tokens", rec.PromptTokens,
		"completion_tokens", rec.CompletionTokens,
		"total_tokens", rec.TotalTokens,
		"client_ip", rec.ClientIP,
		"user_agent", rec.UserAgent,
		"error", rec.ErrorMessage,
	)
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
	rec := db.RequestRecord{
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
	}
	h.persistAndLog(rec)
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

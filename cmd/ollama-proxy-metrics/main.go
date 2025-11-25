package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// RequestPayload is the minimal shape we care about from the incoming request.
type RequestPayload struct {
	Model  string `json:"model"`
	Stream *bool  `json:"stream,omitempty"`
}

// OllamaResponseMetrics is the minimal subset of fields with token stats.
// These exist when stream:false is used.
type OllamaResponseMetrics struct {
	EvalCount       *int64 `json:"eval_count,omitempty"`
	PromptEvalCount *int64 `json:"prompt_eval_count,omitempty"`
}

var (
	upstreamURL *url.URL
	httpClient  = &http.Client{
		// No overall timeout: needed for long/streaming responses.
		Timeout: 0,
	}

	reqTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ollama_proxy_requests_total",
			Help: "Total number of requests handled by the Ollama proxy",
		},
		[]string{"endpoint", "model", "status", "stream"},
	)

	reqDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ollama_proxy_request_duration_seconds",
			Help:    "Duration of Ollama requests handled by the proxy",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint", "model", "stream"},
	)

	bytesIn = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ollama_proxy_request_bytes_in_total",
			Help: "Total number of bytes received in request bodies",
		},
		[]string{"endpoint", "model", "stream"},
	)

	bytesOut = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ollama_proxy_response_bytes_out_total",
			Help: "Total number of bytes sent in response bodies",
		},
		[]string{"endpoint", "model", "stream"},
	)

	tokensIn = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ollama_proxy_prompt_tokens_total",
			Help: "Total number of prompt tokens (from Ollama eval stats, stream=false only)",
		},
		[]string{"endpoint", "model"},
	)

	tokensOut = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ollama_proxy_completion_tokens_total",
			Help: "Total number of completion tokens (from Ollama eval stats, stream=false only)",
		},
		[]string{"endpoint", "model"},
	)
)

func init() {
	// Register metrics
	prometheus.MustRegister(reqTotal, reqDuration, bytesIn, bytesOut, tokensIn, tokensOut)
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func main() {
	var listenAddr string
	var upstream string

	flag.StringVar(&listenAddr, "listen", ":8080", "listen address for proxy (e.g. :8080)")
	flag.StringVar(&upstream, "upstream",
		getEnv("OLLAMA_UPSTREAM", "http://127.0.0.1:11434"),
		"Ollama upstream base URL (or set OLLAMA_UPSTREAM)",
	)
	flag.Parse()

	var err error
	upstreamURL, err = url.Parse(upstream)
	if err != nil {
		log.Fatalf("invalid upstream URL %q: %v", upstream, err)
	}

	// HTTP handlers
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/api/", proxyHandler)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Ollama metrics proxy\n\nUse /api/* for Ollama endpoints and /metrics for Prometheus metrics.\n"))
	})

	log.Printf("Starting Ollama proxy on %s, upstream %s", listenAddr, upstreamURL.String())
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// proxyHandler proxies /api/* requests to the upstream Ollama and records metrics.
func proxyHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	endpoint := r.URL.Path

	// Read entire request body (we need it to forward + parse model/stream).
	var bodyBuf []byte
	if r.Body != nil {
		defer r.Body.Close()
		var err error
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
	}

	// Try to parse model + stream flag from JSON.
	payload := RequestPayload{}
	_ = json.Unmarshal(bodyBuf, &payload) // ignore error, best-effort

	model := payload.Model
	if model == "" {
		model = "unknown"
	}

	stream := true
	if payload.Stream != nil {
		stream = *payload.Stream
	}
	streamLabel := "true"
	if !stream {
		streamLabel = "false"
	}

	bytesIn.WithLabelValues(endpoint, model, streamLabel).Add(float64(len(bodyBuf)))

	// Build upstream URL (base + path + query).
	up := *upstreamURL // copy
	up.Path = strings.TrimRight(up.Path, "/") + endpoint
	up.RawQuery = r.URL.RawQuery

	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, up.String(), bytes.NewReader(bodyBuf))
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}

	// Copy headers.
	for k, vals := range r.Header {
		for _, v := range vals {
			upReq.Header.Add(k, v)
		}
	}

	if upReq.Header.Get("Content-Type") == "" {
		upReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(upReq)
	if err != nil {
		log.Printf("upstream error: %v", err)
		statusCode := http.StatusBadGateway
		statusLabel := strconv.Itoa(statusCode)

		reqTotal.WithLabelValues(endpoint, model, statusLabel, streamLabel).Inc()
		reqDuration.WithLabelValues(endpoint, model, streamLabel).Observe(time.Since(start).Seconds())

		http.Error(w, "upstream error", statusCode)
		return
	}
	defer resp.Body.Close()

	statusLabel := strconv.Itoa(resp.StatusCode)

	// Copy upstream headers to client (simple version, no hop-by-hop stripping).
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if !stream {
		// Non-streaming: buffer entire response body, parse metrics, then write out.
		respBuf, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("failed to read upstream response body: %v", err)
			return
		}

		bytesOut.WithLabelValues(endpoint, model, streamLabel).Add(float64(len(respBuf)))

		// Try to parse token metrics.
		var m OllamaResponseMetrics
		if err := json.Unmarshal(respBuf, &m); err == nil {
			if m.PromptEvalCount != nil {
				tokensIn.WithLabelValues(endpoint, model).Add(float64(*m.PromptEvalCount))
			}
			if m.EvalCount != nil {
				tokensOut.WithLabelValues(endpoint, model).Add(float64(*m.EvalCount))
			}
		}

		if _, err := w.Write(respBuf); err != nil {
			log.Printf("failed to write response body to client: %v", err)
		}

		// Timing after full body processed.
		duration := time.Since(start).Seconds()
		reqTotal.WithLabelValues(endpoint, model, statusLabel, streamLabel).Inc()
		reqDuration.WithLabelValues(endpoint, model, streamLabel).Observe(duration)

		return
	}

	// Streaming mode: just pipe bytes through, we only track duration and size (no token stats).
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		log.Printf("streaming response to client failed: %v", err)
	}

	bytesOut.WithLabelValues(endpoint, model, streamLabel).Add(float64(n))

	duration := time.Since(start).Seconds()
	reqTotal.WithLabelValues(endpoint, model, statusLabel, streamLabel).Inc()
	reqDuration.WithLabelValues(endpoint, model, streamLabel).Observe(duration)
}

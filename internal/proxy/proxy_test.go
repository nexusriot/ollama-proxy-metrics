package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
	"github.com/nexusriot/ollama-proxy-metrics/internal/events"
	"github.com/nexusriot/ollama-proxy-metrics/internal/pricing"
	"github.com/nexusriot/ollama-proxy-metrics/internal/ratelimit"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse url %q: %v", s, err)
	}
	return u
}

// newHandlerOpts builds a Handler from opts, filling in the store, logger and
// metrics registry every test needs. It is the entry point for tests that
// exercise an optional subsystem (cache, gate, budgets, routing).
func newHandlerOpts(t *testing.T, opts Options) *Handler {
	t.Helper()
	if opts.Store == nil {
		opts.Store = openTestDB(t)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Metrics == nil {
		opts.Metrics = NewMetrics(prometheus.NewRegistry())
	}
	return New(opts)
}

// newHandler builds a Handler with explicit upstreams/pricing/limiter/broker.
func newHandler(t *testing.T, upstreams []*url.URL, prices *pricing.Table, limiter *ratelimit.Limiter, broker *events.Broker) *Handler {
	t.Helper()
	return newHandlerOpts(t, Options{
		Upstreams: upstreams,
		Prices:    prices,
		Events:    broker,
		Limiter:   limiter,
	})
}

func openTestDB(t *testing.T) *db.Store {
	t.Helper()
	s, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestHandler(t *testing.T, upstreamURL string) *Handler {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	return newHandlerOpts(t, Options{Upstreams: []*url.URL{u}})
}

func TestServeHTTP_NonStream_ProxiesBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintln(w, `{"response":"hello","done":true,"eval_count":42,"prompt_eval_count":10}`)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL)
	body := `{"model":"llama3","prompt":"hi","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "hello") {
		t.Errorf("response body missing upstream content: %s", rr.Body.String())
	}
}

func TestServeHTTP_NonStream_TokensExtracted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, `{"response":"ok","done":true,"eval_count":99,"prompt_eval_count":7}`)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL)
	body := `{"model":"llama3","prompt":"test","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	rows, _, err := h.store.ListRequests(1, 0, "", "")
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("expected 1 persisted row")
	}
	row := rows[0]
	if row.PromptTokens != 7 {
		t.Errorf("expected prompt_tokens=7, got %d", row.PromptTokens)
	}
	if row.CompletionTokens != 99 {
		t.Errorf("expected completion_tokens=99, got %d", row.CompletionTokens)
	}
	if row.TotalTokens != 106 {
		t.Errorf("expected total_tokens=106, got %d", row.TotalTokens)
	}
}

func TestServeHTTP_NonStream_RecordsStatusCode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		strings.NewReader(`{"model":"m","stream":false}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
	rows, _, _ := h.store.ListRequests(1, 0, "", "")
	if len(rows) == 0 || rows[0].StatusCode != 404 {
		t.Errorf("expected status_code=404 in DB")
	}
}

func TestServeHTTP_Stream_ForwardsChunks(t *testing.T) {
	chunks := []map[string]interface{}{
		{"response": "hel", "done": false},
		{"response": "lo", "done": false},
		{"response": "", "done": true, "eval_count": 5, "prompt_eval_count": 3},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		for _, c := range chunks {
			enc, _ := json.Marshal(c)
			_, _ = fmt.Fprintf(w, "%s\n", enc)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		strings.NewReader(`{"model":"llama3","stream":true}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	scanner := bufio.NewScanner(strings.NewReader(rr.Body.String()))
	var count int
	for scanner.Scan() {
		count++
	}
	if count != 3 {
		t.Errorf("expected 3 chunks forwarded, got %d", count)
	}
}

func TestServeHTTP_Stream_TokensFromFinalChunk(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		lines := []string{
			`{"response":"tok1","done":false}`,
			`{"response":"tok2","done":false}`,
			`{"response":"","done":true,"eval_count":77,"prompt_eval_count":13}`,
		}
		for _, l := range lines {
			_, _ = fmt.Fprintf(w, "%s\n", l)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"llama3","stream":true}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	rows, _, _ := h.store.ListRequests(1, 0, "", "")
	if len(rows) == 0 {
		t.Fatal("expected persisted row")
	}
	r := rows[0]
	if r.PromptTokens != 13 {
		t.Errorf("expected prompt_tokens=13, got %d", r.PromptTokens)
	}
	if r.CompletionTokens != 77 {
		t.Errorf("expected completion_tokens=77, got %d", r.CompletionTokens)
	}
	if r.Stream != true {
		t.Error("expected stream=true in DB")
	}
}

func TestServeHTTP_UpstreamUnavailable_Returns502(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1") // nothing listening
	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		strings.NewReader(`{"model":"x","stream":false}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rr.Code)
	}
}

func TestExtractSessionID_UsesHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Session-ID", "my-session")
	if got := extractSessionID(req); got != "my-session" {
		t.Errorf("expected 'my-session', got %q", got)
	}
}

func TestExtractSessionID_FallsBackToIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.1:54321"
	sid := extractSessionID(req)
	if sid != "192.168.1.1" {
		t.Errorf("expected client IP fallback '192.168.1.1', got %q", sid)
	}
}

func TestExtractClientIP_XForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 172.16.0.1")
	if got := extractClientIP(req); got != "10.0.0.1" {
		t.Errorf("expected '10.0.0.1', got %q", got)
	}
}

func TestServeHTTP_NoModel_DefaultsToUnknown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, `{"done":true}`)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		strings.NewReader(`{"stream":false}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	rows, _, _ := h.store.ListRequests(1, 0, "", "")
	if len(rows) == 0 || rows[0].Model != "unknown" {
		t.Errorf("expected model='unknown', got %v", rows)
	}
}

func TestServeHTTP_PersistsRequestAndResponseBytes(t *testing.T) {
	respPayload := `{"response":"world","done":true}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, respPayload)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL)
	reqBody := `{"model":"test","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(reqBody))
	h.ServeHTTP(httptest.NewRecorder(), req)

	rows, _, _ := h.store.ListRequests(1, 0, "", "")
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	r := rows[0]
	if r.RequestBytes != int64(len(reqBody)) {
		t.Errorf("request_bytes mismatch: want %d got %d", len(reqBody), r.RequestBytes)
	}
	// response bytes should match (respPayload length)
	if r.ResponseBytes != int64(len(respPayload)) {
		t.Errorf("response_bytes mismatch: want %d got %d", len(respPayload), r.ResponseBytes)
	}
}

func TestServeHTTP_OpenAI_NonStream_Usage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"hi there"}}],"usage":{"prompt_tokens":5,"completion_tokens":8,"total_tokens":13}}`)
	}))
	defer upstream.Close()

	h := newHandler(t, []*url.URL{mustURL(t, upstream.URL)}, nil, nil, nil)
	// OpenAI default is non-streaming when "stream" is absent.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	rows, _, _ := h.store.ListRequests(1, 0, "", "")
	if len(rows) == 0 {
		t.Fatal("expected persisted row")
	}
	r := rows[0]
	if r.PromptTokens != 5 || r.CompletionTokens != 8 || r.TotalTokens != 13 {
		t.Errorf("openai usage not extracted: pt=%d ct=%d tt=%d", r.PromptTokens, r.CompletionTokens, r.TotalTokens)
	}
	if r.Stream {
		t.Error("expected stream=false for /v1 without stream flag")
	}
	if r.ResponseText != "hi there" {
		t.Errorf("expected response text 'hi there', got %q", r.ResponseText)
	}
}

func TestServeHTTP_OpenAI_Stream_SSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		events := []string{
			`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
			`data: {"choices":[{"delta":{"content":"lo"}}]}`,
			`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "%s\n\n", e) // SSE event terminator is a blank line
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	h := newHandler(t, []*url.URL{mustURL(t, upstream.URL)}, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// SSE framing (blank line between events) must be preserved to the client.
	if !strings.Contains(rr.Body.String(), "data: [DONE]") {
		t.Errorf("forwarded body missing [DONE]: %q", rr.Body.String())
	}

	rows, _, _ := h.store.ListRequests(1, 0, "", "")
	if len(rows) == 0 {
		t.Fatal("expected persisted row")
	}
	r := rows[0]
	if r.PromptTokens != 11 || r.CompletionTokens != 22 {
		t.Errorf("openai stream usage not extracted: pt=%d ct=%d", r.PromptTokens, r.CompletionTokens)
	}
	if r.ResponseText != "Hello" {
		t.Errorf("expected accumulated 'Hello', got %q", r.ResponseText)
	}
	if !r.Stream {
		t.Error("expected stream=true")
	}
}

func TestForward_FailoverToSecondUpstream(t *testing.T) {
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"response":"ok","done":true,"eval_count":1,"prompt_eval_count":1}`)
	}))
	defer alive.Close()

	// Dead upstream listed first; the proxy must fail over to the alive one.
	ups := []*url.URL{mustURL(t, "http://127.0.0.1:1"), mustURL(t, alive.URL)}
	h := newHandler(t, ups, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		strings.NewReader(`{"model":"m","stream":false}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected failover to succeed with 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "ok") {
		t.Errorf("expected response from alive upstream, got %q", rr.Body.String())
	}
}

func TestServeHTTP_RateLimit_Returns429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"response":"x","done":true}`)
	}))
	defer upstream.Close()

	limiter := ratelimit.New(1, time.Minute) // 1 request per session per minute
	h := newHandler(t, []*url.URL{mustURL(t, upstream.URL)}, nil, limiter, nil)

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/generate",
			strings.NewReader(`{"model":"m","stream":false}`))
		req.Header.Set("X-Session-ID", "same-session")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := do(); code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", code)
	}
	if code := do(); code != http.StatusTooManyRequests {
		t.Errorf("second request should be rate limited (429), got %d", code)
	}
}

func TestServeHTTP_Cost_Recorded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"response":"x","done":true,"eval_count":1000,"prompt_eval_count":1000}`)
	}))
	defer upstream.Close()

	prices := &pricing.Table{
		Currency: "USD",
		Models:   map[string]pricing.ModelRate{"gpt-4o": {PromptPer1K: 5, CompletionPer1K: 15}},
	}
	h := newHandler(t, []*url.URL{mustURL(t, upstream.URL)}, prices, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		strings.NewReader(`{"model":"gpt-4o","stream":false}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	rows, _, _ := h.store.ListRequests(1, 0, "", "")
	if len(rows) == 0 {
		t.Fatal("expected persisted row")
	}
	if got := rows[0].Cost; got != 20.0 { // 1000/1000*5 + 1000/1000*15
		t.Errorf("expected cost=20, got %v", got)
	}
}

func TestDetermineStream(t *testing.T) {
	b := func(v bool) *bool { return &v }
	cases := []struct {
		name     string
		method   string
		endpoint string
		openai   bool
		flag     *bool
		want     bool
	}{
		{"native generate default true", "POST", "/api/generate", false, nil, true},
		{"native generate explicit false", "POST", "/api/generate", false, b(false), false},
		{"native embed never streams", "POST", "/api/embed", false, nil, false},
		{"openai default false", "POST", "/v1/chat/completions", true, nil, false},
		{"openai explicit true", "POST", "/v1/chat/completions", true, b(true), true},
		{"openai embeddings never streams", "POST", "/v1/embeddings", true, b(true), false},
		{"GET never streams", "GET", "/api/tags", false, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := determineStream(c.method, c.endpoint, c.openai, c.flag); got != c.want {
				t.Errorf("determineStream=%v want %v", got, c.want)
			}
		})
	}
}

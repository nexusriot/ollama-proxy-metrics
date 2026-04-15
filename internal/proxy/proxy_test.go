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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
)

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
	store := openTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	return New(u, store, logger, metrics)
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

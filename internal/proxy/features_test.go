package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexusriot/ollama-proxy-metrics/internal/budget"
	"github.com/nexusriot/ollama-proxy-metrics/internal/cache"
	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
	"github.com/nexusriot/ollama-proxy-metrics/internal/gate"
	"github.com/nexusriot/ollama-proxy-metrics/internal/upstream"
)

// post drives one request through the handler and returns the recorder.
func post(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// lastRow returns the most recently persisted request row.
func lastRow(t *testing.T, h *Handler) db.RequestRow {
	t.Helper()
	rows, _, err := h.store.ListRequests(1, 0, "", "")
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no request row was persisted")
	}
	return rows[0]
}

// echoUpstream answers every request with a fixed native Ollama response.
func echoUpstream(t *testing.T, response string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, response)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServeHTTP_BodyOverLimit_Returns413(t *testing.T) {
	up := echoUpstream(t, `{"response":"ok","done":true,"eval_count":1,"prompt_eval_count":1}`)
	h := newHandlerOpts(t, Options{
		Upstreams:    []*url.URL{mustURL(t, up.URL)},
		MaxBodyBytes: 64,
	})

	big := fmt.Sprintf(`{"model":"m","stream":false,"prompt":%q}`, strings.Repeat("x", 500))
	rr := post(t, h, "/api/generate", big)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
	row := lastRow(t, h)
	if row.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("recorded status = %d, want 413", row.StatusCode)
	}
	if !strings.Contains(row.ErrorMessage, "exceeds") {
		t.Errorf("recorded error = %q, want it to mention the limit", row.ErrorMessage)
	}
}

func TestServeHTTP_BodyUnderLimit_PassesThrough(t *testing.T) {
	up := echoUpstream(t, `{"response":"ok","done":true,"eval_count":1,"prompt_eval_count":1}`)
	h := newHandlerOpts(t, Options{
		Upstreams:    []*url.URL{mustURL(t, up.URL)},
		MaxBodyBytes: 1 << 20,
	})

	if rr := post(t, h, "/api/generate", `{"model":"m","stream":false}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestServeHTTP_NormalizesModelName(t *testing.T) {
	up := echoUpstream(t, `{"response":"ok","done":true,"eval_count":1,"prompt_eval_count":1}`)
	h := newHandlerOpts(t, Options{
		Upstreams:       []*url.URL{mustURL(t, up.URL)},
		NormalizeModels: true,
	})

	post(t, h, "/api/generate", `{"model":"llama3","stream":false}`)
	if got := lastRow(t, h).Model; got != "llama3:latest" {
		t.Fatalf("recorded model = %q, want llama3:latest", got)
	}
}

func TestServeHTTP_LeavesModelNameAloneWhenNormalizationOff(t *testing.T) {
	up := echoUpstream(t, `{"response":"ok","done":true,"eval_count":1,"prompt_eval_count":1}`)
	h := newHandlerOpts(t, Options{Upstreams: []*url.URL{mustURL(t, up.URL)}})

	post(t, h, "/api/generate", `{"model":"llama3","stream":false}`)
	if got := lastRow(t, h).Model; got != "llama3" {
		t.Fatalf("recorded model = %q, want it unchanged", got)
	}
}

func TestServeHTTP_AppliesModelAlias(t *testing.T) {
	up := echoUpstream(t, `{"response":"ok","done":true,"eval_count":1,"prompt_eval_count":1}`)
	h := newHandlerOpts(t, Options{
		Upstreams:       []*url.URL{mustURL(t, up.URL)},
		NormalizeModels: true,
		ModelAliases:    map[string]string{"fast": "qwen2.5:0.5b"},
	})

	post(t, h, "/api/generate", `{"model":"fast","stream":false}`)
	if got := lastRow(t, h).Model; got != "qwen2.5:0.5b" {
		t.Fatalf("recorded model = %q, want the alias target", got)
	}
}

// bodyRecorder captures the last request body an upstream received.
type bodyRecorder struct {
	mu   sync.Mutex
	body string
}

func (b *bodyRecorder) set(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.body = s
}

func (b *bodyRecorder) get() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.body
}

// sseUpstream records the request body and answers with one SSE event.
func sseUpstream(t *testing.T) (*httptest.Server, *bodyRecorder) {
	t.Helper()
	rec := &bodyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		rec.set(string(buf))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestServeHTTP_InjectsStreamUsage(t *testing.T) {
	up, rec := sseUpstream(t)
	h := newHandlerOpts(t, Options{
		Upstreams:   []*url.URL{mustURL(t, up.URL)},
		InjectUsage: true,
	})

	post(t, h, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`)

	var sent map[string]any
	if err := json.Unmarshal([]byte(rec.get()), &sent); err != nil {
		t.Fatalf("upstream body was not JSON: %q", rec.get())
	}
	opts, ok := sent["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("upstream body = %v, want stream_options.include_usage=true", sent)
	}
}

func TestServeHTTP_DoesNotInjectUsageWhenDisabled(t *testing.T) {
	up, rec := sseUpstream(t)
	h := newHandlerOpts(t, Options{Upstreams: []*url.URL{mustURL(t, up.URL)}})

	post(t, h, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`)
	if strings.Contains(rec.get(), "stream_options") {
		t.Fatalf("upstream body = %q, want it untouched", rec.get())
	}
}

func TestInjectStreamUsage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool // whether include_usage should end up true
	}{
		{"adds to a streaming request", `{"model":"m","stream":true}`, true},
		{"keeps an explicit client choice", `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`, false},
		{"ignores a non-streaming request", `{"model":"m"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := injectStreamUsage([]byte(c.in))
			var doc map[string]any
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("result is not JSON: %q", out)
			}
			got := false
			if opts, ok := doc["stream_options"].(map[string]any); ok {
				got, _ = opts["include_usage"].(bool)
			}
			if got != c.want {
				t.Fatalf("include_usage = %v, want %v (body %s)", got, c.want, out)
			}
		})
	}
}

func TestInjectStreamUsage_LeavesNonJSONAlone(t *testing.T) {
	in := []byte("not json at all")
	if got := injectStreamUsage(in); string(got) != string(in) {
		t.Fatalf("injectStreamUsage rewrote a non-JSON body: %q", got)
	}
}

func TestServeHTTP_EstimatesTokensWhenUpstreamReportsNone(t *testing.T) {
	// A response with no eval counts at all — the case that used to record 0.
	up := echoUpstream(t, `{"response":"eight chars","done":true}`)
	h := newHandlerOpts(t, Options{
		Upstreams:      []*url.URL{mustURL(t, up.URL)},
		EstimateTokens: true,
	})

	post(t, h, "/api/generate", `{"model":"m","stream":false,"prompt":"four"}`)

	row := lastRow(t, h)
	if !row.TokensEstimated {
		t.Fatal("row is not flagged as estimated")
	}
	if row.PromptTokens == 0 || row.CompletionTokens == 0 {
		t.Fatalf("estimated tokens = %d/%d, want both non-zero", row.PromptTokens, row.CompletionTokens)
	}
	if row.TotalTokens != row.PromptTokens+row.CompletionTokens {
		t.Errorf("total = %d, want the sum of both estimates", row.TotalTokens)
	}
}

func TestServeHTTP_DoesNotEstimateWhenUpstreamReportsCounts(t *testing.T) {
	up := echoUpstream(t, `{"response":"x","done":true,"eval_count":9,"prompt_eval_count":3}`)
	h := newHandlerOpts(t, Options{
		Upstreams:      []*url.URL{mustURL(t, up.URL)},
		EstimateTokens: true,
	})

	post(t, h, "/api/generate", `{"model":"m","stream":false,"prompt":"a long prompt here"}`)

	row := lastRow(t, h)
	if row.TokensEstimated {
		t.Fatal("row flagged as estimated even though the upstream reported counts")
	}
	if row.PromptTokens != 3 || row.CompletionTokens != 9 {
		t.Fatalf("tokens = %d/%d, want the upstream's own counts", row.PromptTokens, row.CompletionTokens)
	}
}

func TestServeHTTP_NoEstimateWhenDisabled(t *testing.T) {
	up := echoUpstream(t, `{"response":"text","done":true}`)
	h := newHandlerOpts(t, Options{Upstreams: []*url.URL{mustURL(t, up.URL)}})

	post(t, h, "/api/generate", `{"model":"m","stream":false,"prompt":"hello"}`)

	row := lastRow(t, h)
	if row.TokensEstimated || row.TotalTokens != 0 {
		t.Fatalf("row = estimated:%v tokens:%d, want the untouched zero counts", row.TokensEstimated, row.TotalTokens)
	}
}

// countingUpstream answers with response and counts how often it was called.
func countingUpstream(t *testing.T, response string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, response)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestServeHTTP_CacheServesRepeatFromMemory(t *testing.T) {
	up, calls := countingUpstream(t, `{"response":"cached hello","done":true,"eval_count":5,"prompt_eval_count":2}`)
	h := newHandlerOpts(t, Options{
		Upstreams: []*url.URL{mustURL(t, up.URL)},
		Cache:     cache.New(time.Minute, 1<<20),
	})

	body := `{"model":"m","stream":false,"prompt":"same"}`
	first := post(t, h, "/api/generate", body)
	second := post(t, h, "/api/generate", body)

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times, want 1", got)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("cached body %q differs from the original %q", second.Body.String(), first.Body.String())
	}
	if got := first.Header().Get("X-Proxy-Cache"); got != "miss" {
		t.Errorf("first response X-Proxy-Cache = %q, want miss", got)
	}
	if got := second.Header().Get("X-Proxy-Cache"); got != "hit" {
		t.Errorf("second response X-Proxy-Cache = %q, want hit", got)
	}

	row := lastRow(t, h)
	if !row.Cached {
		t.Fatal("the replayed row is not flagged as cached")
	}
	if row.TotalTokens != 7 {
		t.Errorf("cached row tokens = %d, want the original 7", row.TotalTokens)
	}
}

func TestServeHTTP_CacheKeyedOnBody(t *testing.T) {
	up, calls := countingUpstream(t, `{"response":"x","done":true,"eval_count":1,"prompt_eval_count":1}`)
	h := newHandlerOpts(t, Options{
		Upstreams: []*url.URL{mustURL(t, up.URL)},
		Cache:     cache.New(time.Minute, 1<<20),
	})

	post(t, h, "/api/generate", `{"model":"m","stream":false,"prompt":"one"}`)
	post(t, h, "/api/generate", `{"model":"m","stream":false,"prompt":"two"}`)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream called %d times, want 2 for two different prompts", got)
	}
}

func TestServeHTTP_StreamingIsNeverCached(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, "{\"response\":\"hi\",\"done\":true,\"eval_count\":1,\"prompt_eval_count\":1}\n")
	}))
	defer up.Close()

	h := newHandlerOpts(t, Options{
		Upstreams: []*url.URL{mustURL(t, up.URL)},
		Cache:     cache.New(time.Minute, 1<<20),
	})

	body := `{"model":"m","stream":true,"prompt":"same"}`
	post(t, h, "/api/generate", body)
	post(t, h, "/api/generate", body)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream called %d times, want 2 — streams must not be cached", got)
	}
}

func TestServeHTTP_ErrorResponsesAreNotCached(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"nope"}`, http.StatusInternalServerError)
	}))
	defer up.Close()

	h := newHandlerOpts(t, Options{
		Upstreams: []*url.URL{mustURL(t, up.URL)},
		Cache:     cache.New(time.Minute, 1<<20),
	})

	body := `{"model":"m","stream":false}`
	post(t, h, "/api/generate", body)
	post(t, h, "/api/generate", body)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream called %d times, want 2 — errors must not be cached", got)
	}
}

func TestServeHTTP_QueueFullReturns503(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-hold
		_, _ = fmt.Fprint(w, `{"response":"x","done":true,"eval_count":1,"prompt_eval_count":1}`)
	}))
	defer up.Close()

	h := newHandlerOpts(t, Options{
		Upstreams: []*url.URL{mustURL(t, up.URL)},
		Gate:      gate.New(1, 1),
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); post(t, h, "/api/generate", `{"model":"m","stream":false}`) }()
	<-started
	go func() { defer wg.Done(); post(t, h, "/api/generate", `{"model":"m","stream":false}`) }()

	// Wait for the second request to occupy the single queue slot.
	deadline := time.Now().Add(2 * time.Second)
	for h.gate.Waiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	rr := post(t, h, "/api/generate", `{"model":"m","stream":false}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once the queue is full", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("503 response carries no Retry-After header")
	}

	close(hold)
	wg.Wait()

	rows, _, err := h.store.ListRequestsFiltered(10, 0, db.RequestFilter{Status: http.StatusServiceUnavailable})
	if err != nil {
		t.Fatalf("ListRequestsFiltered: %v", err)
	}
	if len(rows) != 1 || !strings.Contains(rows[0].ErrorMessage, "queue full") {
		t.Fatalf("recorded rejection rows = %+v", rows)
	}
}

func TestServeHTTP_GateSerializesRequests(t *testing.T) {
	var concurrent, peak atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := concurrent.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		concurrent.Add(-1)
		_, _ = fmt.Fprint(w, `{"response":"x","done":true,"eval_count":1,"prompt_eval_count":1}`)
	}))
	defer up.Close()

	h := newHandlerOpts(t, Options{
		Upstreams: []*url.URL{mustURL(t, up.URL)},
		Gate:      gate.New(1, 0),
	})

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, h, "/api/generate", `{"model":"m","stream":false}`)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > 1 {
		t.Fatalf("peak upstream concurrency = %d, want 1", got)
	}
}

func TestServeHTTP_BudgetExhaustedReturns429(t *testing.T) {
	up := echoUpstream(t, `{"response":"x","done":true,"eval_count":1,"prompt_eval_count":1}`)
	budgets := budget.New(budget.Limits{TokensPerDay: 10})
	budgets.Seed(budgets.Day(), map[string]budget.Usage{"spender": {Tokens: 50}})

	h := newHandlerOpts(t, Options{
		Upstreams: []*url.URL{mustURL(t, up.URL)},
		Budgets:   budgets,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		strings.NewReader(`{"model":"m","stream":false}`))
	req.Header.Set("X-Session-ID", "spender")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("budget rejection carries no Retry-After header")
	}
	row := lastRow(t, h)
	if !strings.Contains(row.ErrorMessage, "budget exceeded") {
		t.Errorf("recorded error = %q", row.ErrorMessage)
	}
}

func TestServeHTTP_BudgetAccruesFromCompletedRequests(t *testing.T) {
	up := echoUpstream(t, `{"response":"x","done":true,"eval_count":40,"prompt_eval_count":10}`)
	budgets := budget.New(budget.Limits{TokensPerDay: 30})
	h := newHandlerOpts(t, Options{
		Upstreams: []*url.URL{mustURL(t, up.URL)},
		Budgets:   budgets,
	})

	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/generate",
			strings.NewReader(`{"model":"m","stream":false}`))
		req.Header.Set("X-Session-ID", "sess")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := send(); code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", code)
	}
	// The first request spent 50 tokens against a 30-token ceiling.
	if code := send(); code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", code)
	}
}

func TestServeHTTP_CacheHitIsNotChargedToTheBudget(t *testing.T) {
	up := echoUpstream(t, `{"response":"x","done":true,"eval_count":40,"prompt_eval_count":10}`)
	budgets := budget.New(budget.Limits{TokensPerDay: 100})
	h := newHandlerOpts(t, Options{
		Upstreams: []*url.URL{mustURL(t, up.URL)},
		Budgets:   budgets,
		Cache:     cache.New(time.Minute, 1<<20),
	})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/generate",
			strings.NewReader(`{"model":"m","stream":false,"prompt":"same"}`))
		req.Header.Set("X-Session-ID", "sess")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	_, usage := budgets.Snapshot()
	if got := usage["sess"].Tokens; got != 50 {
		t.Fatalf("budget usage = %d tokens, want only the one uncached request's 50", got)
	}
}

func TestForward_RetriesOn5xx(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	good := echoUpstream(t, `{"response":"recovered","done":true,"eval_count":1,"prompt_eval_count":1}`)

	h := newHandlerOpts(t, Options{
		Upstreams:   []*url.URL{mustURL(t, failing.URL), mustURL(t, good.URL)},
		RetryStatus: true,
	})

	rr := post(t, h, "/api/generate", `{"model":"m","stream":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want the retry to succeed with 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "recovered") {
		t.Fatalf("body = %q, want the healthy upstream's response", rr.Body.String())
	}
}

func TestForward_DoesNotRetry5xxWhenDisabled(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	good := echoUpstream(t, `{"response":"recovered","done":true}`)

	h := newHandlerOpts(t, Options{
		Upstreams: []*url.URL{mustURL(t, failing.URL), mustURL(t, good.URL)},
	})

	rr := post(t, h, "/api/generate", `{"model":"m","stream":false}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the 5xx passed through untouched", rr.Code)
	}
}

// modelUpstream serves an /api/tags inventory plus a generate response naming itself.
func modelUpstream(t *testing.T, name string, models ...string) *httptest.Server {
	t.Helper()
	tags := `{"models":[`
	for i, m := range models {
		if i > 0 {
			tags += ","
		}
		tags += `{"name":"` + m + `","model":"` + m + `"}`
	}
	tags += `]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/tags" {
			_, _ = fmt.Fprint(w, tags)
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		for _, m := range models {
			if m == payload.Model {
				_, _ = fmt.Fprintf(w, `{"response":%q,"done":true,"eval_count":1,"prompt_eval_count":1}`, name)
				return
			}
		}
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestForward_RoutesToTheUpstreamServingTheModel(t *testing.T) {
	a := modelUpstream(t, "node-a", "llama3:latest")
	b := modelUpstream(t, "node-b", "qwen3:8b")

	pool := upstream.New(upstream.Options{
		URLs: []*url.URL{mustURL(t, a.URL), mustURL(t, b.URL)},
	})
	pool.Refresh(context.Background())

	h := newHandlerOpts(t, Options{Pool: pool, NormalizeModels: true})

	for i := 0; i < 4; i++ {
		rr := post(t, h, "/api/generate", `{"model":"qwen3:8b","stream":false}`)
		if !strings.Contains(rr.Body.String(), "node-b") {
			t.Fatalf("request %d went to %q, want the node serving qwen3:8b", i, rr.Body.String())
		}
	}
}

func TestForward_RetriesElsewhereOnModelNotFound(t *testing.T) {
	a := modelUpstream(t, "node-a", "llama3:latest")
	b := modelUpstream(t, "node-b", "qwen3:8b")

	urlA, urlB := mustURL(t, a.URL), mustURL(t, b.URL)
	pool := upstream.New(upstream.Options{
		URLs:             []*url.URL{urlA, urlB},
		FailureThreshold: 1,
		Cooldown:         time.Minute,
	})
	pool.Refresh(context.Background())

	// Trip node-b's circuit so routing has to try node-a first, even though
	// node-a is the one that cannot serve the model.
	pool.Failure(urlB)

	h := newHandlerOpts(t, Options{Pool: pool, NormalizeModels: true})

	rr := post(t, h, "/api/generate", `{"model":"qwen3:8b","stream":false}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "node-b") {
		t.Fatalf("status %d body %q, want the 404 retried onto node-b", rr.Code, rr.Body.String())
	}
}

func TestForward_KeepsA404NoUpstreamCanServe(t *testing.T) {
	a := modelUpstream(t, "node-a", "llama3:latest")
	b := modelUpstream(t, "node-b", "llama3:latest")

	pool := upstream.New(upstream.Options{
		URLs: []*url.URL{mustURL(t, a.URL), mustURL(t, b.URL)},
	})
	pool.Refresh(context.Background())

	h := newHandlerOpts(t, Options{Pool: pool, NormalizeModels: true})

	rr := post(t, h, "/api/generate", `{"model":"nobody-has-this","stream":false}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the 404 passed through", rr.Code)
	}
}

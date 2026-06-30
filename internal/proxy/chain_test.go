package proxy

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// These tests cover the proxy->proxy->LLM topology: a request flows
// client -> proxy A -> proxy B -> upstream. Because the proxy is transparent
// (path/query/headers/body/status and streaming framing are preserved), chaining
// works; these tests pin that behavior and the known attribution caveats.

// captured records what the real upstream observed, guarded for the race detector.
type captured struct {
	mu    sync.Mutex
	path  string
	query string
	sess  string
	xff   string
}

func (c *captured) set(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.Path
	c.query = r.URL.RawQuery
	c.sess = r.Header.Get("X-Session-ID")
	c.xff = r.Header.Get("X-Forwarded-For")
}

func (c *captured) get() (path, query, sess, xff string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.query, c.sess, c.xff
}

// newChain wires client -> A -> B -> fake upstream(handler) and returns both
// proxy handlers plus what the fake upstream saw. A is driven directly via
// ServeHTTP; B and the upstream run as real httptest servers so A's HTTP client
// actually crosses the network to B, and B's to the upstream.
func newChain(t *testing.T, upstream http.HandlerFunc) (a, b *Handler, cap *captured) {
	t.Helper()
	cap = &captured{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.set(r)
		upstream(w, r)
	}))
	t.Cleanup(fake.Close)

	b = newHandler(t, []*url.URL{mustURL(t, fake.URL)}, nil, nil, nil)
	bServer := httptest.NewServer(b)
	t.Cleanup(bServer.Close)

	a = newHandler(t, []*url.URL{mustURL(t, bServer.URL)}, nil, nil, nil)
	return a, b, cap
}

// bothProxies is a small helper to assert against A and B together.
func bothProxies(a, b *Handler) map[string]*Handler {
	return map[string]*Handler{"A": a, "B": b}
}

func TestChain_TransparentForwarding(t *testing.T) {
	a, b, cap := newChain(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"response":"chained hello","done":true,"eval_count":9,"prompt_eval_count":4}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/generate?foo=bar",
		strings.NewReader(`{"model":"llama3","prompt":"hi","stream":false}`))
	rr := httptest.NewRecorder()
	a.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("client got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "chained hello") {
		t.Errorf("client body missing upstream content: %q", rr.Body.String())
	}

	// Path and query survived both hops to reach the real upstream unchanged.
	path, query, _, _ := cap.get()
	if path != "/api/generate" {
		t.Errorf("upstream path=%q, want /api/generate", path)
	}
	if query != "foo=bar" {
		t.Errorf("upstream query=%q, want foo=bar", query)
	}

	// Both proxies independently recorded the request with identical token counts.
	for name, h := range bothProxies(a, b) {
		rows, _, err := h.store.ListRequests(1, 0, "", "")
		if err != nil {
			t.Fatalf("proxy %s ListRequests: %v", name, err)
		}
		if len(rows) != 1 {
			t.Fatalf("proxy %s recorded %d rows, want 1 (double recording is expected)", name, len(rows))
		}
		if rows[0].TotalTokens != 13 {
			t.Errorf("proxy %s total_tokens=%d, want 13", name, rows[0].TotalTokens)
		}
	}
}

func TestChain_SessionIDPropagates(t *testing.T) {
	a, b, cap := newChain(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"response":"x","done":true}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		strings.NewReader(`{"model":"m","stream":false}`))
	req.Header.Set("X-Session-ID", "alice")
	a.ServeHTTP(httptest.NewRecorder(), req)

	// The header rides through every hop to the real upstream...
	if _, _, sess, _ := cap.get(); sess != "alice" {
		t.Errorf("upstream saw X-Session-ID=%q, want alice", sess)
	}
	// ...and both proxies attribute the request to that session.
	for name, h := range bothProxies(a, b) {
		rows, _, _ := h.store.ListRequests(1, 0, "", "")
		if len(rows) == 0 || rows[0].SessionID != "alice" {
			t.Errorf("proxy %s session_id mismatch: %+v", name, rows)
		}
	}
}

func TestChain_ClientIPNotForwardedToInnerProxy(t *testing.T) {
	a, b, cap := newChain(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"response":"x","done":true}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		strings.NewReader(`{"model":"m","stream":false}`))
	req.RemoteAddr = "203.0.113.7:5555" // the original client
	// Deliberately no X-Session-ID and no X-Forwarded-For.
	a.ServeHTTP(httptest.NewRecorder(), req)

	aRows, _, _ := a.store.ListRequests(1, 0, "", "")
	bRows, _, _ := b.store.ListRequests(1, 0, "", "")
	if len(aRows) == 0 || len(bRows) == 0 {
		t.Fatal("expected a recorded row on both proxies")
	}

	// The outer proxy sees the real client IP.
	if aRows[0].ClientIP != "203.0.113.7" {
		t.Errorf("A client_ip=%q, want 203.0.113.7", aRows[0].ClientIP)
	}
	// forward() does not append X-Forwarded-For, so the inner proxy cannot see the
	// original client: it attributes the request to A's connection instead. This
	// documents the current behavior (see the proxy->proxy discussion). If we ever
	// start forwarding XFF, this test should be updated to assert propagation.
	if _, _, _, xff := cap.get(); xff != "" {
		t.Errorf("upstream unexpectedly saw X-Forwarded-For=%q (XFF forwarding changed?)", xff)
	}
	if bRows[0].ClientIP == "203.0.113.7" {
		t.Errorf("inner proxy B unexpectedly saw the original client IP")
	}
	if bRows[0].SessionID == "203.0.113.7" {
		t.Errorf("inner proxy B unexpectedly grouped the session by the original client IP")
	}
}

func TestChain_StreamingNDJSONSurvivesTwoHops(t *testing.T) {
	a, b, _ := newChain(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for _, l := range []string{
			`{"message":{"content":"he"},"done":false}`,
			`{"message":{"content":"llo"},"done":false}`,
			`{"message":{"content":""},"done":true,"eval_count":30,"prompt_eval_count":6}`,
		} {
			_, _ = fmt.Fprintf(w, "%s\n", l)
			fl.Flush()
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"llama3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	rr := httptest.NewRecorder()
	a.ServeHTTP(rr, req)

	// All three NDJSON chunks reached the client (framing intact across both hops).
	sc := bufio.NewScanner(strings.NewReader(rr.Body.String()))
	n := 0
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("client received %d non-empty chunks, want 3", n)
	}

	for name, h := range bothProxies(a, b) {
		rows, _, _ := h.store.ListRequests(1, 0, "", "")
		if len(rows) == 0 {
			t.Fatalf("proxy %s recorded no rows", name)
		}
		r := rows[0]
		if !r.Stream {
			t.Errorf("proxy %s stream=false, want true", name)
		}
		if r.PromptTokens != 6 || r.CompletionTokens != 30 {
			t.Errorf("proxy %s tokens pt=%d ct=%d, want 6/30", name, r.PromptTokens, r.CompletionTokens)
		}
		if r.ResponseText != "hello" {
			t.Errorf("proxy %s response_text=%q, want hello", name, r.ResponseText)
		}
	}
}

func TestChain_OpenAISSESurvivesTwoHops(t *testing.T) {
	a, b, _ := newChain(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for _, e := range []string{
			`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
			`data: {"choices":[{"delta":{"content":"lo"}}]}`,
			`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":11,"total_tokens":18}}`,
			`data: [DONE]`,
		} {
			_, _ = fmt.Fprintf(w, "%s\n\n", e) // SSE: blank line terminates each event
			fl.Flush()
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	rr := httptest.NewRecorder()
	a.ServeHTTP(rr, req)

	// The SSE sentinel and framing must reach the client through both hops.
	if !strings.Contains(rr.Body.String(), "data: [DONE]") {
		t.Errorf("client missing [DONE]; SSE framing broke across hops: %q", rr.Body.String())
	}

	for name, h := range bothProxies(a, b) {
		rows, _, _ := h.store.ListRequests(1, 0, "", "")
		if len(rows) == 0 {
			t.Fatalf("proxy %s recorded no rows", name)
		}
		r := rows[0]
		if r.PromptTokens != 7 || r.CompletionTokens != 11 {
			t.Errorf("proxy %s usage pt=%d ct=%d, want 7/11", name, r.PromptTokens, r.CompletionTokens)
		}
		if r.ResponseText != "Hello" {
			t.Errorf("proxy %s response_text=%q, want Hello", name, r.ResponseText)
		}
	}
}

func TestChain_StatusCodePropagates(t *testing.T) {
	a, b, _ := newChain(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/generate",
		strings.NewReader(`{"model":"m","stream":false}`))
	rr := httptest.NewRecorder()
	a.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("client status=%d, want 404", rr.Code)
	}
	for name, h := range bothProxies(a, b) {
		rows, _, _ := h.store.ListRequests(1, 0, "", "")
		if len(rows) == 0 || rows[0].StatusCode != http.StatusNotFound {
			t.Errorf("proxy %s did not record 404: %+v", name, rows)
		}
	}
}

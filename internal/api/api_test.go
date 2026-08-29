package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/ollama-proxy-metrics/internal/budget"
	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
	"github.com/nexusriot/ollama-proxy-metrics/internal/events"
	"github.com/nexusriot/ollama-proxy-metrics/internal/pricing"
)

func openTestDB(t *testing.T) *db.Store {
	t.Helper()
	s, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestMux(t *testing.T, store *db.Store) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	h := New(Options{Store: store})
	h.Register(mux, "/admin/api")
	return mux
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func insertSample(t *testing.T, s *db.Store, id, model, session string, pt, ct int64) {
	t.Helper()
	if err := s.InsertRequest(db.RequestRecord{
		RequestID:        id,
		SessionID:        session,
		Timestamp:        time.Now().UTC(),
		Endpoint:         "/api/generate",
		Method:           "POST",
		Model:            model,
		Stream:           false,
		StatusCode:       200,
		DurationMS:       500,
		RequestBytes:     100,
		ResponseBytes:    200,
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      pt + ct,
	}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
}

func TestCORSHeaders(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	rr := get(t, mux, "/admin/api/summary")
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("expected CORS *, got %q", got)
	}
}

func TestCORSPreflight(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	req := httptest.NewRequest(http.MethodOptions, "/admin/api/summary", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("expected 204 for OPTIONS, got %d", rr.Code)
	}
}

func TestHandleSummary_Empty(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	rr := get(t, mux, "/admin/api/summary")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var sum db.Summary
	if err := json.NewDecoder(rr.Body).Decode(&sum); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sum.TotalRequests != 0 {
		t.Errorf("expected 0 requests, got %d", sum.TotalRequests)
	}
}

func TestHandleSummary_WithData(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "s1", 10, 20)
	insertSample(t, store, "r2", "codellama", "s2", 5, 15)

	mux := newTestMux(t, store)
	rr := get(t, mux, "/admin/api/summary")

	var sum db.Summary
	_ = json.NewDecoder(rr.Body).Decode(&sum)
	if sum.TotalRequests != 2 {
		t.Errorf("expected 2, got %d", sum.TotalRequests)
	}
	if sum.TotalTokens != 50 {
		t.Errorf("expected total_tokens=50, got %d", sum.TotalTokens)
	}
	if len(sum.UniqueModels) != 2 {
		t.Errorf("expected 2 unique models, got %v", sum.UniqueModels)
	}
}

func TestHandleSummary_MethodNotAllowed(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	req := httptest.NewRequest(http.MethodPost, "/admin/api/summary", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}

func TestHandleRequests_Empty(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	rr := get(t, mux, "/admin/api/requests")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp requestsResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Total != 0 || len(resp.Data) != 0 {
		t.Errorf("expected empty, got %+v", resp)
	}
}

func TestHandleRequests_Pagination(t *testing.T) {
	store := openTestDB(t)
	for i := 0; i < 5; i++ {
		insertSample(t, store, string(rune('a'+i)), "llama3", "", 1, 1)
	}
	mux := newTestMux(t, store)
	rr := get(t, mux, "/admin/api/requests?limit=2&offset=0")

	var resp requestsResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Total != 5 {
		t.Errorf("expected total=5, got %d", resp.Total)
	}
	if len(resp.Data) != 2 {
		t.Errorf("expected 2 items, got %d", len(resp.Data))
	}
	if resp.Limit != 2 || resp.Offset != 0 {
		t.Errorf("unexpected limit/offset in response: %+v", resp)
	}
}

func TestHandleRequests_FilterByModel(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "", 1, 1)
	insertSample(t, store, "r2", "codellama", "", 1, 1)
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/requests?model=llama3")
	var resp requestsResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Total != 1 || len(resp.Data) != 1 {
		t.Errorf("filter by model: expected 1, got %+v", resp)
	}
}

func TestHandleRequests_FilterBySession(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "sess-A", 1, 1)
	insertSample(t, store, "r2", "llama3", "sess-B", 1, 1)
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/requests?session=sess-A")
	var resp requestsResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Total != 1 {
		t.Errorf("filter by session: expected 1, got %d", resp.Total)
	}
}

func TestHandleDaily_Empty(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	rr := get(t, mux, "/admin/api/daily")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var stats []db.DailyStat
	_ = json.NewDecoder(rr.Body).Decode(&stats)
	if len(stats) != 0 {
		t.Errorf("expected empty daily stats, got %v", stats)
	}
}

func TestHandleDaily_WithData(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "", 100, 200)
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/daily?days=7")
	var stats []db.DailyStat
	_ = json.NewDecoder(rr.Body).Decode(&stats)
	if len(stats) == 0 {
		t.Fatal("expected at least one daily stat")
	}
	if stats[0].TotalRequests != 1 {
		t.Errorf("expected 1 request, got %d", stats[0].TotalRequests)
	}
}

func TestHandleSessions_Empty(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	rr := get(t, mux, "/admin/api/sessions")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var stats []db.SessionStat
	_ = json.NewDecoder(rr.Body).Decode(&stats)
	if len(stats) != 0 {
		t.Errorf("expected empty sessions, got %v", stats)
	}
}

func TestHandleSessions_WithData(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "sess-X", 10, 20)
	insertSample(t, store, "r2", "llama3", "sess-X", 5, 10)
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/sessions")
	var stats []db.SessionStat
	_ = json.NewDecoder(rr.Body).Decode(&stats)
	if len(stats) == 0 {
		t.Fatal("expected session stats")
	}
	if stats[0].SessionID != "sess-X" {
		t.Errorf("expected sess-X, got %s", stats[0].SessionID)
	}
	if stats[0].TotalRequests != 2 {
		t.Errorf("expected 2, got %d", stats[0].TotalRequests)
	}
}

func TestHandleModels_Empty(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	rr := get(t, mux, "/admin/api/models")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var models []string
	_ = json.NewDecoder(rr.Body).Decode(&models)
	if len(models) != 0 {
		t.Errorf("expected empty models, got %v", models)
	}
}

func TestHandleModels_WithData(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "", 0, 0)
	insertSample(t, store, "r2", "codellama", "", 0, 0)
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/models")
	var models []string
	_ = json.NewDecoder(rr.Body).Decode(&models)
	if len(models) != 2 {
		t.Errorf("expected 2 models, got %v", models)
	}
}

func TestResponseContentType(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	rr := get(t, mux, "/admin/api/summary")
	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
}

func TestHandleCleanup_DeletesAllRows(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "s1", 10, 20)
	insertSample(t, store, "r2", "llama3", "s2", 5, 15)
	mux := newTestMux(t, store)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/cleanup", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// All rows must be gone.
	_, total, err := store.ListRequests(100, 0, "", "")
	if err != nil {
		t.Fatalf("ListRequests after cleanup: %v", err)
	}
	if total != 0 {
		t.Errorf("expected 0 rows after cleanup, got %d", total)
	}
}

func TestHandleCleanup_SummaryReturnsZeroAfterCleanup(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "s1", 100, 200)
	mux := newTestMux(t, store)

	req := httptest.NewRequest(http.MethodPost, "/admin/api/cleanup", nil)
	httptest.NewRecorder() // discard
	mux.ServeHTTP(httptest.NewRecorder(), req)

	rr := get(t, mux, "/admin/api/summary")
	var sum map[string]interface{}
	_ = json.NewDecoder(rr.Body).Decode(&sum)
	if sum["total_requests"].(float64) != 0 {
		t.Errorf("expected total_requests=0 after cleanup, got %v", sum["total_requests"])
	}
}

func TestHandleCleanup_MethodNotAllowed(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	rr := get(t, mux, "/admin/api/cleanup") // GET instead of POST
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}

func TestHandleCleanup_IdempotentOnEmptyDB(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	req := httptest.NewRequest(http.MethodPost, "/admin/api/cleanup", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 on empty DB, got %d", rr.Code)
	}
}

func TestHandleModelStats(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "s1", 10, 20)
	insertSample(t, store, "r2", "llama3", "s1", 10, 20)
	insertSample(t, store, "r3", "codellama", "s1", 5, 5)
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/model-stats")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var stats []db.ModelStat
	if err := json.NewDecoder(rr.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 models, got %d", len(stats))
	}
	if stats[0].Model != "llama3" || stats[0].TotalRequests != 2 {
		t.Errorf("unexpected busiest model: %+v", stats[0])
	}
}

func TestHandleRequests_Search(t *testing.T) {
	store := openTestDB(t)
	if err := store.InsertRequest(db.RequestRecord{
		RequestID: "r1", Timestamp: time.Now().UTC(), Endpoint: "/api/generate",
		Model: "llama3", PromptText: "why is the sky blue",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertRequest(db.RequestRecord{
		RequestID: "r2", Timestamp: time.Now().UTC(), Endpoint: "/api/generate",
		Model: "llama3", PromptText: "tell me a joke",
	}); err != nil {
		t.Fatal(err)
	}
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/requests?q=joke")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp struct {
		Data  []db.RequestRow `json:"data"`
		Total int             `json:"total"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Data) != 1 || resp.Data[0].RequestID != "r2" {
		t.Errorf("search q=joke => total=%d data=%+v", resp.Total, resp.Data)
	}
}

func TestHandleExport_CSV(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "s1", 10, 20)
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/export") // default format = csv
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/csv" {
		t.Errorf("expected text/csv, got %q", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "request_id,session_id") {
		t.Errorf("missing CSV header: %q", body)
	}
	if !strings.Contains(body, "r1") {
		t.Errorf("missing data row: %q", body)
	}
}

func TestHandleExport_JSON(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "r1", "llama3", "s1", 10, 20)
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/export?format=json")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var rows []db.RequestRow
	if err := json.NewDecoder(rr.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 || rows[0].RequestID != "r1" {
		t.Errorf("unexpected export rows: %+v", rows)
	}
}

func TestHandlePricing(t *testing.T) {
	store := openTestDB(t)
	prices := &pricing.Table{Currency: "EUR", Models: map[string]pricing.ModelRate{
		"m": {PromptPer1K: 1, CompletionPer1K: 2},
	}}
	mux := http.NewServeMux()
	New(Options{Store: store, Prices: prices}).Register(mux, "/admin/api")

	rr := get(t, mux, "/admin/api/pricing")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var got pricing.Table
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Currency != "EUR" || got.Models["m"].CompletionPer1K != 2 {
		t.Errorf("unexpected pricing payload: %+v", got)
	}
}

func TestHandleStream_NoBrokerReturns503(t *testing.T) {
	mux := newTestMux(t, openTestDB(t)) // newTestMux passes nil broker
	rr := get(t, mux, "/admin/api/stream")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 without broker, got %d", rr.Code)
	}
}

func TestHandleStream_PushesEvent(t *testing.T) {
	store := openTestDB(t)
	broker := events.NewBroker()
	mux := http.NewServeMux()
	New(Options{Store: store, Events: broker}).Register(mux, "/admin/api")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/admin/api/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	// Wait until the handler has subscribed, then publish an event.
	deadline := time.Now().Add(2 * time.Second)
	for broker.Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	broker.Publish([]byte(`{"request_id":"live-1"}`))

	timeout := time.After(2 * time.Second)
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, "live-1") {
				return // success
			}
		case <-timeout:
			t.Fatal("did not receive streamed event within timeout")
		}
	}
}

// insertFailure records a request that failed, for the error-view tests.
func insertFailure(t *testing.T, s *db.Store, id string, status int, errMsg string) {
	t.Helper()
	if err := s.InsertRequest(db.RequestRecord{
		RequestID:    id,
		SessionID:    "sess",
		Timestamp:    time.Now().UTC(),
		Endpoint:     "/api/generate",
		Method:       "POST",
		Model:        "llama3",
		StatusCode:   status,
		ErrorMessage: errMsg,
	}); err != nil {
		t.Fatalf("insert failure: %v", err)
	}
}

func TestRequests_ErrorsOnlyFilter(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "ok-1", "llama3", "sess", 10, 20)
	insertFailure(t, store, "bad-1", 502, "upstream: refused")
	insertFailure(t, store, "bad-2", 200, "write to client: broken pipe")
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/requests?errors=true")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got struct {
		Data  []db.RequestRow `json:"data"`
		Total int             `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 2 {
		t.Fatalf("total = %d, want the 2 failures", got.Total)
	}
	for _, row := range got.Data {
		if row.RequestID == "ok-1" {
			t.Fatal("errors=true returned the successful request")
		}
	}
}

func TestRequests_StatusFilter(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "ok-1", "llama3", "sess", 10, 20)
	insertFailure(t, store, "bad-1", 502, "upstream: refused")
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/requests?status=502")
	var got struct {
		Data  []db.RequestRow `json:"data"`
		Total int             `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 1 || got.Data[0].RequestID != "bad-1" {
		t.Fatalf("status=502 returned %+v", got.Data)
	}
}

func TestRequests_MalformedStatusIsIgnored(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "ok-1", "llama3", "sess", 10, 20)
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/requests?status=abc&errors=maybe")
	var got struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 1 {
		t.Fatalf("total = %d, want an unparseable filter to be ignored", got.Total)
	}
}

func TestStatusCounts(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "ok-1", "llama3", "sess", 10, 20)
	insertSample(t, store, "ok-2", "llama3", "sess", 10, 20)
	insertFailure(t, store, "bad-1", 502, "upstream: refused")
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/status-counts")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var counts []db.StatusCount
	if err := json.Unmarshal(rr.Body.Bytes(), &counts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[int]int64{}
	for _, c := range counts {
		got[c.StatusCode] = c.Count
	}
	if got[200] != 2 || got[502] != 1 {
		t.Fatalf("counts = %v", got)
	}
}

func TestStatusCounts_IgnoresTheStatusSelectionItself(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "ok-1", "llama3", "sess", 10, 20)
	insertFailure(t, store, "bad-1", 502, "upstream: refused")
	mux := newTestMux(t, store)

	// Selecting one code must not hide the others from the facet.
	rr := get(t, mux, "/admin/api/status-counts?status=502&errors=true")
	var counts []db.StatusCount
	if err := json.Unmarshal(rr.Body.Bytes(), &counts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(counts) != 2 {
		t.Fatalf("counts = %+v, want both status codes", counts)
	}
}

func TestStatusCounts_HonorsTheOtherFilters(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "ok-1", "llama3", "sess", 10, 20)
	insertSample(t, store, "ok-2", "mistral", "sess", 10, 20)
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/status-counts?model=mistral")
	var counts []db.StatusCount
	if err := json.Unmarshal(rr.Body.Bytes(), &counts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(counts) != 1 || counts[0].Count != 1 {
		t.Fatalf("counts = %+v, want just the one mistral row", counts)
	}
}

func TestStatusCounts_MethodGuard(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	req := httptest.NewRequest(http.MethodPost, "/admin/api/status-counts", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /status-counts = %d, want 405", rr.Code)
	}
}

func TestExport_HonorsTheErrorsFilter(t *testing.T) {
	store := openTestDB(t)
	insertSample(t, store, "ok-1", "llama3", "sess", 10, 20)
	insertFailure(t, store, "bad-1", 502, "upstream: refused")
	mux := newTestMux(t, store)

	rr := get(t, mux, "/admin/api/export?errors=true")
	body := rr.Body.String()
	if !strings.Contains(body, "bad-1") {
		t.Fatal("CSV export is missing the failing request")
	}
	if strings.Contains(body, "ok-1") {
		t.Fatal("CSV export included a successful request despite errors=true")
	}
	if !strings.Contains(body, "tokens_estimated") || !strings.Contains(body, "cached") {
		t.Fatal("CSV header is missing the estimated/cached columns")
	}
}

func TestBudgets_ReportsLimitsAndUsage(t *testing.T) {
	tracker := budget.New(budget.Limits{TokensPerDay: 1000, CostPerDay: 2.5})
	tracker.Add("sess", 250, 0.75)

	mux := http.NewServeMux()
	New(Options{Store: openTestDB(t), Budgets: tracker}).Register(mux, "/admin/api")

	rr := get(t, mux, "/admin/api/budgets")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got struct {
		Enabled bool                    `json:"enabled"`
		Day     string                  `json:"day"`
		Limits  budget.Limits           `json:"limits"`
		Usage   map[string]budget.Usage `json:"usage"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled || got.Limits.TokensPerDay != 1000 || got.Limits.CostPerDay != 2.5 {
		t.Fatalf("budgets = %+v", got)
	}
	if got.Usage["sess"].Tokens != 250 || got.Usage["sess"].Cost != 0.75 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if got.Day == "" {
		t.Error("budgets response carries no day")
	}
}

func TestBudgets_WithoutATracker(t *testing.T) {
	mux := newTestMux(t, openTestDB(t))
	rr := get(t, mux, "/admin/api/budgets")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled {
		t.Fatal("budgets reported enabled with no tracker configured")
	}
}

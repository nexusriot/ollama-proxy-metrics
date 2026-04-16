package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
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
	h := New(store)
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

package db

import (
	"testing"
	"time"
)

// openTestDB opens an in-memory SQLite database suitable for unit tests.
func openTestDB(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleRecord(id string) RequestRecord {
	return RequestRecord{
		RequestID:        id,
		SessionID:        "session-abc",
		Timestamp:        time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC),
		Endpoint:         "/api/generate",
		Method:           "POST",
		Model:            "llama3",
		Stream:           false,
		StatusCode:       200,
		DurationMS:       1234,
		RequestBytes:     512,
		ResponseBytes:    2048,
		PromptTokens:     100,
		CompletionTokens: 250,
		TotalTokens:      350,
		ErrorMessage:     "",
		ClientIP:         "127.0.0.1",
		UserAgent:        "curl/8.0",
	}
}

func TestOpen_CreatesSchema(t *testing.T) {
	s := openTestDB(t)
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM requests").Scan(&count); err != nil {
		t.Fatalf("table not created: %v", err)
	}
}

func TestInsertRequest_Basic(t *testing.T) {
	s := openTestDB(t)
	if err := s.InsertRequest(sampleRecord("req-1")); err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}
	var count int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM requests").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
}

func TestInsertRequest_DuplicateIDReturnsError(t *testing.T) {
	s := openTestDB(t)
	r := sampleRecord("dup-id")
	if err := s.InsertRequest(r); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := s.InsertRequest(r); err == nil {
		t.Fatal("expected error on duplicate request_id, got nil")
	}
}

func TestInsertRequest_TokensStoredAsInt64(t *testing.T) {
	s := openTestDB(t)
	r := sampleRecord("req-bigint")
	r.PromptTokens = 1_000_000_000_000
	r.CompletionTokens = 2_000_000_000_000
	r.TotalTokens = 3_000_000_000_000
	if err := s.InsertRequest(r); err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}
	var pt, ct, tt int64
	_ = s.db.QueryRow("SELECT prompt_tokens, completion_tokens, total_tokens FROM requests WHERE request_id = ?", "req-bigint").
		Scan(&pt, &ct, &tt)
	if pt != 1_000_000_000_000 || ct != 2_000_000_000_000 || tt != 3_000_000_000_000 {
		t.Fatalf("unexpected token values: pt=%d ct=%d tt=%d", pt, ct, tt)
	}
}

func TestListRequests_Pagination(t *testing.T) {
	s := openTestDB(t)
	for i := 0; i < 5; i++ {
		r := sampleRecord(string(rune('a' + i)))
		r.Timestamp = time.Date(2026, 4, 15, i, 0, 0, 0, time.UTC)
		if err := s.InsertRequest(r); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	rows, total, err := s.ListRequests(2, 0, "", "")
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if total != 5 {
		t.Errorf("expected total=5, got %d", total)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows on page 1, got %d", len(rows))
	}
}

func TestListRequests_FilterByModel(t *testing.T) {
	s := openTestDB(t)
	r1 := sampleRecord("r1")
	r1.Model = "llama3"
	r2 := sampleRecord("r2")
	r2.Model = "codellama"
	_ = s.InsertRequest(r1)
	_ = s.InsertRequest(r2)

	rows, total, err := s.ListRequests(10, 0, "llama3", "")
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Errorf("expected 1 row for model llama3, got total=%d rows=%d", total, len(rows))
	}
}

func TestListRequests_FilterBySession(t *testing.T) {
	s := openTestDB(t)
	r1 := sampleRecord("r1")
	r1.SessionID = "sess-A"
	r2 := sampleRecord("r2")
	r2.SessionID = "sess-B"
	_ = s.InsertRequest(r1)
	_ = s.InsertRequest(r2)

	rows, total, err := s.ListRequests(10, 0, "", "sess-A")
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Errorf("expected 1 row for sess-A, got total=%d rows=%d", total, len(rows))
	}
}

func TestListRequests_OrderNewestFirst(t *testing.T) {
	s := openTestDB(t)
	r1 := sampleRecord("r1")
	r1.Timestamp = time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC)
	r2 := sampleRecord("r2")
	r2.Timestamp = time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	_ = s.InsertRequest(r1)
	_ = s.InsertRequest(r2)

	rows, _, _ := s.ListRequests(10, 0, "", "")
	if rows[0].RequestID != "r2" {
		t.Errorf("expected newest first (r2), got %s", rows[0].RequestID)
	}
}

func TestListRequests_StreamFieldRoundtrips(t *testing.T) {
	s := openTestDB(t)
	r := sampleRecord("stream-req")
	r.Stream = true
	_ = s.InsertRequest(r)

	rows, _, _ := s.ListRequests(1, 0, "", "")
	if !rows[0].Stream {
		t.Error("expected stream=true to round-trip")
	}
}

func TestDailyStats_Aggregates(t *testing.T) {
	s := openTestDB(t)
	for i, ts := range []time.Time{
		time.Now().UTC().Add(-24 * time.Hour),
		time.Now().UTC().Add(-24 * time.Hour),
		time.Now().UTC(),
	} {
		r := sampleRecord(string(rune('a' + i)))
		r.Timestamp = ts
		r.PromptTokens = 100
		r.CompletionTokens = 50
		r.TotalTokens = 150
		_ = s.InsertRequest(r)
	}

	stats, err := s.DailyStats(7)
	if err != nil {
		t.Fatalf("DailyStats: %v", err)
	}
	if len(stats) < 1 {
		t.Fatal("expected at least 1 daily stat")
	}
	// Yesterday should have 2 requests
	var found bool
	for _, d := range stats {
		if d.TotalRequests == 2 {
			found = true
			if d.TotalTokens != 300 {
				t.Errorf("expected 300 total tokens for day with 2 requests, got %d", d.TotalTokens)
			}
		}
	}
	if !found {
		t.Error("expected a day with 2 requests")
	}
}

func TestSessionStats_GroupsBySession(t *testing.T) {
	s := openTestDB(t)
	for i := 0; i < 3; i++ {
		r := sampleRecord(string(rune('a' + i)))
		r.SessionID = "sess-X"
		r.PromptTokens = 100
		r.TotalTokens = 100
		_ = s.InsertRequest(r)
	}
	r := sampleRecord("other")
	r.SessionID = "sess-Y"
	_ = s.InsertRequest(r)

	stats, err := s.SessionStats(10)
	if err != nil {
		t.Fatalf("SessionStats: %v", err)
	}
	var sx SessionStat
	for _, ss := range stats {
		if ss.SessionID == "sess-X" {
			sx = ss
		}
	}
	if sx.TotalRequests != 3 {
		t.Errorf("expected 3 requests for sess-X, got %d", sx.TotalRequests)
	}
	if sx.PromptTokens != 300 {
		t.Errorf("expected 300 prompt tokens for sess-X, got %d", sx.PromptTokens)
	}
}

func TestSessionStats_EmptySessionExcluded(t *testing.T) {
	s := openTestDB(t)
	r := sampleRecord("no-session")
	r.SessionID = ""
	_ = s.InsertRequest(r)

	stats, err := s.SessionStats(10)
	if err != nil {
		t.Fatalf("SessionStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected empty session to be excluded, got %d rows", len(stats))
	}
}

func TestGetSummary_Empty(t *testing.T) {
	s := openTestDB(t)
	sum, err := s.GetSummary()
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if sum.TotalRequests != 0 {
		t.Errorf("expected 0 requests, got %d", sum.TotalRequests)
	}
	if len(sum.UniqueModels) != 0 {
		t.Errorf("expected no models, got %v", sum.UniqueModels)
	}
}

func TestGetSummary_Counts(t *testing.T) {
	s := openTestDB(t)
	models := []string{"llama3", "codellama", "llama3"}
	for i, m := range models {
		r := sampleRecord(string(rune('a' + i)))
		r.Model = m
		r.SessionID = "sess"
		r.PromptTokens = 10
		r.CompletionTokens = 20
		r.TotalTokens = 30
		_ = s.InsertRequest(r)
	}

	sum, err := s.GetSummary()
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if sum.TotalRequests != 3 {
		t.Errorf("expected 3 requests, got %d", sum.TotalRequests)
	}
	if sum.TotalTokens != 90 {
		t.Errorf("expected 90 total tokens, got %d", sum.TotalTokens)
	}
	if len(sum.UniqueModels) != 2 {
		t.Errorf("expected 2 unique models, got %v", sum.UniqueModels)
	}
	if sum.UniqueSessions != 1 {
		t.Errorf("expected 1 unique session, got %d", sum.UniqueSessions)
	}
}

func TestModels_ReturnsDistinct(t *testing.T) {
	s := openTestDB(t)
	for i, m := range []string{"a", "b", "a"} {
		r := sampleRecord(string(rune('x' + i)))
		r.Model = m
		_ = s.InsertRequest(r)
	}
	models, err := s.Models()
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("expected 2 distinct models, got %v", models)
	}
}

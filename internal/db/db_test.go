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

func TestModelStats_Aggregates(t *testing.T) {
	s := openTestDB(t)
	for i := 0; i < 2; i++ {
		r := sampleRecord(string(rune('a' + i)))
		r.Model = "llama3"
		r.PromptTokens = 100
		r.CompletionTokens = 200
		r.TotalTokens = 300
		r.Cost = 1.5
		_ = s.InsertRequest(r)
	}
	r := sampleRecord("c")
	r.Model = "codellama"
	r.TotalTokens = 10
	_ = s.InsertRequest(r)

	stats, err := s.ModelStats(10)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 models, got %d", len(stats))
	}
	// Busiest (most total tokens) first => llama3.
	if stats[0].Model != "llama3" {
		t.Errorf("expected llama3 first, got %s", stats[0].Model)
	}
	if stats[0].TotalRequests != 2 || stats[0].TotalTokens != 600 {
		t.Errorf("llama3 aggregates wrong: req=%d tokens=%d", stats[0].TotalRequests, stats[0].TotalTokens)
	}
	if stats[0].Cost != 3.0 {
		t.Errorf("expected llama3 cost=3.0, got %v", stats[0].Cost)
	}
}

func TestModelStats_ExcludesUnknown(t *testing.T) {
	s := openTestDB(t)
	r := sampleRecord("u")
	r.Model = "unknown"
	_ = s.InsertRequest(r)
	stats, err := s.ModelStats(10)
	if err != nil {
		t.Fatalf("ModelStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 'unknown' excluded, got %d rows", len(stats))
	}
}

func TestExportRequests_RespectsFilterAndLimit(t *testing.T) {
	s := openTestDB(t)
	for i := 0; i < 5; i++ {
		r := sampleRecord(string(rune('a' + i)))
		r.Model = "llama3"
		_ = s.InsertRequest(r)
	}
	r := sampleRecord("other")
	r.Model = "codellama"
	_ = s.InsertRequest(r)

	rows, err := s.ExportRequests(1000, "llama3", "")
	if err != nil {
		t.Fatalf("ExportRequests: %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("expected 5 llama3 rows, got %d", len(rows))
	}

	limited, err := s.ExportRequests(2, "", "")
	if err != nil {
		t.Fatalf("ExportRequests limited: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("expected limit of 2 respected, got %d", len(limited))
	}
}

func TestListRequestsFiltered_Query(t *testing.T) {
	s := openTestDB(t)
	r1 := sampleRecord("r1")
	r1.PromptText = "why is the sky blue"
	r2 := sampleRecord("r2")
	r2.PromptText = "tell me a joke"
	r2.ResponseText = "the sky is the limit"
	_ = s.InsertRequest(r1)
	_ = s.InsertRequest(r2)

	// Matches r1's prompt only.
	rows, total, err := s.ListRequestsFiltered(10, 0, RequestFilter{Query: "sky blue"})
	if err != nil {
		t.Fatalf("ListRequestsFiltered: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].RequestID != "r1" {
		t.Errorf("query 'sky blue' => total=%d rows=%d", total, len(rows))
	}

	// Case-insensitive, matches both prompt (r1) and response (r2).
	_, total, _ = s.ListRequestsFiltered(10, 0, RequestFilter{Query: "SKY"})
	if total != 2 {
		t.Errorf("query 'SKY' should match both rows, got %d", total)
	}

	// LIKE wildcards in the term are matched literally (escaped).
	_, total, _ = s.ListRequestsFiltered(10, 0, RequestFilter{Query: "%"})
	if total != 0 {
		t.Errorf("literal '%%' should match nothing, got %d", total)
	}
}

func TestListRequestsFiltered_DateRange(t *testing.T) {
	s := openTestDB(t)
	for i, ts := range []time.Time{
		time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC),
	} {
		r := sampleRecord(string(rune('a' + i)))
		r.Timestamp = ts
		_ = s.InsertRequest(r)
	}

	_, total, err := s.ListRequestsFiltered(10, 0, RequestFilter{
		Since: "2026-04-12T00:00:00Z",
		Until: "2026-04-18T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("ListRequestsFiltered: %v", err)
	}
	if total != 1 {
		t.Errorf("date range should match exactly the Apr-15 row, got %d", total)
	}
}

func TestExportRequestsFiltered_Query(t *testing.T) {
	s := openTestDB(t)
	r1 := sampleRecord("r1")
	r1.Model = "llama3"
	r2 := sampleRecord("r2")
	r2.Model = "codellama"
	_ = s.InsertRequest(r1)
	_ = s.InsertRequest(r2)

	rows, err := s.ExportRequestsFiltered(1000, RequestFilter{Query: "codel"})
	if err != nil {
		t.Fatalf("ExportRequestsFiltered: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "codellama" {
		t.Errorf("expected only codellama row, got %+v", rows)
	}
}

func TestGetSummary_Cost(t *testing.T) {
	s := openTestDB(t)
	for i := 0; i < 3; i++ {
		r := sampleRecord(string(rune('a' + i)))
		r.Cost = 2.5
		_ = s.InsertRequest(r)
	}
	sum, err := s.GetSummary()
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if sum.Cost != 7.5 {
		t.Errorf("expected total cost 7.5, got %v", sum.Cost)
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

func TestInsertRequest_RoundTripsEstimatedAndCachedFlags(t *testing.T) {
	s := openTestDB(t)

	plain := sampleRecord("plain")
	estimated := sampleRecord("estimated")
	estimated.TokensEstimated = true
	cached := sampleRecord("cached")
	cached.Cached = true

	for _, r := range []RequestRecord{plain, estimated, cached} {
		if err := s.InsertRequest(r); err != nil {
			t.Fatalf("InsertRequest(%s): %v", r.RequestID, err)
		}
	}

	rows, _, err := s.ListRequestsFiltered(10, 0, RequestFilter{})
	if err != nil {
		t.Fatalf("ListRequestsFiltered: %v", err)
	}
	got := map[string]RequestRow{}
	for _, r := range rows {
		got[r.RequestID] = r
	}
	if got["plain"].TokensEstimated || got["plain"].Cached {
		t.Errorf("plain row = %+v, want both flags false", got["plain"])
	}
	if !got["estimated"].TokensEstimated || got["estimated"].Cached {
		t.Errorf("estimated row = %+v, want only TokensEstimated", got["estimated"])
	}
	if !got["cached"].Cached || got["cached"].TokensEstimated {
		t.Errorf("cached row = %+v, want only Cached", got["cached"])
	}
}

// errorFixtures inserts one healthy row plus three failures of different shapes.
func errorFixtures(t *testing.T) *Store {
	t.Helper()
	s := openTestDB(t)

	ok := sampleRecord("ok")

	notFound := sampleRecord("not-found")
	notFound.StatusCode = 404

	upstream := sampleRecord("upstream")
	upstream.StatusCode = 502
	upstream.ErrorMessage = "upstream: connection refused"

	// A client that hangs up mid-stream leaves a message but a 200 status.
	disconnect := sampleRecord("disconnect")
	disconnect.StatusCode = 200
	disconnect.ErrorMessage = "write to client: broken pipe"

	for _, r := range []RequestRecord{ok, notFound, upstream, disconnect} {
		if err := s.InsertRequest(r); err != nil {
			t.Fatalf("InsertRequest(%s): %v", r.RequestID, err)
		}
	}
	return s
}

func TestListRequestsFiltered_ErrorsOnly(t *testing.T) {
	s := errorFixtures(t)

	rows, total, err := s.ListRequestsFiltered(10, 0, RequestFilter{ErrorsOnly: true})
	if err != nil {
		t.Fatalf("ListRequestsFiltered: %v", err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("errors-only total = %d (%d rows), want 3", total, len(rows))
	}
	for _, r := range rows {
		if r.RequestID == "ok" {
			t.Fatal("errors-only returned the successful row")
		}
	}
}

func TestListRequestsFiltered_ByStatus(t *testing.T) {
	s := errorFixtures(t)

	rows, total, err := s.ListRequestsFiltered(10, 0, RequestFilter{Status: 502})
	if err != nil {
		t.Fatalf("ListRequestsFiltered: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].RequestID != "upstream" {
		t.Fatalf("status=502 returned %d rows: %+v", total, rows)
	}
}

func TestListRequestsFiltered_StatusCombinesWithOtherFilters(t *testing.T) {
	s := errorFixtures(t)

	_, total, err := s.ListRequestsFiltered(10, 0, RequestFilter{Status: 502, Model: "nope"})
	if err != nil {
		t.Fatalf("ListRequestsFiltered: %v", err)
	}
	if total != 0 {
		t.Fatalf("status + non-matching model returned %d rows, want 0", total)
	}
}

func TestStatusCounts(t *testing.T) {
	s := errorFixtures(t)

	counts, err := s.StatusCounts(RequestFilter{})
	if err != nil {
		t.Fatalf("StatusCounts: %v", err)
	}
	got := map[int]int64{}
	for _, c := range counts {
		got[c.StatusCode] = c.Count
	}
	if got[200] != 2 || got[404] != 1 || got[502] != 1 {
		t.Fatalf("StatusCounts = %v", got)
	}
	// Commonest first.
	if counts[0].StatusCode != 200 {
		t.Fatalf("StatusCounts[0] = %d, want the commonest status", counts[0].StatusCode)
	}
}

func TestStatusCounts_HonorsOtherFilters(t *testing.T) {
	s := errorFixtures(t)

	counts, err := s.StatusCounts(RequestFilter{Model: "llama3", Session: "nobody"})
	if err != nil {
		t.Fatalf("StatusCounts: %v", err)
	}
	if len(counts) != 0 {
		t.Fatalf("StatusCounts with a non-matching session = %v, want none", counts)
	}
}

func TestUsageSince(t *testing.T) {
	s := openTestDB(t)

	old := sampleRecord("old")
	old.Timestamp = time.Date(2026, 4, 14, 23, 0, 0, 0, time.UTC)
	old.Cost = 5

	today := sampleRecord("today")
	today.Cost = 0.25

	other := sampleRecord("other-session")
	other.SessionID = "session-xyz"
	other.TotalTokens = 10
	other.Cost = 0.5

	anon := sampleRecord("anon")
	anon.SessionID = ""

	for _, r := range []RequestRecord{old, today, other, anon} {
		if err := s.InsertRequest(r); err != nil {
			t.Fatalf("InsertRequest(%s): %v", r.RequestID, err)
		}
	}

	usage, err := s.UsageSince("2026-04-15T00:00:00Z")
	if err != nil {
		t.Fatalf("UsageSince: %v", err)
	}
	got := map[string]SessionUsage{}
	for _, u := range usage {
		got[u.SessionID] = u
	}
	if len(got) != 2 {
		t.Fatalf("UsageSince returned %d sessions: %+v", len(got), usage)
	}
	if got["session-abc"].Tokens != 350 || got["session-abc"].Cost != 0.25 {
		t.Errorf("session-abc usage = %+v, want today's row only", got["session-abc"])
	}
	if got["session-xyz"].Tokens != 10 {
		t.Errorf("session-xyz usage = %+v", got["session-xyz"])
	}
}

func TestRenameModel(t *testing.T) {
	s := openTestDB(t)

	tagged := sampleRecord("tagged")
	tagged.Model = "llama3:latest"
	untagged := sampleRecord("untagged")
	untagged.Model = "llama3"
	other := sampleRecord("other")
	other.Model = "mistral:7b"

	for _, r := range []RequestRecord{tagged, untagged, other} {
		if err := s.InsertRequest(r); err != nil {
			t.Fatalf("InsertRequest(%s): %v", r.RequestID, err)
		}
	}

	n, err := s.RenameModel("llama3", "llama3:latest")
	if err != nil {
		t.Fatalf("RenameModel: %v", err)
	}
	if n != 1 {
		t.Fatalf("RenameModel affected %d rows, want 1", n)
	}

	models, err := s.Models()
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0] != "llama3:latest" || models[1] != "mistral:7b" {
		t.Fatalf("Models after rename = %v", models)
	}
}

func TestRenameModel_NoopForEmptyOrIdenticalName(t *testing.T) {
	s := openTestDB(t)
	if err := s.InsertRequest(sampleRecord("r1")); err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}
	for _, c := range [][2]string{{"", "x"}, {"llama3", "llama3"}} {
		n, err := s.RenameModel(c[0], c[1])
		if err != nil || n != 0 {
			t.Fatalf("RenameModel(%q,%q) = %d, %v; want 0, nil", c[0], c[1], n, err)
		}
	}
}

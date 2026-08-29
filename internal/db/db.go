// Package db provides a SQLite-backed store for per-request metrics.
package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // CGO-free SQLite driver; registers as "sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS requests (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id        TEXT    NOT NULL UNIQUE,
    session_id        TEXT    NOT NULL DEFAULT '',
    timestamp         TEXT    NOT NULL,
    endpoint          TEXT    NOT NULL,
    method            TEXT    NOT NULL DEFAULT 'POST',
    model             TEXT    NOT NULL DEFAULT '',
    stream            INTEGER NOT NULL DEFAULT 0,
    status_code       INTEGER NOT NULL DEFAULT 0,
    duration_ms       BIGINT  NOT NULL DEFAULT 0,
    ttft_ms           BIGINT  NOT NULL DEFAULT 0,
    request_bytes     BIGINT  NOT NULL DEFAULT 0,
    response_bytes    BIGINT  NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT  NOT NULL DEFAULT 0,
    completion_tokens BIGINT  NOT NULL DEFAULT 0,
    total_tokens      BIGINT  NOT NULL DEFAULT 0,
    cost              REAL    NOT NULL DEFAULT 0,
    error_message     TEXT    NOT NULL DEFAULT '',
    client_ip         TEXT    NOT NULL DEFAULT '',
    user_agent        TEXT    NOT NULL DEFAULT '',
    prompt_text       TEXT    NOT NULL DEFAULT '',
    response_text     TEXT    NOT NULL DEFAULT '',
    tokens_estimated  INTEGER NOT NULL DEFAULT 0,
    cached            INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_requests_timestamp  ON requests(timestamp);
CREATE INDEX IF NOT EXISTS idx_requests_session_id ON requests(session_id);
CREATE INDEX IF NOT EXISTS idx_requests_model      ON requests(model);
CREATE INDEX IF NOT EXISTS idx_requests_date       ON requests(substr(timestamp,1,10));
CREATE INDEX IF NOT EXISTS idx_requests_status     ON requests(status_code);
`

// migrations are additive, idempotent ALTERs applied on every Open so that
// databases created by older versions migrate forward automatically. SQLite
// returns "duplicate column name" when a column already exists — that's fine.
var migrations = []string{
	`ALTER TABLE requests ADD COLUMN ttft_ms       BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE requests ADD COLUMN cost          REAL   NOT NULL DEFAULT 0`,
	`ALTER TABLE requests ADD COLUMN prompt_text   TEXT   NOT NULL DEFAULT ''`,
	`ALTER TABLE requests ADD COLUMN response_text TEXT   NOT NULL DEFAULT ''`,
	`ALTER TABLE requests ADD COLUMN tokens_estimated INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE requests ADD COLUMN cached           INTEGER NOT NULL DEFAULT 0`,
}

// Store wraps a SQLite database connection.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// WAL mode for better write concurrency.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	for _, m := range migrations {
		_, _ = db.Exec(m)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error { return s.db.Close() }

// RequestRecord holds all data we persist for one proxied request.
type RequestRecord struct {
	RequestID        string
	SessionID        string
	Timestamp        time.Time
	Endpoint         string
	Method           string
	Model            string
	Stream           bool
	StatusCode       int
	DurationMS       int64
	TTFTMs           int64
	RequestBytes     int64
	ResponseBytes    int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	Cost             float64
	ErrorMessage     string
	ClientIP         string
	UserAgent        string
	PromptText       string
	ResponseText     string
	TokensEstimated  bool
	Cached           bool
}

// InsertRequest persists a RequestRecord.
func (s *Store) InsertRequest(r RequestRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO requests (
			request_id, session_id, timestamp, endpoint, method, model, stream,
			status_code, duration_ms, ttft_ms, request_bytes, response_bytes,
			prompt_tokens, completion_tokens, total_tokens, cost,
			error_message, client_ip, user_agent,
			prompt_text, response_text, tokens_estimated, cached
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.RequestID,
		r.SessionID,
		r.Timestamp.UTC().Format(time.RFC3339Nano),
		r.Endpoint,
		r.Method,
		r.Model,
		boolToInt(r.Stream),
		r.StatusCode,
		r.DurationMS,
		r.TTFTMs,
		r.RequestBytes,
		r.ResponseBytes,
		r.PromptTokens,
		r.CompletionTokens,
		r.TotalTokens,
		r.Cost,
		r.ErrorMessage,
		r.ClientIP,
		r.UserAgent,
		r.PromptText,
		r.ResponseText,
		boolToInt(r.TokensEstimated),
		boolToInt(r.Cached),
	)
	return err
}

// RequestRow is a full request record as returned by list queries.
type RequestRow struct {
	ID               int64     `json:"id"`
	RequestID        string    `json:"request_id"`
	SessionID        string    `json:"session_id"`
	Timestamp        time.Time `json:"timestamp"`
	Endpoint         string    `json:"endpoint"`
	Method           string    `json:"method"`
	Model            string    `json:"model"`
	Stream           bool      `json:"stream"`
	StatusCode       int       `json:"status_code"`
	DurationMS       int64     `json:"duration_ms"`
	TTFTMs           int64     `json:"ttft_ms"`
	RequestBytes     int64     `json:"request_bytes"`
	ResponseBytes    int64     `json:"response_bytes"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	Cost             float64   `json:"cost"`
	ErrorMessage     string    `json:"error_message"`
	ClientIP         string    `json:"client_ip"`
	UserAgent        string    `json:"user_agent"`
	PromptText       string    `json:"prompt_text"`
	ResponseText     string    `json:"response_text"`
	TokensEstimated  bool      `json:"tokens_estimated"`
	Cached           bool      `json:"cached"`
}

// selectColumns is the canonical column list for reading full request rows.
const selectColumns = `
	id, request_id, session_id, timestamp, endpoint, method, model, stream,
	status_code, duration_ms, ttft_ms, request_bytes, response_bytes,
	prompt_tokens, completion_tokens, total_tokens, cost,
	error_message, client_ip, user_agent,
	prompt_text, response_text, tokens_estimated, cached`

// scanRow scans one full request row from a *sql.Rows positioned at a record.
func scanRow(dbRows *sql.Rows) (RequestRow, error) {
	var r RequestRow
	var tsStr string
	var streamInt, estimatedInt, cachedInt int
	err := dbRows.Scan(
		&r.ID, &r.RequestID, &r.SessionID, &tsStr,
		&r.Endpoint, &r.Method, &r.Model, &streamInt,
		&r.StatusCode, &r.DurationMS, &r.TTFTMs, &r.RequestBytes, &r.ResponseBytes,
		&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.Cost,
		&r.ErrorMessage, &r.ClientIP, &r.UserAgent,
		&r.PromptText, &r.ResponseText, &estimatedInt, &cachedInt,
	)
	if err != nil {
		return r, err
	}
	r.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)
	r.Stream = streamInt != 0
	r.TokensEstimated = estimatedInt != 0
	r.Cached = cachedInt != 0
	return r, nil
}

// RequestFilter holds the optional filters applied to request list/export
// queries. Empty fields are ignored.
type RequestFilter struct {
	Model   string // exact model match
	Session string // exact session_id match
	Query   string // case-insensitive substring across endpoint/model/session/prompt/response
	Since   string // inclusive lower bound on timestamp (RFC3339; lexical == chronological)
	Until   string // inclusive upper bound on timestamp (RFC3339)
	Status  int    // exact status_code match; 0 means any
	// ErrorsOnly keeps just the failures: rows that recorded an error message or
	// answered with a 4xx/5xx. Both are needed — a rate-limited request has a
	// status and a message, a client disconnect mid-stream has only a message.
	ErrorsOnly bool
}

// filterClause builds a parameterized WHERE clause from a RequestFilter.
func filterClause(f RequestFilter) (where string, args []interface{}) {
	conds := []string{"1=1"}
	if f.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, f.Model)
	}
	if f.Session != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, f.Session)
	}
	if f.Query != "" {
		like := "%" + escapeLike(f.Query) + "%"
		conds = append(conds, "(endpoint LIKE ? ESCAPE '\\' OR model LIKE ? ESCAPE '\\' OR session_id LIKE ? ESCAPE '\\' OR prompt_text LIKE ? ESCAPE '\\' OR response_text LIKE ? ESCAPE '\\')")
		args = append(args, like, like, like, like, like)
	}
	if f.Since != "" {
		conds = append(conds, "timestamp >= ?")
		args = append(args, f.Since)
	}
	if f.Until != "" {
		conds = append(conds, "timestamp <= ?")
		args = append(args, f.Until)
	}
	if f.Status != 0 {
		conds = append(conds, "status_code = ?")
		args = append(args, f.Status)
	}
	if f.ErrorsOnly {
		conds = append(conds, "(error_message != '' OR status_code >= 400)")
	}
	return strings.Join(conds, " AND "), args
}

// escapeLike escapes the LIKE wildcards so a search term is matched literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ListRequests returns paginated requests, newest first, optionally filtered by
// exact model and/or session_id.
func (s *Store) ListRequests(limit, offset int, model, sessionID string) ([]RequestRow, int, error) {
	return s.ListRequestsFiltered(limit, offset, RequestFilter{Model: model, Session: sessionID})
}

// ListRequestsFiltered returns paginated requests, newest first, matching f.
func (s *Store) ListRequestsFiltered(limit, offset int, f RequestFilter) (rows []RequestRow, total int, err error) {
	where, args := filterClause(f)

	if err = s.db.QueryRow("SELECT COUNT(*) FROM requests WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT ` + selectColumns + `
		FROM requests WHERE ` + where + `
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?`

	args = append(args, limit, offset)
	dbRows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer dbRows.Close()

	for dbRows.Next() {
		r, err := scanRow(dbRows)
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, r)
	}
	return rows, total, dbRows.Err()
}

// ExportRequests returns up to limit requests (newest first) matching the model
// and/or session filters, for CSV/JSON export.
func (s *Store) ExportRequests(limit int, model, sessionID string) ([]RequestRow, error) {
	return s.ExportRequestsFiltered(limit, RequestFilter{Model: model, Session: sessionID})
}

// ExportRequestsFiltered returns up to limit requests (newest first) matching f.
func (s *Store) ExportRequestsFiltered(limit int, f RequestFilter) ([]RequestRow, error) {
	where, args := filterClause(f)
	query := `SELECT ` + selectColumns + `
		FROM requests WHERE ` + where + `
		ORDER BY timestamp DESC
		LIMIT ?`
	args = append(args, limit)
	dbRows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer dbRows.Close()

	var rows []RequestRow
	for dbRows.Next() {
		r, err := scanRow(dbRows)
		if err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, dbRows.Err()
}

// DailyStat holds aggregated statistics for one calendar day.
type DailyStat struct {
	Date             string  `json:"date"`
	TotalRequests    int64   `json:"total_requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	AvgDurationMS    float64 `json:"avg_duration_ms"`
	Cost             float64 `json:"cost"`
	ErrorCount       int64   `json:"error_count"`
}

// DailyStats returns per-day aggregates for the last n days.
func (s *Store) DailyStats(days int) ([]DailyStat, error) {
	rows, err := s.db.Query(`
		SELECT
			substr(timestamp,1,10)                                       AS day,
			COUNT(*)                                                      AS total_requests,
			COALESCE(SUM(prompt_tokens),0)                               AS prompt_tokens,
			COALESCE(SUM(completion_tokens),0)                           AS completion_tokens,
			COALESCE(SUM(total_tokens),0)                                AS total_tokens,
			COALESCE(AVG(duration_ms),0.0)                               AS avg_duration_ms,
			COALESCE(SUM(cost),0.0)                                      AS cost,
			COALESCE(SUM(CASE WHEN error_message != '' THEN 1 ELSE 0 END), 0) AS error_count
		FROM requests
		WHERE timestamp >= datetime('now', ? || ' days')
		GROUP BY day
		ORDER BY day ASC`,
		fmt.Sprintf("-%d", days),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DailyStat
	for rows.Next() {
		var d DailyStat
		if err := rows.Scan(
			&d.Date, &d.TotalRequests,
			&d.PromptTokens, &d.CompletionTokens, &d.TotalTokens,
			&d.AvgDurationMS, &d.Cost, &d.ErrorCount,
		); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SessionStat holds aggregated statistics per session.
type SessionStat struct {
	SessionID        string    `json:"session_id"`
	TotalRequests    int64     `json:"total_requests"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	AvgDurationMS    float64   `json:"avg_duration_ms"`
	Cost             float64   `json:"cost"`
	FirstSeen        time.Time `json:"first_seen"`
	LastSeen         time.Time `json:"last_seen"`
}

// SessionStats returns per-session aggregates ordered by last activity.
func (s *Store) SessionStats(limit int) ([]SessionStat, error) {
	rows, err := s.db.Query(`
		SELECT
			session_id,
			COUNT(*)                           AS total_requests,
			COALESCE(SUM(prompt_tokens),0)     AS prompt_tokens,
			COALESCE(SUM(completion_tokens),0) AS completion_tokens,
			COALESCE(SUM(total_tokens),0)      AS total_tokens,
			COALESCE(AVG(duration_ms),0.0)     AS avg_duration_ms,
			COALESCE(SUM(cost),0.0)            AS cost,
			MIN(timestamp)                     AS first_seen,
			MAX(timestamp)                     AS last_seen
		FROM requests
		WHERE session_id != ''
		GROUP BY session_id
		ORDER BY last_seen DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionStat
	for rows.Next() {
		var ss SessionStat
		var firstStr, lastStr string
		if err := rows.Scan(
			&ss.SessionID, &ss.TotalRequests,
			&ss.PromptTokens, &ss.CompletionTokens, &ss.TotalTokens,
			&ss.AvgDurationMS, &ss.Cost, &firstStr, &lastStr,
		); err != nil {
			return nil, err
		}
		ss.FirstSeen, _ = time.Parse(time.RFC3339Nano, firstStr)
		ss.LastSeen, _ = time.Parse(time.RFC3339Nano, lastStr)
		out = append(out, ss)
	}
	return out, rows.Err()
}

// ModelStat holds aggregated statistics per model, for the dashboard's model
// comparison view.
type ModelStat struct {
	Model            string    `json:"model"`
	TotalRequests    int64     `json:"total_requests"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	AvgDurationMS    float64   `json:"avg_duration_ms"`
	AvgTTFTMs        float64   `json:"avg_ttft_ms"`
	TokensPerSec     float64   `json:"tokens_per_sec"`
	Cost             float64   `json:"cost"`
	ErrorCount       int64     `json:"error_count"`
	FirstSeen        time.Time `json:"first_seen"`
	LastSeen         time.Time `json:"last_seen"`
}

// ModelStats returns per-model aggregates ordered by total tokens (busiest first).
// TokensPerSec is overall completion throughput (completion tokens / total
// generation seconds); AvgTTFTMs averages only requests that recorded a TTFT.
func (s *Store) ModelStats(limit int) ([]ModelStat, error) {
	rows, err := s.db.Query(`
		SELECT
			model,
			COUNT(*)                                                     AS total_requests,
			COALESCE(SUM(prompt_tokens),0)                              AS prompt_tokens,
			COALESCE(SUM(completion_tokens),0)                          AS completion_tokens,
			COALESCE(SUM(total_tokens),0)                               AS total_tokens,
			COALESCE(AVG(duration_ms),0.0)                              AS avg_duration_ms,
			COALESCE(AVG(NULLIF(ttft_ms,0)),0.0)                        AS avg_ttft_ms,
			COALESCE(SUM(completion_tokens) / NULLIF(SUM(duration_ms)/1000.0,0), 0.0) AS tokens_per_sec,
			COALESCE(SUM(cost),0.0)                                     AS cost,
			COALESCE(SUM(CASE WHEN error_message != '' THEN 1 ELSE 0 END),0) AS error_count,
			MIN(timestamp)                                              AS first_seen,
			MAX(timestamp)                                              AS last_seen
		FROM requests
		WHERE model != '' AND model != 'unknown'
		GROUP BY model
		ORDER BY total_tokens DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelStat
	for rows.Next() {
		var m ModelStat
		var firstStr, lastStr string
		if err := rows.Scan(
			&m.Model, &m.TotalRequests,
			&m.PromptTokens, &m.CompletionTokens, &m.TotalTokens,
			&m.AvgDurationMS, &m.AvgTTFTMs, &m.TokensPerSec, &m.Cost, &m.ErrorCount,
			&firstStr, &lastStr,
		); err != nil {
			return nil, err
		}
		m.FirstSeen, _ = time.Parse(time.RFC3339Nano, firstStr)
		m.LastSeen, _ = time.Parse(time.RFC3339Nano, lastStr)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Summary holds overall aggregate statistics.
type Summary struct {
	TotalRequests    int64    `json:"total_requests"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	TotalTokens      int64    `json:"total_tokens"`
	AvgDurationMS    float64  `json:"avg_duration_ms"`
	Cost             float64  `json:"cost"`
	UniqueSessions   int64    `json:"unique_sessions"`
	UniqueModels     []string `json:"unique_models"`
	ErrorCount       int64    `json:"error_count"`
}

// GetSummary returns overall statistics across all recorded requests.
func (s *Store) GetSummary() (Summary, error) {
	var sum Summary
	if err := s.db.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(prompt_tokens),0),
			COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(total_tokens),0),
			COALESCE(AVG(duration_ms),0.0),
			COALESCE(SUM(cost),0.0),
			COUNT(DISTINCT CASE WHEN session_id != '' THEN session_id END),
			COALESCE(SUM(CASE WHEN error_message != '' THEN 1 ELSE 0 END), 0)
		FROM requests`,
	).Scan(
		&sum.TotalRequests, &sum.PromptTokens, &sum.CompletionTokens, &sum.TotalTokens,
		&sum.AvgDurationMS, &sum.Cost, &sum.UniqueSessions, &sum.ErrorCount,
	); err != nil {
		return sum, err
	}

	models, err := s.Models()
	if err != nil {
		return sum, err
	}
	sum.UniqueModels = models
	return sum, nil
}

// Models returns the list of distinct model names that have been seen.
func (s *Store) Models() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT model FROM requests WHERE model != '' ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if out == nil {
		out = []string{}
	}
	return out, rows.Err()
}

// StatusCount is the number of requests recorded under one HTTP status code.
type StatusCount struct {
	StatusCode int   `json:"status_code"`
	Count      int64 `json:"count"`
}

// StatusCounts returns the status-code distribution for the rows matching f,
// commonest first. It backs the dashboard's status facet, so callers pass a
// filter with the status/errors selection cleared — otherwise selecting one
// code would zero out every other chip.
func (s *Store) StatusCounts(f RequestFilter) ([]StatusCount, error) {
	where, args := filterClause(f)
	rows, err := s.db.Query(`
		SELECT status_code, COUNT(*) AS count
		FROM requests WHERE `+where+`
		GROUP BY status_code
		ORDER BY count DESC, status_code ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StatusCount
	for rows.Next() {
		var c StatusCount
		if err := rows.Scan(&c.StatusCode, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SessionUsage is one session's consumption over a time window.
type SessionUsage struct {
	SessionID string  `json:"session_id"`
	Tokens    int64   `json:"tokens"`
	Cost      float64 `json:"cost"`
}

// UsageSince returns per-session token and cost totals for rows at or after the
// given RFC3339 timestamp. It seeds the in-memory budget tracker at startup so a
// restart resumes the day's accounting instead of resetting it.
func (s *Store) UsageSince(since string) ([]SessionUsage, error) {
	rows, err := s.db.Query(`
		SELECT session_id,
		       COALESCE(SUM(total_tokens),0),
		       COALESCE(SUM(cost),0.0)
		FROM requests
		WHERE session_id != '' AND timestamp >= ?
		GROUP BY session_id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionUsage
	for rows.Next() {
		var u SessionUsage
		if err := rows.Scan(&u.SessionID, &u.Tokens, &u.Cost); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RenameModel rewrites every row recorded under from so it reads as to, and
// reports how many rows changed. It backs the one-shot normalization backfill
// that folds historical "llama3" rows into "llama3:latest".
func (s *Store) RenameModel(from, to string) (int64, error) {
	if from == "" || from == to {
		return 0, nil
	}
	res, err := s.db.Exec(`UPDATE requests SET model = ? WHERE model = ?`, to, from)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAll removes every row from the requests table and reclaims space.
func (s *Store) DeleteAll() error {
	_, err := s.db.Exec("DELETE FROM requests")
	if err != nil {
		return err
	}
	_, err = s.db.Exec("VACUUM")
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

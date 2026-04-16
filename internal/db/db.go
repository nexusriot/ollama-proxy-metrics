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
    request_bytes     BIGINT  NOT NULL DEFAULT 0,
    response_bytes    BIGINT  NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT  NOT NULL DEFAULT 0,
    completion_tokens BIGINT  NOT NULL DEFAULT 0,
    total_tokens      BIGINT  NOT NULL DEFAULT 0,
    error_message     TEXT    NOT NULL DEFAULT '',
    client_ip         TEXT    NOT NULL DEFAULT '',
    user_agent        TEXT    NOT NULL DEFAULT '',
    prompt_text       TEXT    NOT NULL DEFAULT '',
    response_text     TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_requests_timestamp  ON requests(timestamp);
CREATE INDEX IF NOT EXISTS idx_requests_session_id ON requests(session_id);
CREATE INDEX IF NOT EXISTS idx_requests_model      ON requests(model);
CREATE INDEX IF NOT EXISTS idx_requests_date       ON requests(substr(timestamp,1,10));
`

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
	// SQLite returns "duplicate column name" when a column already exists – that's fine.
	for _, col := range []string{
		`ALTER TABLE requests ADD COLUMN prompt_text   TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE requests ADD COLUMN response_text TEXT NOT NULL DEFAULT ''`,
	} {
		_, _ = db.Exec(col)
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
	RequestBytes     int64
	ResponseBytes    int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	ErrorMessage     string
	ClientIP         string
	UserAgent        string
	PromptText       string
	ResponseText     string
}

// InsertRequest persists a RequestRecord.
func (s *Store) InsertRequest(r RequestRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO requests (
			request_id, session_id, timestamp, endpoint, method, model, stream,
			status_code, duration_ms, request_bytes, response_bytes,
			prompt_tokens, completion_tokens, total_tokens,
			error_message, client_ip, user_agent,
			prompt_text, response_text
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.RequestID,
		r.SessionID,
		r.Timestamp.UTC().Format(time.RFC3339Nano),
		r.Endpoint,
		r.Method,
		r.Model,
		boolToInt(r.Stream),
		r.StatusCode,
		r.DurationMS,
		r.RequestBytes,
		r.ResponseBytes,
		r.PromptTokens,
		r.CompletionTokens,
		r.TotalTokens,
		r.ErrorMessage,
		r.ClientIP,
		r.UserAgent,
		r.PromptText,
		r.ResponseText,
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
	RequestBytes     int64     `json:"request_bytes"`
	ResponseBytes    int64     `json:"response_bytes"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	ErrorMessage     string    `json:"error_message"`
	ClientIP         string    `json:"client_ip"`
	UserAgent        string    `json:"user_agent"`
	PromptText       string    `json:"prompt_text"`
	ResponseText     string    `json:"response_text"`
}

// ListRequests returns paginated requests, newest first.
// Optional model and sessionID filter by those columns when non-empty.
func (s *Store) ListRequests(limit, offset int, model, sessionID string) (rows []RequestRow, total int, err error) {
	conds := []string{"1=1"}
	args := []interface{}{}

	if model != "" {
		conds = append(conds, "model = ?")
		args = append(args, model)
	}
	if sessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, sessionID)
	}
	where := strings.Join(conds, " AND ")

	if err = s.db.QueryRow("SELECT COUNT(*) FROM requests WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `
		SELECT id, request_id, session_id, timestamp, endpoint, method, model, stream,
		       status_code, duration_ms, request_bytes, response_bytes,
		       prompt_tokens, completion_tokens, total_tokens,
		       error_message, client_ip, user_agent,
		       prompt_text, response_text
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
		var r RequestRow
		var tsStr string
		var streamInt int
		if err = dbRows.Scan(
			&r.ID, &r.RequestID, &r.SessionID, &tsStr,
			&r.Endpoint, &r.Method, &r.Model, &streamInt,
			&r.StatusCode, &r.DurationMS, &r.RequestBytes, &r.ResponseBytes,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
			&r.ErrorMessage, &r.ClientIP, &r.UserAgent,
			&r.PromptText, &r.ResponseText,
		); err != nil {
			return nil, 0, err
		}
		r.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)
		r.Stream = streamInt != 0
		rows = append(rows, r)
	}
	return rows, total, dbRows.Err()
}

// DailyStat holds aggregated statistics for one calendar day.
type DailyStat struct {
	Date             string  `json:"date"`
	TotalRequests    int64   `json:"total_requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	AvgDurationMS    float64 `json:"avg_duration_ms"`
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
			&d.AvgDurationMS, &d.ErrorCount,
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
			&ss.AvgDurationMS, &firstStr, &lastStr,
		); err != nil {
			return nil, err
		}
		ss.FirstSeen, _ = time.Parse(time.RFC3339Nano, firstStr)
		ss.LastSeen, _ = time.Parse(time.RFC3339Nano, lastStr)
		out = append(out, ss)
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
			COUNT(DISTINCT CASE WHEN session_id != '' THEN session_id END),
			COALESCE(SUM(CASE WHEN error_message != '' THEN 1 ELSE 0 END), 0)
		FROM requests`,
	).Scan(
		&sum.TotalRequests, &sum.PromptTokens, &sum.CompletionTokens, &sum.TotalTokens,
		&sum.AvgDurationMS, &sum.UniqueSessions, &sum.ErrorCount,
	); err != nil {
		return sum, err
	}

	rows, err := s.db.Query(`SELECT DISTINCT model FROM requests WHERE model != '' ORDER BY model`)
	if err != nil {
		return sum, err
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return sum, err
		}
		sum.UniqueModels = append(sum.UniqueModels, m)
	}
	if sum.UniqueModels == nil {
		sum.UniqueModels = []string{}
	}
	return sum, rows.Err()
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

// Package api provides the REST API that powers the metrics dashboard frontend.
package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/nexusriot/ollama-proxy-metrics/internal/budget"
	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
	"github.com/nexusriot/ollama-proxy-metrics/internal/events"
	"github.com/nexusriot/ollama-proxy-metrics/internal/pricing"
)

// exportMaxRows caps how many rows a single export request returns.
const exportMaxRows = 100_000

// Handler exposes the admin REST API over a given http.ServeMux prefix.
type Handler struct {
	store   *db.Store
	prices  *pricing.Table
	events  *events.Broker
	budgets *budget.Tracker
}

// Options configures an API Handler. Only Store is required; a nil Prices
// reports an empty table, a nil Events makes the stream endpoint report 503, and
// a nil Budgets reports no ceilings.
type Options struct {
	Store   *db.Store
	Prices  *pricing.Table
	Events  *events.Broker
	Budgets *budget.Tracker
}

// New creates a new API Handler from opts.
func New(opts Options) *Handler {
	if opts.Prices == nil {
		opts.Prices = pricing.Empty()
	}
	return &Handler{
		store:   opts.Store,
		prices:  opts.Prices,
		events:  opts.Events,
		budgets: opts.Budgets,
	}
}

// Register mounts all API routes under mux at the given prefix (e.g. "/admin/api").
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc(prefix+"/summary", corsMiddleware(h.handleSummary))
	mux.HandleFunc(prefix+"/requests", corsMiddleware(h.handleRequests))
	mux.HandleFunc(prefix+"/daily", corsMiddleware(h.handleDaily))
	mux.HandleFunc(prefix+"/sessions", corsMiddleware(h.handleSessions))
	mux.HandleFunc(prefix+"/models", corsMiddleware(h.handleModels))
	mux.HandleFunc(prefix+"/model-stats", corsMiddleware(h.handleModelStats))
	mux.HandleFunc(prefix+"/status-counts", corsMiddleware(h.handleStatusCounts))
	mux.HandleFunc(prefix+"/budgets", corsMiddleware(h.handleBudgets))
	mux.HandleFunc(prefix+"/pricing", corsMiddleware(h.handlePricing))
	mux.HandleFunc(prefix+"/export", corsMiddleware(h.handleExport))
	mux.HandleFunc(prefix+"/cleanup", corsMiddleware(h.handleCleanup))
	mux.HandleFunc(prefix+"/stream", corsMiddleware(h.handleStream))
}

// corsMiddleware adds CORS headers to support the React dev-server.
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// writeJSON encodes v as JSON and writes it with appropriate headers.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	sum, err := h.store.GetSummary()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

type requestsResponse struct {
	Data   []db.RequestRow `json:"data"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

func (h *Handler) handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	limit := queryInt(q.Get("limit"), 50)
	offset := queryInt(q.Get("offset"), 0)

	if limit < 1 || limit > 500 {
		limit = 50
	}

	rows, total, err := h.store.ListRequestsFiltered(limit, offset, requestFilter(q))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []db.RequestRow{}
	}
	writeJSON(w, http.StatusOK, requestsResponse{
		Data:   rows,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

func (h *Handler) handleDaily(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	days := queryInt(r.URL.Query().Get("days"), 30)
	if days < 1 || days > 365 {
		days = 30
	}
	stats, err := h.store.DailyStats(days)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if stats == nil {
		stats = []db.DailyStat{}
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *Handler) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	limit := queryInt(r.URL.Query().Get("limit"), 50)
	if limit < 1 || limit > 200 {
		limit = 50
	}
	stats, err := h.store.SessionStats(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if stats == nil {
		stats = []db.SessionStat{}
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	models, err := h.store.Models()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, models)
}

func (h *Handler) handleModelStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	limit := queryInt(r.URL.Query().Get("limit"), 100)
	if limit < 1 || limit > 500 {
		limit = 100
	}
	stats, err := h.store.ModelStats(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if stats == nil {
		stats = []db.ModelStat{}
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleStatusCounts reports the status-code distribution for the current
// filters. The status and errors-only selections are deliberately dropped from
// the filter: a facet has to keep showing the codes you are not looking at.
func (h *Handler) handleStatusCounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	f := requestFilter(r.URL.Query())
	f.Status = 0
	f.ErrorsOnly = false

	counts, err := h.store.StatusCounts(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if counts == nil {
		counts = []db.StatusCount{}
	}
	writeJSON(w, http.StatusOK, counts)
}

// budgetsResponse reports the configured ceilings alongside today's usage.
type budgetsResponse struct {
	Enabled bool                    `json:"enabled"`
	Day     string                  `json:"day"`
	Limits  budget.Limits           `json:"limits"`
	Usage   map[string]budget.Usage `json:"usage"`
}

func (h *Handler) handleBudgets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	day, usage := h.budgets.Snapshot()
	writeJSON(w, http.StatusOK, budgetsResponse{
		Enabled: h.budgets.Enabled(),
		Day:     day,
		Limits:  h.budgets.Limits(),
		Usage:   usage,
	})
}

func (h *Handler) handlePricing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, h.prices)
}

func (h *Handler) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if err := h.store.DeleteAll(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleExport streams all matching requests as CSV (default) or JSON for download.
func (h *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	format := q.Get("format")

	rows, err := h.store.ExportRequestsFiltered(exportMaxRows, requestFilter(q))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="requests.json"`)
		_ = json.NewEncoder(w).Encode(rows)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="requests.csv"`)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"request_id", "session_id", "timestamp", "endpoint", "method", "model",
		"stream", "status_code", "duration_ms", "ttft_ms", "request_bytes",
		"response_bytes", "prompt_tokens", "completion_tokens", "total_tokens",
		"cost", "tokens_estimated", "cached", "error_message", "client_ip", "user_agent",
	})
	for _, r := range rows {
		_ = cw.Write([]string{
			r.RequestID, r.SessionID, r.Timestamp.Format(time.RFC3339Nano),
			r.Endpoint, r.Method, r.Model, strconv.FormatBool(r.Stream),
			strconv.Itoa(r.StatusCode), strconv.FormatInt(r.DurationMS, 10),
			strconv.FormatInt(r.TTFTMs, 10), strconv.FormatInt(r.RequestBytes, 10),
			strconv.FormatInt(r.ResponseBytes, 10), strconv.FormatInt(r.PromptTokens, 10),
			strconv.FormatInt(r.CompletionTokens, 10), strconv.FormatInt(r.TotalTokens, 10),
			strconv.FormatFloat(r.Cost, 'f', -1, 64),
			strconv.FormatBool(r.TokensEstimated), strconv.FormatBool(r.Cached),
			r.ErrorMessage, r.ClientIP, r.UserAgent,
		})
	}
}

// handleStream is a Server-Sent Events endpoint that pushes each newly recorded
// request to the client as it happens.
func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	if h.events == nil {
		writeError(w, http.StatusServiceUnavailable, "live stream not enabled")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch, cancel := h.events.Subscribe()
	defer cancel()

	// Initial comment so the client sees the connection open immediately.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// Heartbeat keeps idle connections alive through proxies/load balancers that
	// would otherwise drop a quiet stream.
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

// requestFilter builds a db.RequestFilter from the query parameters shared by
// the requests and export endpoints (model, session, q, since, until, status,
// errors).
func requestFilter(q url.Values) db.RequestFilter {
	return db.RequestFilter{
		Model:      q.Get("model"),
		Session:    q.Get("session"),
		Query:      q.Get("q"),
		Since:      q.Get("since"),
		Until:      q.Get("until"),
		Status:     queryInt(q.Get("status"), 0),
		ErrorsOnly: queryBool(q.Get("errors")),
	}
}

// queryBool reads a boolean query parameter, treating anything unparseable as
// false so a malformed filter widens the result set rather than hiding rows.
func queryBool(s string) bool {
	if s == "" {
		return false
	}
	v, err := strconv.ParseBool(s)
	return err == nil && v
}

func queryInt(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

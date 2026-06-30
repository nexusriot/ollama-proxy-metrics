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

	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
	"github.com/nexusriot/ollama-proxy-metrics/internal/events"
	"github.com/nexusriot/ollama-proxy-metrics/internal/pricing"
)

// exportMaxRows caps how many rows a single export request returns.
const exportMaxRows = 100_000

// Handler exposes the admin REST API over a given http.ServeMux prefix.
type Handler struct {
	store  *db.Store
	prices *pricing.Table
	events *events.Broker
}

// New creates a new API Handler backed by store. prices and broker may be nil
// (the pricing endpoint reports an empty table; the stream endpoint reports 503).
func New(store *db.Store, prices *pricing.Table, broker *events.Broker) *Handler {
	if prices == nil {
		prices = pricing.Empty()
	}
	return &Handler{store: store, prices: prices, events: broker}
}

// Register mounts all API routes under mux at the given prefix (e.g. "/admin/api").
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc(prefix+"/summary", corsMiddleware(h.handleSummary))
	mux.HandleFunc(prefix+"/requests", corsMiddleware(h.handleRequests))
	mux.HandleFunc(prefix+"/daily", corsMiddleware(h.handleDaily))
	mux.HandleFunc(prefix+"/sessions", corsMiddleware(h.handleSessions))
	mux.HandleFunc(prefix+"/models", corsMiddleware(h.handleModels))
	mux.HandleFunc(prefix+"/model-stats", corsMiddleware(h.handleModelStats))
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
		"cost", "error_message", "client_ip", "user_agent",
	})
	for _, r := range rows {
		_ = cw.Write([]string{
			r.RequestID, r.SessionID, r.Timestamp.Format(time.RFC3339Nano),
			r.Endpoint, r.Method, r.Model, strconv.FormatBool(r.Stream),
			strconv.Itoa(r.StatusCode), strconv.FormatInt(r.DurationMS, 10),
			strconv.FormatInt(r.TTFTMs, 10), strconv.FormatInt(r.RequestBytes, 10),
			strconv.FormatInt(r.ResponseBytes, 10), strconv.FormatInt(r.PromptTokens, 10),
			strconv.FormatInt(r.CompletionTokens, 10), strconv.FormatInt(r.TotalTokens, 10),
			strconv.FormatFloat(r.Cost, 'f', -1, 64), r.ErrorMessage, r.ClientIP, r.UserAgent,
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
// the requests and export endpoints (model, session, q, since, until).
func requestFilter(q url.Values) db.RequestFilter {
	return db.RequestFilter{
		Model:   q.Get("model"),
		Session: q.Get("session"),
		Query:   q.Get("q"),
		Since:   q.Get("since"),
		Until:   q.Get("until"),
	}
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

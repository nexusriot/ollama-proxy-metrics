// Package api provides the REST API that powers the metrics dashboard frontend.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/nexusriot/ollama-proxy-metrics/internal/db"
)

// Handler exposes the admin REST API over a given http.ServeMux prefix.
type Handler struct {
	store *db.Store
}

// New creates a new API Handler backed by store.
func New(store *db.Store) *Handler {
	return &Handler{store: store}
}

// Register mounts all API routes under mux at the given prefix (e.g. "/admin/api").
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc(prefix+"/summary", corsMiddleware(h.handleSummary))
	mux.HandleFunc(prefix+"/requests", corsMiddleware(h.handleRequests))
	mux.HandleFunc(prefix+"/daily", corsMiddleware(h.handleDaily))
	mux.HandleFunc(prefix+"/sessions", corsMiddleware(h.handleSessions))
	mux.HandleFunc(prefix+"/models", corsMiddleware(h.handleModels))
	mux.HandleFunc(prefix+"/cleanup", corsMiddleware(h.handleCleanup))
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
	model := q.Get("model")
	sessionID := q.Get("session")

	if limit < 1 || limit > 500 {
		limit = 50
	}

	rows, total, err := h.store.ListRequests(limit, offset, model, sessionID)
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

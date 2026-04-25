package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON serialises v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writeJSON encode error", "err", err)
	}
}

// errorResponse is the standard error envelope for JSON API endpoints.
type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// writeError writes a standard JSON error response.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: msg, Code: code})
}

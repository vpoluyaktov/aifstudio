package server

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"aifstudio/internal/ifdb"
)

var ifdbIDRE = regexp.MustCompile(`^[a-z0-9]{10,32}$`)

func (s *Server) handleIFDBSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "missing_query", "q parameter is required and must not be empty")
		return
	}
	if len(q) > 200 {
		writeError(w, http.StatusBadRequest, "query_too_long", "q must be 1-200 characters")
		return
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 25
	if limitStr != "" {
		v, err := strconv.Atoi(limitStr)
		if err == nil {
			limit = v
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}

	clientIP := ifdb.ExtractClientIP(r.Header.Get("X-Forwarded-For"), r.RemoteAddr)
	result, cacheHit, err := s.ifdbClient.Search(r.Context(), clientIP, q, limit)
	if err != nil {
		handleIFDBError(w, err)
		return
	}

	if cacheHit {
		w.Header().Set("X-Cache", "HIT")
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleIFDBGame(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ifdbIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^[a-z0-9]{10,32}$")
		return
	}

	clientIP := ifdb.ExtractClientIP(r.Header.Get("X-Forwarded-For"), r.RemoteAddr)
	game, cacheHit, err := s.ifdbClient.GetGame(r.Context(), clientIP, id)
	if err != nil {
		handleIFDBError(w, err)
		return
	}

	if cacheHit {
		w.Header().Set("X-Cache", "HIT")
	}
	writeJSON(w, http.StatusOK, game)
}

func handleIFDBError(w http.ResponseWriter, err error) {
	var rl ifdb.ErrRateLimit
	var up ifdb.ErrUpstream
	var nf ifdb.ErrNotFound

	switch {
	case errors.As(err, &rl):
		writeError(w, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
	case errors.As(err, &nf):
		writeError(w, http.StatusNotFound, "not_found", "game not found")
	case errors.As(err, &up):
		if strings.Contains(up.Msg, "malformed") {
			writeError(w, http.StatusServiceUnavailable, "upstream_invalid", "ifdb upstream malformed response")
		} else {
			writeError(w, http.StatusServiceUnavailable, "upstream_error", up.Msg)
		}
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

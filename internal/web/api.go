package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/olafkfreund/siphon/internal/store"
)

func (s *server) apiRoutes(mux *http.ServeMux) {
	get := func(path string, f func(r *http.Request) (any, int, error)) {
		mux.HandleFunc("GET "+path, s.api(func(w http.ResponseWriter, r *http.Request) { s.reply(w, r, f) }))
	}
	post := func(path string, f func(r *http.Request) (any, int, error)) {
		mux.HandleFunc("POST "+path, s.api(func(w http.ResponseWriter, r *http.Request) { s.reply(w, r, f) }))
	}
	get("/api/sources", func(*http.Request) (any, int, error) { v, err := s.sources(); return v, 200, err })
	get("/api/rules", func(*http.Request) (any, int, error) { v, err := s.rules(); return v, 200, err })
	get("/api/jobs", func(r *http.Request) (any, int, error) {
		v, err := store.QueryJobs(s.Store.DB, r.URL.Query().Get("state"), limit(r))
		if v == nil {
			v = []store.JobRow{}
		}
		return v, 200, err
	})
	get("/api/jobs/{id}", func(r *http.Request) (any, int, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return nil, 400, errMsg("bad job id")
		}
		j, ok, err := s.job(id)
		if err == nil && !ok {
			return nil, 404, errMsg("no such job")
		}
		return j, 200, err
	})
	get("/api/approvals", func(*http.Request) (any, int, error) {
		v, err := store.PendingApprovals(s.Store.DB)
		if v == nil {
			v = []store.PendingApproval{}
		}
		return v, 200, err
	})
	get("/api/audit", func(r *http.Request) (any, int, error) {
		v, err := store.ListAudit(s.Store.DB, limit(r))
		if v == nil {
			v = []store.AuditRow{}
		}
		return v, 200, err
	})
	post("/api/jobs/{id}/{verb}", func(r *http.Request) (any, int, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		verb := r.PathValue("verb")
		if err != nil || (verb != "approve" && verb != "deny") {
			return nil, 404, errMsg("not found")
		}
		if err := s.Decide(id, verb == "approve", "api"); err != nil {
			code, msg := decisionError(err)
			return nil, code, errMsg(msg)
		}
		return map[string]bool{"ok": true}, 200, nil
	})
	post("/api/rules/{name}/{verb}", func(r *http.Request) (any, int, error) {
		name, verb := r.PathValue("name"), r.PathValue("verb")
		if (verb != "enable" && verb != "disable") || !s.hasRule(name) {
			return nil, 404, errMsg("not found")
		}
		err := store.SetRuleOverride(s.Store.DB, name, verb == "enable", "api", s.Now())
		return map[string]bool{"ok": true}, 200, err
	})
}

// decisionError maps a Decide error to a status and a fixed client message;
// anything unexpected is logged and reported as a plain 500.
func decisionError(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 404, "not found"
	case errors.Is(err, store.ErrNotPending):
		return 409, "already decided, expired or not pending"
	}
	slog.Error("decide", "err", err)
	return 500, "internal error"
}

type errMsg string

func (e errMsg) Error() string { return string(e) }

func limit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n < 1 {
		return 100
	}
	return min(n, 500)
}

// api wraps a handler with bearer auth and the failed-auth rate limit.
func (s *server) api(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if s.lim.blocked(ip) {
			http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !s.tokenOK(tok) {
			s.lim.fail(ip)
			w.Header().Set("WWW-Authenticate", `Bearer realm="siphon"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *server) reply(w http.ResponseWriter, r *http.Request, f func(*http.Request) (any, int, error)) {
	v, code, err := f(r)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		msg, ok := err.(errMsg) // only our fixed messages reach clients
		if !ok {
			slog.Error("api", "path", r.URL.Path, "err", err)
			code, msg = 500, "internal error"
		}
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": string(msg)})
		return
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

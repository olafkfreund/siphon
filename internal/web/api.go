package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/olafkfreund/siphon/internal/store"
)

func (s *server) apiRoutes(mux *http.ServeMux) {
	get := func(path string, f func(r *http.Request) (any, int, error)) {
		mux.HandleFunc("GET "+path, s.api(func(w http.ResponseWriter, r *http.Request) { s.reply(w, r, f) }))
	}
	post := func(path string, f func(r *http.Request) (any, int, error)) {
		mux.HandleFunc("POST "+path, s.api(func(w http.ResponseWriter, r *http.Request) { s.reply(w, r, f) }))
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) { jsonErr(w, http.StatusNotFound, "not found") })
	mux.HandleFunc("GET /metrics", s.bearer(s.metricsOK, s.metrics))
	s.connectionAPI(mux)
	s.diagAPI(mux)
	s.inventoryAPI(mux)
	s.draftAPI(mux)
	s.notifyAPI(mux)
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
		q := r.URL.Query()
		f := store.AuditFilter{Rule: q.Get("rule"), Event: q.Get("event"), Limit: limit(r)}
		if v := q.Get("since"); v != "" { // a time (RFC 3339) or how long ago (1h)
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				f.Since = t
			} else if d, err := time.ParseDuration(v); err == nil && d > 0 {
				f.Since = s.Now().Add(-d)
			} else {
				return nil, 400, errMsg("bad since: want a time like 2026-01-02T15:04:05Z or a duration like 1h")
			}
		}
		v, err := store.QueryAudit(s.Store.DB, f)
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
		actor, aerr := apiActor(r)
		if aerr != nil {
			return nil, 400, aerr
		}
		if err := s.Decide(id, verb == "approve", actor); err != nil {
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
		actor, aerr := apiActor(r)
		if aerr != nil {
			return nil, 400, aerr
		}
		err := store.SetRuleOverride(s.Store.DB, name, verb == "enable", actor, s.Now())
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

// api wraps a handler with admin bearer auth and the failed-auth rate limit.
func (s *server) api(h http.HandlerFunc) http.HandlerFunc { return s.bearer(s.tokenOK, h) }

func (s *server) bearer(ok func(string) bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)
		if s.lim.blocked(ip) {
			jsonErr(w, http.StatusTooManyRequests, "too many failed attempts")
			return
		}
		tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || !ok(tok) {
			s.lim.fail(ip)
			w.Header().Set("WWW-Authenticate", `Bearer realm="siphon"`)
			jsonErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	}
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// actorLabel is what a caller may call itself in the audit trail.
var actorLabel = regexp.MustCompile(`^[\w.@:-]{1,64}$`)

// apiActor is "api", or "api:<label>" from X-Siphon-Actor.
func apiActor(r *http.Request) (string, error) {
	l := r.Header.Get("X-Siphon-Actor")
	if l == "" {
		return "api", nil
	}
	if !actorLabel.MatchString(l) {
		return "", errMsg("bad X-Siphon-Actor")
	}
	return "api:" + l, nil
}

func (s *server) reply(w http.ResponseWriter, r *http.Request, f func(*http.Request) (any, int, error)) {
	v, code, err := f(r)
	w.Header().Set("Content-Type", "application/json")
	var inv errInvalid
	if errors.As(err, &inv) { // a config the checks refuse: every problem, as a list
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{"error": inv.msg, "errors": inv.list(), "warnings": []string{}})
		return
	}
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

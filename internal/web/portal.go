package web

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/olafkfreund/siphon/internal/store"
)

// view is the data for every portal partial; only the fields a page uses are set.
type view struct {
	CSRF      string
	State     string
	Sources   []sourceView
	Rules     []ruleView
	Jobs      []store.JobRow
	Job       store.JobDetail
	Approvals []store.PendingApproval
	Audit     []store.AuditRow
}

type layout struct {
	Title, Path, CSRF string
	Body              template.HTML
}

func (s *server) portalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) { s.render(w, "login", "") })
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/jobs", http.StatusSeeOther) })

	page := func(path, name string, fill func(r *http.Request, v *view) (bool, error)) {
		mux.HandleFunc("GET "+path, s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
			v := view{CSRF: csrf}
			found, err := fill(r, &v)
			switch {
			case err != nil:
				slog.Error("portal page", "path", r.URL.Path, "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			case !found:
				http.NotFound(w, r)
			default:
				s.page(w, r, name, v)
			}
		}))
	}
	page("/sources", "sources", func(_ *http.Request, v *view) (ok bool, err error) { v.Sources, err = s.sources(); return true, err })
	page("/rules", "rules", func(_ *http.Request, v *view) (ok bool, err error) { v.Rules, err = s.rules(); return true, err })
	page("/jobs", "jobs", func(r *http.Request, v *view) (ok bool, err error) {
		v.State = r.URL.Query().Get("state")
		v.Jobs, err = store.QueryJobs(s.Store.DB, v.State, 200)
		return true, err
	})
	page("/jobs/{id}", "job", func(r *http.Request, v *view) (bool, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return false, nil
		}
		j, ok, err := s.job(id)
		v.Job = j
		return ok, err
	})
	page("/approvals", "approvals", func(_ *http.Request, v *view) (ok bool, err error) {
		v.Approvals, err = store.PendingApprovals(s.Store.DB)
		return true, err
	})
	page("/audit", "audit", func(_ *http.Request, v *view) (ok bool, err error) {
		v.Audit, err = store.ListAudit(s.Store.DB, 200)
		return true, err
	})

	mux.HandleFunc("POST /logout", s.portal(func(w http.ResponseWriter, r *http.Request, _ string) {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureCookie(r)})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /rules/{name}/{verb}", s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
		name, verb := r.PathValue("name"), r.PathValue("verb")
		if (verb != "enable" && verb != "disable") || !s.hasRule(name) {
			http.NotFound(w, r)
			return
		}
		if err := store.SetRuleOverride(s.Store.DB, name, verb == "enable", "portal", s.Now()); err != nil {
			slog.Error("rule override", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		v := view{CSRF: csrf}
		v.Rules, _ = s.rules()
		s.afterPost(w, r, "/rules", "rules", v)
	}))
	mux.HandleFunc("POST /approvals/{id}/{verb}", s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		verb := r.PathValue("verb")
		if err != nil || (verb != "approve" && verb != "deny") {
			http.NotFound(w, r)
			return
		}
		if err := s.Decide(id, verb == "approve", "portal"); err != nil {
			code, msg := decisionError(err)
			http.Error(w, msg, code)
			return
		}
		v := view{CSRF: csrf}
		v.Approvals, _ = store.PendingApprovals(s.Store.DB)
		s.afterPost(w, r, "/approvals", "approvals", v)
	}))
}

// afterPost answers an htmx POST with the refreshed partial, a plain form POST with a redirect.
func (s *server) afterPost(w http.ResponseWriter, r *http.Request, path, name string, v view) {
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, name, v)
		return
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// page renders a partial for htmx requests, or wraps it in the full layout.
func (s *server) page(w http.ResponseWriter, r *http.Request, name string, v view) {
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, name, v)
		return
	}
	var body bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&body, name, v); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	s.render(w, "layout", layout{Title: name, Path: r.URL.RequestURI(), CSRF: v.CSRF, Body: template.HTML(body.String())})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.lim.blocked(ip) {
		http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !s.tokenOK(r.PostFormValue("token")) {
		s.lim.fail(ip)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login", "Invalid token")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.sessionValue(), Path: "/", MaxAge: 86400,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureCookie(r)})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// portal requires a valid session cookie, and a CSRF token on every POST.
func (s *server) portal(h func(w http.ResponseWriter, r *http.Request, csrf string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || !s.sessionValid(c.Value) {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		csrf := s.csrfFor(c.Value)
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			got := r.Header.Get("X-CSRF-Token")
			if got == "" {
				got = r.PostFormValue("csrf")
			}
			if !eq(got, csrf) {
				http.Error(w, "bad csrf token", http.StatusForbidden)
				return
			}
		}
		h(w, r, csrf)
	}
}

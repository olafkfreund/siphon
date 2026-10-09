package web

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/olafkfreund/siphon/internal/mcpoauth"
)

// oauthSource reports whether name is a source with auth.oauth (and logins are on).
func (s *server) oauthSource(name string) bool {
	src := s.Config().Sources[name]
	return s.OAuth != nil && src != nil && src.Auth != nil && src.Auth.OAuth != nil
}

// startLogin begins a login and audits it (source name and action only).
func (s *server) startLogin(r *http.Request, actor, name string) (string, int, error) {
	u, err := s.OAuth.Start(r.Context(), name)
	if err != nil { // mcpoauth cleans its errors: no token reaches them
		return "", http.StatusBadGateway, errMsg("OAuth login did not start: " + err.Error())
	}
	if pu, err := url.Parse(u); err != nil || (pu.Scheme != "https" && pu.Scheme != "http") {
		return "", http.StatusBadGateway, errMsg("bad authorization URL")
	}
	s.audit(actor, "oauth_login_started", name)
	return u, http.StatusOK, nil
}

func (s *server) oauthRoutes(mux *http.ServeMux) {
	// API (admin token).
	mux.HandleFunc("POST /api/sources/{name}/oauth/login", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, r, func(r *http.Request) (any, int, error) {
			name := r.PathValue("name")
			if !s.oauthSource(name) {
				return nil, 404, errMsg("no such OAuth source")
			}
			actor, err := apiActor(r)
			if err != nil {
				return nil, 400, err
			}
			u, code, err := s.startLogin(r, actor, name)
			if err != nil {
				return nil, code, err
			}
			return map[string]string{"url": u}, 200, nil
		})
	}))
	mux.HandleFunc("GET /api/sources/{name}/oauth", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, r, func(r *http.Request) (any, int, error) {
			name := r.PathValue("name")
			if !s.oauthSource(name) {
				return nil, 404, errMsg("no such OAuth source")
			}
			st, exp := s.OAuth.Status(name)
			out := map[string]any{"status": st, "expiry": (*time.Time)(nil)}
			if !exp.IsZero() {
				out["expiry"] = exp
			}
			return out, 200, nil
		})
	}))
	mux.HandleFunc("DELETE /api/sources/{name}/oauth", s.api(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, r, func(r *http.Request) (any, int, error) {
			name := r.PathValue("name")
			if !s.oauthSource(name) {
				return nil, 404, errMsg("no such OAuth source")
			}
			actor, err := apiActor(r)
			if err != nil {
				return nil, 400, err
			}
			if err := s.OAuth.Logout(name); err != nil {
				return nil, 500, err
			}
			s.audit(actor, "oauth_logout", name)
			return map[string]string{"status": "none"}, 200, nil
		})
	}))

	// Portal: the Log in button.
	mux.HandleFunc("POST /sources/{name}/oauth", s.portal(func(w http.ResponseWriter, r *http.Request, _ string) {
		name := r.PathValue("name")
		if !s.oauthSource(name) {
			http.NotFound(w, r)
			return
		}
		u, code, err := s.startLogin(r, "portal", name)
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		http.Redirect(w, r, u, http.StatusSeeOther)
	}))

	// The browser comes back here. Public: the single-use state is the proof.
	mux.HandleFunc("GET /oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if s.lim.blocked(ip) { // before any lookup
			http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}
		if s.OAuth == nil {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		src, err := s.OAuth.Complete(q.Get("state"), q.Get("code"), q.Get("iss"), q.Get("error"))
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			s.lim.fail(ip)
			msg, code := err.Error(), http.StatusBadRequest
			if errors.Is(err, mcpoauth.ErrUnknownLogin) {
				msg = "unknown or expired login"
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(code)
			s.render(w, "oauth", map[string]string{"Err": msg})
			return
		}
		s.render(w, "oauth", map[string]string{"Source": src})
	})
}

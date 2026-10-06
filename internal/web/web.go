// Package web serves the JSON API (/api, bearer token) and the htmx portal
// (cookie session + CSRF) over one http.ServeMux.
package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/olafkfreund/MCP-AgentGateway/internal/config"
	"github.com/olafkfreund/MCP-AgentGateway/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Options struct {
	Token string // bearer token and portal login; empty locks everyone out
	Store *store.Store
	Cfg   *config.Config
	// Decide approves or denies a pending job (Pipeline.Decide). Nil uses the store directly.
	Decide func(jobID int64, approve bool, by string) error
	// Hooks are mounted at POST /hook/{source}, unauthenticated: HMAC is their auth.
	Hooks map[string]http.Handler
	Now   func() time.Time
}

type server struct {
	Options
	key     []byte // per-process; a restart logs everyone out
	tokHash [32]byte
	lim     *limiter
	tpl     *template.Template
}

const (
	cookieName = "agentgw_session"
	failBurst  = 5 // bad logins / API auth failures per IP per minute
)

func New(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &server{Options: o, key: make([]byte, 32), tokHash: sha256.Sum256([]byte(o.Token)), lim: &limiter{now: o.Now, m: map[string]*bucket{}}}
	if _, err := rand.Read(s.key); err != nil {
		panic(err)
	}
	if s.Decide == nil {
		s.Decide = func(id int64, approve bool, by string) error {
			return store.DecideApproval(s.Store.DB, id, approve, by, "", s.Now())
		}
	}
	s.tpl = template.Must(template.New("").Funcs(template.FuncMap{
		"tm": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
		"tp": func(t *time.Time) string {
			if t == nil {
				return "-"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"exit": func(e *int) string {
			if e == nil {
				return "-"
			}
			return itoa(*e)
		},
		"pe": url.PathEscape,
	}).ParseFS(assets, "templates/*.html"))

	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("POST /hook/{source}", func(w http.ResponseWriter, r *http.Request) {
		if h := s.Hooks[r.PathValue("source")]; h != nil {
			h.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
	s.apiRoutes(mux)
	s.portalRoutes(mux)
	return secure(mux)
}

func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

func (s *server) tokenOK(t string) bool {
	if s.Token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(t))
	return subtle.ConstantTimeCompare(sum[:], s.tokHash[:]) == 1
}

func (s *server) mac(msg string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// The cookie is an HMAC over the token under a random key: the raw token never reaches the browser.
func (s *server) sessionValue() string          { return s.mac("session:" + s.Token) }
func (s *server) csrfFor(session string) string { return s.mac("csrf:" + session) }

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h // ponytail: X-Forwarded-For is not trusted; behind a proxy all clients share one bucket
}

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// limiter is a per-IP token bucket for failed authentications.
type limiter struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func (l *limiter) refill(b *bucket) {
	n := l.now()
	b.tokens = min(failBurst, b.tokens+n.Sub(b.at).Minutes()*failBurst)
	b.at = n
}

func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[ip]
	if b == nil {
		return false
	}
	l.refill(b)
	return b.tokens < 1
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > 4096 { // drop fully refilled entries
		for k, b := range l.m {
			if l.refill(b); b.tokens >= failBurst {
				delete(l.m, k)
			}
		}
	}
	b := l.m[ip]
	if b == nil {
		b = &bucket{tokens: failBurst, at: l.now()}
		l.m[ip] = b
	}
	l.refill(b)
	b.tokens--
}

func secureCookie(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

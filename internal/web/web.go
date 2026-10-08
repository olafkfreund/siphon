// Package web serves the JSON API (/api, bearer token) and the htmx portal
// (cookie session + CSRF) over one http.ServeMux.
package web

import (
	"bytes"
	"context"
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
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Options struct {
	Token string // bearer token and portal login; empty locks everyone out
	Store *store.Store
	Cfg   *config.Config // fixed config; used only when Config is nil (tests)
	// Config returns the live config (Pipeline.Config); Apply makes a new one live (Pipeline.Apply).
	Config func() *config.Config
	Apply  func(*config.Config) error
	// ConfigPath is the config file the portal edits are layered on; empty disables editing.
	ConfigPath  string
	Unsandboxed bool   // permanent banner: sandbox is none
	Banner      string // shown in red on every page (startup fallback notice)
	// Decide approves or denies a pending job (Pipeline.Decide). Nil uses the store directly.
	Decide func(jobID int64, approve bool, by string) error
	// TestAWS checks an AWS source: identity, key expiry and the bridged server's tools
	// (Pipeline.TestAWS). Nil disables the Test button for AWS sources.
	TestAWS func(ctx context.Context, source string) (arn string, expires time.Time, tools []string, err error)
	// Hooks are mounted at POST /hook/{source}, unauthenticated: HMAC is their auth.
	Hooks func(source string) http.Handler // nil result = not a webhook source
	Now   func() time.Time
	// Version is the build version shown by /metrics.
	Version string
}

type server struct {
	Options
	key     []byte // per-process; a restart logs everyone out
	tokHash [32]byte
	lim     *limiter
	tpl     *template.Template
	editMu  sync.Mutex             // serialises config saves
	notice  atomic.Pointer[string] // set when a save could not be applied live
}

const (
	cookieName = "siphon_session"
	failBurst  = 5 // bad logins / API auth failures per IP per minute
	maxBuckets = 4096
)

func New(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &server{Options: o, key: make([]byte, 32), tokHash: sha256.Sum256([]byte(o.Token)), lim: &limiter{now: o.Now, m: map[string]*bucket{}}}
	if _, err := rand.Read(s.key); err != nil {
		panic(err)
	}
	if s.Config == nil {
		cfg := o.Cfg
		s.Config = func() *config.Config { return cfg }
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
		"pe":    url.PathEscape,
		"has":   func(l []string, s string) bool { return slices.Contains(l, s) },
		"ticks": ticks,
		"lines": func(s string) []string {
			var out []string
			for _, l := range strings.Split(s, "\n") {
				if l = strings.TrimSpace(l); l != "" {
					out = append(out, l)
				}
			}
			return out
		},
		"hasprefix": strings.HasPrefix,
		"tools":     toolsHint,
		"kindtitle": func(k string) string { return kindTitle[k] },
		"kindone":   func(k string) string { return kindOne[k] },
		"prov":      func(p string) string { return provLabel[p] },
		"diffhtml":  diffHTML,
		"dur": func(d config.Duration) string {
			if d == 0 {
				return "no timeout"
			}
			return time.Duration(d).String() + " timeout"
		},
		"ago": func(t time.Time) string { return ago(o.Now(), t) },
		// zoned shows a time in an IANA zone: "2026-03-01 06:00 Europe/London".
		"zoned": func(t *time.Time, zone string) string {
			if t == nil {
				return "-"
			}
			if loc, err := time.LoadLocation(zone); err == nil {
				return t.In(loc).Format("2006-01-02 15:04") + " " + zone
			}
			return t.Format("2006-01-02 15:04 MST")
		},
		"agop": func(t *time.Time) string {
			if t == nil {
				return "never"
			}
			return ago(o.Now(), *t)
		},
	}).ParseFS(assets, "templates/*.html"))

	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("POST /hook/{source}", func(w http.ResponseWriter, r *http.Request) {
		if h := s.hook(r.PathValue("source")); h != nil {
			h.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
	s.apiRoutes(mux)
	s.portalRoutes(mux)
	s.configRoutes(mux)
	s.helpRoutes(mux)
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
// Cookie value: "<issue unix seconds>|<HMAC(key, "session:"+ts)>", valid for 24 h.
// There is no server-side session store, so logout only clears the browser's
// cookie; the value itself stays valid until it expires or the process restarts.
const sessionTTL = 24 * time.Hour

func (s *server) sessionValue() string {
	ts := strconv.FormatInt(s.Now().Unix(), 10)
	return ts + "|" + s.mac("session:"+ts)
}

func (s *server) sessionValid(v string) bool {
	ts, sig, ok := strings.Cut(v, "|")
	if !ok || s.Token == "" || !eq(sig, s.mac("session:"+ts)) {
		return false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	age := s.Now().Sub(time.Unix(n, 0))
	return err == nil && age >= -time.Minute && age < sessionTTL
}

func (s *server) csrfFor(session string) string { return s.mac("csrf:" + session) }

// clientIP is the limiter key: the IPv4 address, or the /64 prefix of an IPv6
// one (a single host controls a whole /64).
// ponytail: X-Forwarded-For is not trusted; behind a proxy all clients share one bucket.
func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		h = r.RemoteAddr
	}
	a, err := netip.ParseAddr(h)
	if err != nil {
		return h
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
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
	if len(l.m) > maxBuckets { // ponytail: forgets all penalties; beats sweeping under the lock
		l.m = map[string]*bucket{}
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

func (s *server) hook(name string) http.Handler {
	if s.Hooks == nil {
		return nil
	}
	return s.Hooks(name)
}

var tickRe = regexp.MustCompile("`([^`]+)`")

// ticks escapes text and renders `code` spans.
func ticks(s string) template.HTML {
	return template.HTML(tickRe.ReplaceAllString(template.HTMLEscapeString(s), "<code>$1</code>"))
}

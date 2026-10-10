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
	"encoding/base64"
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

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/mcpoauth"
	"github.com/olafkfreund/siphon/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Options struct {
	Token string // bearer token and portal login; empty locks everyone out
	// MetricsToken opens /metrics only; empty: admin token only.
	MetricsToken string
	Store        *store.Store
	Cfg          *config.Config // fixed config; used only when Config is nil (tests)
	// Config returns the live config (Pipeline.Config); Apply makes a new one live (Pipeline.Apply).
	Config func() *config.Config
	onNew  func(*server) // tests: gets the server, to mint sessions
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
	// OAuth holds the logins of auth.oauth sources (Pipeline.OAuth); nil disables the login routes.
	OAuth *mcpoauth.Manager
	// Version is the build version shown by /metrics.
	Version string
	// TrustedProxies are the peers whose X-Forwarded-For the failed-login limiter believes.
	TrustedProxies []netip.Prefix
}

type server struct {
	Options
	key         []byte // per-process; a restart logs everyone out
	tokHash     [32]byte
	metricsHash [32]byte
	lim         *limiter
	tpl         *template.Template
	editMu      sync.Mutex             // serialises config saves
	notice      atomic.Pointer[string] // set when a save could not be applied live
	oidcMu      sync.Mutex
	oidcKey     string // issuer and allow_private of oidcProv/oidcErr
	oidcProv    *oidc.Provider
	oidcErr     error // last discovery failure, reused until oidcErrAt+oidcRetry
	oidcErrAt   time.Time
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
	s := &server{Options: o, key: make([]byte, 32), tokHash: sha256.Sum256([]byte(o.Token)), metricsHash: sha256.Sum256([]byte(o.MetricsToken)), lim: &limiter{now: o.Now, m: map[string]*bucket{}}}
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
		"can":   func(have, need string) bool { return parseRole(have) >= parseRole(need) && parseRole(have) != 0 },
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
	s.oauthRoutes(mux)
	s.apiRoutes(mux)
	s.portalRoutes(mux)
	s.oidcRoutes(mux)
	s.configRoutes(mux)
	s.helpRoutes(mux)
	if o.onNew != nil {
		o.onNew(s)
	}
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

// metricsOK is tokenOK or the scrape token; use it for /metrics only.
// Both compares always run, so timing doesn't say which token matched.
func (s *server) metricsOK(t string) bool {
	admin := s.tokenOK(t)
	sum := sha256.Sum256([]byte(t))
	scrape := subtle.ConstantTimeCompare(sum[:], s.metricsHash[:]) == 1
	return admin || s.MetricsToken != "" && scrape
}

func (s *server) mac(msg string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// role orders portal privileges: viewer < operator < admin. Zero is "unset".
type role int

const (
	roleViewer role = iota + 1
	roleOperator
	roleAdmin
)

func (r role) String() string {
	switch r {
	case roleViewer:
		return "viewer"
	case roleOperator:
		return "operator"
	case roleAdmin:
		return "admin"
	}
	return ""
}

func parseRole(v string) role {
	for _, r := range []role{roleViewer, roleOperator, roleAdmin} {
		if r.String() == v {
			return r
		}
	}
	return 0
}

type session struct {
	role  role
	actor string
}

// The cookie is stateless and signed under the per-process key; the raw token
// never reaches the browser. Value:
// "<unix ts>|<role>|<base64url(actor)>|<HMAC(key, "session:"+ts+"|"+role+"|"+actor)>".
// logout only clears the browser's cookie; the value stays valid until it
// expires or the process restarts.
const (
	sessionTTL     = 24 * time.Hour
	oidcSessionTTL = 12 * time.Hour
)

func (s *server) sessionValue(r role, actor string) string {
	ts := strconv.FormatInt(s.Now().Unix(), 10)
	return ts + "|" + r.String() + "|" + base64.RawURLEncoding.EncodeToString([]byte(actor)) + "|" + s.mac("session:"+ts+"|"+r.String()+"|"+actor)
}

func (s *server) parseSession(v string) (session, bool) {
	p := strings.Split(v, "|")
	if len(p) != 4 || s.Token == "" {
		return session{}, false
	}
	ab, err := base64.RawURLEncoding.DecodeString(p[2])
	actor := string(ab)
	if err != nil || !eq(p[3], s.mac("session:"+p[0]+"|"+p[1]+"|"+actor)) {
		return session{}, false
	}
	n, err := strconv.ParseInt(p[0], 10, 64)
	ttl := sessionTTL
	if strings.HasPrefix(actor, "oidc:") {
		ttl = oidcSessionTTL
	}
	age := s.Now().Sub(time.Unix(n, 0))
	rl := parseRole(p[1])
	return session{rl, actor}, err == nil && rl != 0 && age >= -time.Minute && age < ttl
}

type sessionKey struct{}

// actor is who the audit log names for a portal action.
func (s *server) actor(r *http.Request) string {
	if se, ok := r.Context().Value(sessionKey{}).(session); ok {
		return se.actor
	}
	return "portal"
}

func (s *server) role(r *http.Request) role {
	se, _ := r.Context().Value(sessionKey{}).(session)
	return se.role
}

func (s *server) csrfFor(session string) string { return s.mac("csrf:" + session) }

// clientIP is the limiter key (see ipKey). Behind a trusted proxy it is the client
// X-Forwarded-For names: walk the entries from the right past trusted hops; the first
// untrusted address is the client. A bad entry stops the walk at the last trusted hop.
func (s *server) clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		h = r.RemoteAddr
	}
	hop, err := netip.ParseAddr(h)
	if err != nil {
		return h
	}
	hop = hop.Unmap()
	if !s.trusted(hop) {
		return ipKey(hop)
	}
	xs := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	if len(xs) == 1 && strings.TrimSpace(xs[0]) == "" {
		return ipKey(hop)
	}
	for i := len(xs) - 1; i >= 0; i-- {
		e := strings.TrimSpace(xs[i])
		a, err := netip.ParseAddr(e)
		if err != nil {
			ap, err := netip.ParseAddrPort(e)
			if err != nil {
				return ipKey(hop)
			}
			a = ap.Addr()
		}
		a = a.Unmap()
		if !s.trusted(a) {
			return ipKey(a)
		}
		hop = a
	}
	return ipKey(hop)
}

func (s *server) trusted(a netip.Addr) bool {
	for _, p := range s.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ipKey is the IPv4 address, or the /64 prefix of an IPv6 one (a single host controls a whole /64).
func ipKey(a netip.Addr) string {
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

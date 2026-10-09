// Package mcpoauth logs in to remote MCP sources with OAuth (authorization
// code + PKCE), keeps the login on disk and refreshes it on use. It never
// puts a token in an error or a log line.
package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/cred"
	"github.com/olafkfreund/siphon/internal/source"
)

const (
	file         = "mcp-oauth.json"
	loginTimeout = 10 * time.Minute // a pending login and its state live this long
	skew         = 30 * time.Second // refresh this long before the access token expires
)

// startWait bounds how long Start waits for the authorization URL (a var for tests).
var startWait = 30 * time.Second

// ErrLoginRequired means the source has no usable login. Errors wrap it.
var ErrLoginRequired = errors.New("OAuth login required")

func loginRequired(source string) error {
	return fmt.Errorf("source %q: %w: siphon connect oauth %s, or Log in on the Sources page", source, ErrLoginRequired, source)
}

type tokenJSON struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token,omitempty"`
	Type    string    `json:"token_type,omitempty"`
	Expiry  time.Time `json:"expiry,omitzero"`
}

// stored is the on-disk login (credentials/.mcp/<source>/mcp-oauth.json).
type stored struct {
	Resource     string    `json:"resource"`
	Issuer       string    `json:"issuer,omitempty"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret,omitempty"` // only a dynamically registered client's
	Dynamic      bool      `json:"dynamic,omitempty"`
	AuthURL      string    `json:"auth_url"`
	TokenURL     string    `json:"token_url"`
	AuthStyle    int       `json:"auth_style"`
	Scopes       []string  `json:"scopes,omitempty"`
	Token        tokenJSON `json:"token"`
}

type result struct {
	code, iss string
	err       error
}

// login is one browser login in flight.
type login struct {
	cancel  context.CancelFunc
	resp    chan result   // from Complete to the fetcher
	done    chan struct{} // the goroutine ended
	saved   atomic.Bool
	fetched atomic.Bool // the browser round was used
	err     error       // set before done closes, only when !saved
	iss     string
	ferr    error // the first fetch error, repeated on the SDK's retry
}

type pending struct {
	source  string
	expires time.Time
	l       *login
}

// Manager owns the logins of every OAuth source.
type Manager struct {
	store cred.Store
	cfg   func() *config.Config
	now   func() time.Time

	// ponytail: one lock for every source's refresh, so a slow token endpoint
	// delays the others; per-source locks if that ever matters.
	rmu sync.Mutex // serialises refreshes (rotating refresh tokens must not race)

	mu      sync.Mutex
	logins  map[string]*login
	states  map[string]pending
	dead    map[string]bool // the refresh token was rejected
	clients map[string]*http.Client
}

// New returns a Manager. dir is <credentials>/.mcp.
func New(dir string, cfg func() *config.Config, now func() time.Time) *Manager {
	return &Manager{store: cred.Store{Dir: dir}, cfg: cfg, now: now,
		logins: map[string]*login{}, states: map[string]pending{}, dead: map[string]bool{}, clients: map[string]*http.Client{}}
}

func (m *Manager) source(name string) (*config.Source, error) {
	s := m.cfg().Sources[name]
	if s == nil || s.Auth == nil || s.Auth.OAuth == nil || s.URL == "" {
		return nil, fmt.Errorf("source %q has no auth.oauth", name)
	}
	return s, nil
}

// client is the source's guarded egress client (discovery, registration, token, MCP).
func (m *Manager) client(name string, s *config.Source) *http.Client {
	key := fmt.Sprintf("%s/%v", name, s.AllowPrivate)
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.clients[key]; c != nil {
		return c
	}
	c := source.HTTPClient(s.AllowPrivate, 30*time.Second, 1<<20)
	m.clients[key] = c
	return c
}

// load reads the stored login; ok is false unless it matches the source's url and client.
func (m *Manager) load(name string, s *config.Source) (raw []byte, st stored, ok bool) {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return nil, st, false
	}
	raw, err := os.ReadFile(filepath.Join(m.store.Dir, name, file))
	if err != nil || json.Unmarshal(raw, &st) != nil || st.Resource != s.URL {
		return nil, stored{}, false
	}
	if id := s.Auth.OAuth.ClientID; id != "" && (st.Dynamic || st.ClientID != id) || id == "" && !st.Dynamic {
		return nil, stored{}, false
	}
	return raw, st, true
}

func (m *Manager) oauthConfig(st stored, s *config.Source) *oauth2.Config {
	secret := st.ClientSecret
	if !st.Dynamic {
		secret = s.Auth.OAuth.ClientSecret.Value
	}
	return &oauth2.Config{ClientID: st.ClientID, ClientSecret: secret, Scopes: st.Scopes,
		Endpoint: oauth2.Endpoint{AuthURL: st.AuthURL, TokenURL: st.TokenURL, AuthStyle: oauth2.AuthStyle(st.AuthStyle)}}
}

var safeText = regexp.MustCompile(`[^A-Za-z0-9 _.:,/-]`)

func clip(s string, n int) string {
	s = safeText.ReplaceAllString(s, "")
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// cleanErr cuts an error down to the OAuth error and description, so a body
// or URL carrying a token never reaches a log, job output or the API.
func cleanErr(err error) error {
	if err == nil {
		return nil
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return fmt.Errorf("token request failed: %s: %s", clip(re.ErrorCode, 64), clip(re.ErrorDescription, 200))
	}
	if errors.Is(err, ErrLoginRequired) {
		return err
	}
	return errors.New(clip(err.Error(), 300))
}

// access returns a fresh access token, refreshing and writing it back if needed.
func (m *Manager) access(ctx context.Context, name string) (*oauth2.Token, error) {
	s, err := m.source(name)
	if err != nil {
		return nil, err
	}
	m.rmu.Lock()
	defer m.rmu.Unlock()
	for range 2 {
		raw, st, ok := m.load(name, s)
		if !ok {
			return nil, loginRequired(name)
		}
		t := st.Token
		if t.Access != "" && (t.Expiry.IsZero() || t.Expiry.After(m.now().Add(skew))) {
			return &oauth2.Token{AccessToken: t.Access, TokenType: t.Type, Expiry: t.Expiry}, nil
		}
		if t.Refresh == "" {
			return nil, loginRequired(name)
		}
		rctx := context.WithValue(ctx, oauth2.HTTPClient, m.client(name, s))
		nt, err := m.oauthConfig(st, s).TokenSource(rctx, &oauth2.Token{RefreshToken: t.Refresh}).Token()
		if err != nil {
			var re *oauth2.RetrieveError
			if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
				m.mu.Lock()
				m.dead[name] = true
				m.mu.Unlock()
				return nil, loginRequired(name)
			}
			return nil, cleanErr(err)
		}
		st.Token = tokenJSON{Access: nt.AccessToken, Refresh: nt.RefreshToken, Type: nt.TokenType, Expiry: nt.Expiry}
		b, _ := json.Marshal(st)
		wrote, err := m.store.Save(name, file, raw, b)
		if err != nil {
			return nil, cleanErr(err)
		}
		if wrote {
			return nt, nil
		}
		// another writer won: reload and use its token
	}
	return nil, errors.New("OAuth refresh raced with another writer; try again")
}

// Token returns the source's current access token, or an error wrapping ErrLoginRequired.
func (m *Manager) Token(ctx context.Context, name string) (string, error) {
	t, err := m.access(ctx, name)
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

type tokenSource struct {
	m    *Manager
	name string
}

func (t tokenSource) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return t.m.access(ctx, t.name)
}

type handler struct {
	m    *Manager
	name string
}

// Handler is the steady-state auth.OAuthHandler: it never waits for a browser.
func (m *Manager) Handler(name string) auth.OAuthHandler { return handler{m, name} }

func (h handler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	s, err := h.m.source(h.name)
	if err != nil {
		return nil, err
	}
	if _, _, ok := h.m.load(h.name, s); !ok {
		return nil, nil // no header; the 401 then reaches Authorize
	}
	return tokenSource{h.m, h.name}, nil
}

func (h handler) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	resp.Body.Close()
	return loginRequired(h.name)
}

// Status is none, pending, ok or expired, and the access token's expiry.
func (m *Manager) Status(name string) (string, time.Time) {
	s, err := m.source(name)
	if err != nil {
		return "none", time.Time{}
	}
	m.mu.Lock()
	_, busy := m.logins[name]
	dead := m.dead[name]
	m.mu.Unlock()
	if busy {
		return "pending", time.Time{}
	}
	_, st, ok := m.load(name, s)
	switch {
	case !ok:
		return "none", time.Time{}
	case dead, st.Token.Refresh == "" && !st.Token.Expiry.IsZero() && !st.Token.Expiry.After(m.now()):
		return "expired", st.Token.Expiry
	}
	return "ok", st.Token.Expiry
}

// Logout cancels a pending login and deletes the stored one.
func (m *Manager) Logout(name string) error {
	m.mu.Lock()
	if l := m.logins[name]; l != nil {
		l.cancel()
	}
	delete(m.dead, name)
	m.mu.Unlock()
	if _, err := m.source(name); err != nil {
		return err
	}
	return m.store.Delete(name)
}

// Start begins a browser login and returns the authorization URL. It waits at
// most 30 seconds for it, and cancels an earlier pending login of the source.
func (m *Manager) Start(ctx context.Context, name string) (string, error) {
	s, err := m.source(name)
	if err != nil {
		return "", err
	}
	pub := strings.TrimRight(m.cfg().Server.PublicURL, "/")
	if pub == "" {
		return "", errors.New("OAuth login needs server.public_url")
	}
	lctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	l := &login{cancel: cancel, resp: make(chan result, 1), done: make(chan struct{})}
	m.mu.Lock()
	if old := m.logins[name]; old != nil {
		old.cancel()
	}
	m.logins[name] = l
	delete(m.dead, name)
	m.mu.Unlock()

	urlCh := make(chan string, 1)
	go func() {
		defer close(l.done)
		defer cancel()
		err := m.run(lctx, name, s, pub+"/oauth/callback", l, urlCh)
		if !l.saved.Load() {
			l.err = cleanErr(err)
			if l.err == nil {
				l.err = errors.New("login ended without a token")
			}
		}
		m.mu.Lock()
		if m.logins[name] == l {
			delete(m.logins, name)
		}
		for st, p := range m.states {
			if p.l == l {
				delete(m.states, st)
			}
		}
		m.mu.Unlock()
	}()

	select {
	case u := <-urlCh:
		return u, nil
	case <-l.done:
		return "", l.err
	case <-ctx.Done():
		cancel()
		return "", ctx.Err()
	case <-time.After(startWait):
		cancel()
		return "", errors.New("timed out waiting for the authorization URL")
	}
}

func (m *Manager) run(ctx context.Context, name string, s *config.Source, redirect string, l *login, urlCh chan<- string) error {
	hc := m.client(name, s)
	oc := s.Auth.OAuth
	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:         redirect,
		RequestRefreshToken: true,
		Client:              hc,
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			// the SDK retries after a failed attempt: one browser round per login
			if !l.fetched.CompareAndSwap(false, true) {
				if l.ferr != nil {
					return nil, l.ferr
				}
				return nil, errors.New("login failed")
			}
			u, err := url.Parse(args.URL)
			if err != nil || u.Query().Get("state") == "" {
				return nil, errors.New("authorization URL has no state")
			}
			state := u.Query().Get("state")
			m.mu.Lock()
			m.states[state] = pending{name, m.now().Add(loginTimeout), l}
			m.mu.Unlock()
			urlCh <- args.URL
			select {
			case r := <-l.resp:
				if r.err != nil {
					l.ferr = r.err
					return nil, r.err
				}
				l.iss = r.iss
				return &auth.AuthorizationResult{Code: r.code, State: state, Iss: r.iss}, nil
			case <-ctx.Done():
				return nil, errors.New("login timed out or was cancelled")
			}
		},
		NewTokenSource: func(_ context.Context, c *oauth2.Config, t *oauth2.Token) (oauth2.TokenSource, error) {
			st := stored{Resource: s.URL, Issuer: l.iss, ClientID: c.ClientID, Dynamic: oc.ClientID == "",
				AuthURL: c.Endpoint.AuthURL, TokenURL: c.Endpoint.TokenURL, AuthStyle: int(c.Endpoint.AuthStyle), Scopes: c.Scopes,
				Token: tokenJSON{Access: t.AccessToken, Refresh: t.RefreshToken, Type: t.TokenType, Expiry: t.Expiry}}
			if st.Dynamic {
				st.ClientSecret = c.ClientSecret
			}
			b, _ := json.Marshal(st)
			var old []byte
			if o, err := os.ReadFile(filepath.Join(m.store.Dir, name, file)); err == nil {
				old = o
			}
			if wrote, err := m.store.Save(name, file, old, b); err != nil {
				return nil, cleanErr(err)
			} else if !wrote {
				if err := m.store.Put(name, file, b); err != nil {
					return nil, cleanErr(err)
				}
			}
			l.saved.Store(true)
			m.mu.Lock()
			delete(m.dead, name)
			m.mu.Unlock()
			return tokenSource{m, name}, nil
		},
	}
	if len(oc.Scopes) > 0 {
		cfg.ScopeFilter = func([]string) []string { return append([]string(nil), oc.Scopes...) }
	}
	_, prev, reuse := m.load(name, s)
	switch {
	case oc.ClientID != "":
		pc := &oauthex.ClientCredentials{ClientID: oc.ClientID}
		if oc.ClientSecret.Value != "" {
			pc.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: oc.ClientSecret.Value}
		}
		cfg.PreregisteredClient = pc
	case reuse && prev.Issuer != "": // a dynamically registered client is reused on re-login, bound to its issuer
		pc := &oauthex.ClientCredentials{ClientID: prev.ClientID, Issuer: prev.Issuer}
		if prev.ClientSecret != "" {
			pc.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: prev.ClientSecret}
		}
		cfg.PreregisteredClient = pc
	default:
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs: []string{redirect}, ClientName: "siphon", TokenEndpointAuthMethod: "none",
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}}}
	}
	h, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return err
	}
	tr := &mcp.StreamableClientTransport{Endpoint: s.URL, HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true, OAuthHandler: h}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "siphon", Version: "login"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		return err
	}
	return cs.Close()
}

// Complete finishes the login the browser returned for: state is single-use
// and expires. It returns the source name once the token is saved (or the
// login failed). errParam is the OAuth "error" query parameter, if any.
func (m *Manager) Complete(state, code, iss, errParam string) (string, error) {
	m.mu.Lock()
	p, ok := m.states[state]
	delete(m.states, state)
	m.mu.Unlock()
	if !ok || m.now().After(p.expires) {
		return "", errors.New("unknown or expired login")
	}
	r := result{code: code, iss: iss}
	if errParam != "" {
		r.err = fmt.Errorf("authorization refused: %s", clip(errParam, 64))
	}
	p.l.resp <- r
	select {
	case <-p.l.done:
	case <-time.After(startWait):
		return p.source, errors.New("timed out finishing the login")
	}
	if p.l.saved.Load() {
		return p.source, nil
	}
	return p.source, p.l.err
}

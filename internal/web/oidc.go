package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/source"
)

const (
	oidcCookie = "siphon_oidc"
	oidcPath   = "/login/oidc"
	oidcWindow = 10 * time.Minute
)

// loginData is the login page's data.
type loginData struct {
	Err        string
	SSO, Token bool
}

func (s *server) loginData(err string) loginData {
	o := s.Config().Server.OIDC
	return loginData{Err: err, SSO: o != nil, Token: o == nil || o.TokenLoginOn()}
}

// oidcRetry is how long a failed discovery is answered from memory, so an
// unreachable issuer can't tie up a connection per visit.
const oidcRetry = 30 * time.Second

// oidcProvider discovers lazily and caches the provider per issuer and
// allow_private (JWKS reuses the discovery client). A failure is cached for oidcRetry.
func (s *server) oidcProvider(r *http.Request, o *config.OIDC) (*oidc.Provider, error) {
	key := o.Issuer + "|" + strconv.FormatBool(o.AllowPrivate)
	s.oidcMu.Lock()
	if s.oidcKey == key {
		p, err, at := s.oidcProv, s.oidcErr, s.oidcErrAt
		if p != nil || err != nil && s.Now().Sub(at) < oidcRetry {
			s.oidcMu.Unlock()
			return p, err
		}
	}
	s.oidcMu.Unlock()
	p, err := oidc.NewProvider(oidcCtx(r, o), o.Issuer)
	s.oidcMu.Lock()
	s.oidcKey, s.oidcProv, s.oidcErr, s.oidcErrAt = key, p, err, s.Now()
	s.oidcMu.Unlock()
	return p, err
}

// oidcCtx makes go-oidc and oauth2 use the guarded client (no redirects, private-address rules).
func oidcCtx(r *http.Request, o *config.OIDC) context.Context {
	return oidc.ClientContext(r.Context(), source.HTTPClient(o.AllowPrivate, 30*time.Second, 1<<20))
}

func (s *server) oauthConfig(p *oidc.Provider, o *config.OIDC) *oauth2.Config {
	return &oauth2.Config{ClientID: o.ClientID, ClientSecret: o.ClientSecret.Value, Endpoint: p.Endpoint(),
		RedirectURL: strings.TrimRight(s.Config().Server.PublicURL, "/") + oidcPath + "/callback", Scopes: o.ScopesOrDefault()}
}

func randString() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

var longToken = regexp.MustCompile(`[A-Za-z0-9._~+/=-]{24,}`)

// cleanErr keeps an error short and free of anything token-shaped.
func cleanErr(err error) string {
	m := longToken.ReplaceAllString(err.Error(), "[redacted]")
	if len(m) > 200 {
		m = m[:200] + "…"
	}
	return m
}

// oidcPage renders the small result page (signed in, refused, failed).
func (s *server) oidcPage(w http.ResponseWriter, code int, title, msg string, refresh bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	s.tpl.ExecuteTemplate(w, "signedin", map[string]any{"Title": title, "Msg": msg, "Refresh": refresh})
}

func (s *server) oidcRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+oidcPath, s.oidcStart)
	mux.HandleFunc("GET "+oidcPath+"/callback", s.oidcCallback)
}

func (s *server) oidcStart(w http.ResponseWriter, r *http.Request) {
	o := s.Config().Server.OIDC
	if o == nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.oidcProvider(r, o)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		s.render(w, "login", s.loginData("SSO provider unreachable: "+cleanErr(err)))
		return
	}
	state, nonce, verifier := randString(), randString(), oauth2.GenerateVerifier()
	ts := strconv.FormatInt(s.Now().Unix(), 10)
	body := ts + "|" + state + "|" + nonce + "|" + verifier
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: body + "|" + s.mac("oidc:"+body), Path: oidcPath, MaxAge: int(oidcWindow.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookie(r)})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, s.oauthConfig(p, o).AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusSeeOther)
}

// roleFor is the highest role whose groups or verified emails match.
func roleFor(o *config.OIDC, email string, verified bool, groups []string) role {
	match := func(m config.RoleMatch) bool {
		for _, g := range groups {
			if slices.Contains(m.Groups, g) {
				return true
			}
		}
		return verified && slices.ContainsFunc(m.Emails, func(e string) bool { return strings.EqualFold(e, email) })
	}
	switch {
	case match(o.Roles.Admin):
		return roleAdmin
	case match(o.Roles.Operator):
		return roleOperator
	case match(o.Roles.Viewer):
		return roleViewer
	}
	return 0
}

func claimGroups(raw map[string]any, claim string) []string {
	switch v := raw[claim].(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			if g, ok := x.(string); ok {
				out = append(out, g)
			}
		}
		return out
	}
	return nil
}

func (s *server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	o := s.Config().Server.OIDC
	if o == nil {
		http.NotFound(w, r)
		return
	}
	ip := s.clientIP(r)
	if s.lim.blocked(ip) {
		http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
		return
	}
	// fail counts toward the limiter. The page says which step failed, never
	// the provider's words: code, state, tokens and claims are not echoed.
	fail := func(step string, err error) {
		s.lim.fail(ip)
		if err != nil {
			slog.Warn("oidc sign-in failed", "step", step, "err", cleanErr(err))
		}
		s.oidcPage(w, http.StatusBadRequest, "Sign-in failed", "SSO sign-in failed ("+step+"). Start again from the sign-in page.", false)
	}
	c, err := r.Cookie(oidcCookie)
	if err != nil {
		// Not counted: without the signed cookie there is nothing to guess, and
		// behind a proxy every user shares one limiter bucket.
		s.oidcPage(w, http.StatusBadRequest, "Sign-in failed", "SSO sign-in failed (no sign-in in progress). Start again from the sign-in page.", false)
		return
	}
	f := strings.Split(c.Value, "|")
	if len(f) != 5 || !eq(f[4], s.mac("oidc:"+strings.Join(f[:4], "|"))) {
		fail("bad sign-in cookie", nil)
		return
	}
	n, _ := strconv.ParseInt(f[0], 10, 64)
	if age := s.Now().Sub(time.Unix(n, 0)); age < -time.Minute || age >= oidcWindow {
		fail("sign-in expired", nil)
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f[1])) != 1 {
		fail("state mismatch", nil)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Path: oidcPath, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookie(r)})
	if q.Get("error") != "" || q.Get("code") == "" {
		fail("provider refused", nil)
		return
	}
	p, err := s.oidcProvider(r, o)
	if err != nil {
		fail("provider unreachable", err)
		return
	}
	ctx := oidcCtx(r, o)
	tok, err := s.oauthConfig(p, o).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(f[3]))
	if err != nil {
		fail("code exchange", err)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		fail("no id_token", nil)
		return
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: o.ClientID, Now: s.Now}).Verify(ctx, raw)
	if err != nil {
		fail("id_token", err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f[2])) != 1 {
		fail("nonce mismatch", nil)
		return
	}
	var cl struct {
		Sub, Email    string
		EmailVerified any `json:"email_verified"` // some providers send "true"
	}
	var all map[string]any
	if err := idt.Claims(&cl); err != nil {
		fail("claims", err)
		return
	}
	if err := idt.Claims(&all); err != nil {
		fail("claims", err)
		return
	}
	// An unverified email never names anyone: the audit log would believe it.
	who := cl.Sub
	verified := cl.EmailVerified == true || cl.EmailVerified == "true"
	if cl.Email != "" && verified {
		who = cl.Email
	}
	actor := "oidc:" + who
	rl := roleFor(o, cl.Email, verified, claimGroups(all, o.GroupsClaimOrDefault()))
	if rl == 0 {
		s.audit(actor, "oidc_refused", actor)
		s.oidcPage(w, http.StatusForbidden, "Not allowed", who+" has no role in Siphon. Ask an admin.", false)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.sessionValue(rl, actor), Path: "/", MaxAge: int(oidcSessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureCookie(r)})
	s.audit(actor, "oidc_signin", actor+" as "+rl.String())
	// A 303 here would stay cross-site, and the Strict cookie would not be sent; a
	// refresh from our own page is same-site.
	s.oidcPage(w, http.StatusOK, "Signed in", "Signed in as "+who+".", true)
}

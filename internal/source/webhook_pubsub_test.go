package source

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

var pubsubNow = time.Unix(1_800_000_000, 0)

const (
	psAud = "https://siphon.example.com/hook/p"
	psSA  = "push@p.iam.gserviceaccount.com"
)

type psClaims struct {
	Iss      string   `json:"iss"`
	Aud      []string `json:"aud"`
	Email    string   `json:"email"`
	Verified bool     `json:"email_verified"`
	Exp      int64    `json:"exp"`
}

func psToken(t *testing.T, key *rsa.PrivateKey, c psClaims) string {
	t.Helper()
	sg, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(sg).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestPubSubWebhook(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	}))
	defer jwks.Close()
	now := func() time.Time { return pubsubNow }
	gv := newGoogleVerifier(jwks.Client(), jwks.URL, now) // loopback: a plain client

	good := psClaims{"https://accounts.google.com", []string{psAud}, psSA, true, pubsubNow.Add(time.Hour).Unix()}
	with := func(f func(*psClaims)) psClaims { c := good; f(&c); return c }
	env := func(data string) string {
		return `{"message":{"data":"` + base64.StdEncoding.EncodeToString([]byte(data)) + `","attributes":{"k":"v"},"messageId":"m1","publishTime":"2026-01-01T00:00:00Z"},"subscription":"projects/p/subscriptions/s"}`
	}

	for _, tc := range []struct {
		name   string
		auth   string // "" signs good claims with key
		claims *psClaims
		signer *rsa.PrivateKey
		body   string
		status int
	}{
		{name: "valid JSON data", body: env(`{"a":1}`), status: 204},
		{name: "valid text data", body: env(`plain`), status: 204},
		{name: "accounts.google.com issuer", claims: ptr(with(func(c *psClaims) { c.Iss = "accounts.google.com" })), body: env(`{}`), status: 204},
		{name: "email case differs", claims: ptr(with(func(c *psClaims) { c.Email = strings.ToUpper(psSA) })), body: env(`{}`), status: 204},
		{name: "no header", auth: "none", body: env(`{}`), status: 401},
		{name: "not a bearer", auth: "Basic abc", body: env(`{}`), status: 401},
		{name: "garbage token", auth: "Bearer abc.def.ghi", body: env(`{}`), status: 401},
		{name: "wrong key", signer: other, body: env(`{}`), status: 401},
		{name: "expired", claims: ptr(with(func(c *psClaims) { c.Exp = pubsubNow.Add(-time.Minute).Unix() })), body: env(`{}`), status: 401},
		{name: "wrong issuer", claims: ptr(with(func(c *psClaims) { c.Iss = "https://evil.example" })), body: env(`{}`), status: 401},
		{name: "wrong audience", claims: ptr(with(func(c *psClaims) { c.Aud = []string{"https://other"} })), body: env(`{}`), status: 401},
		{name: "wrong email", claims: ptr(with(func(c *psClaims) { c.Email = "x@p.iam.gserviceaccount.com" })), body: env(`{}`), status: 401},
		{name: "email unverified", claims: ptr(with(func(c *psClaims) { c.Verified = false })), body: env(`{}`), status: 401},
		{name: "no message", body: `{"subscription":"s"}`, status: 400},
		{name: "bad base64", body: `{"message":{"data":"!!!","messageId":"m1"}}`, status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []Event
			var ids []string
			h := NewWebhook(WebhookOptions{Name: "p", Signature: "google-oidc", Format: "pubsub", Audience: psAud, ServiceAccount: psSA, Google: gv, Now: now},
				func(_ context.Context, ev Event, id string) (bool, error) {
					got, ids = append(got, ev), append(ids, id)
					return false, nil
				})
			c, k := good, key
			if tc.claims != nil {
				c = *tc.claims
			}
			if tc.signer != nil {
				k = tc.signer
			}
			auth := tc.auth
			if auth == "" {
				auth = "Bearer " + psToken(t, k, c)
			}
			r := httptest.NewRequest(http.MethodPost, "/hook/p", strings.NewReader(tc.body))
			if auth != "none" {
				r.Header.Set("Authorization", auth)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
			if tc.status != 204 {
				if len(got) != 0 {
					t.Fatalf("delivered on %d", w.Code)
				}
				return
			}
			if len(got) != 1 || ids[0] != "m1" && tc.name != "valid JSON data" {
				t.Fatalf("got %d events, ids %v", len(got), ids)
			}
			if _, ok := got[0].Headers["authorization"]; ok {
				t.Error("bearer header reached the event")
			}
			m := got[0].Data.(map[string]any)
			if m["message_id"] != "m1" || m["subscription"] != "projects/p/subscriptions/s" || m["publish_time"] != "2026-01-01T00:00:00Z" || m["attributes"].(map[string]string)["k"] != "v" {
				t.Errorf("event %v", m)
			}
			switch tc.name {
			case "valid JSON data":
				if d, _ := m["data"].(map[string]any); d["a"] == nil {
					t.Errorf("data %v", m["data"])
				}
			case "valid text data":
				if m["data"] != "plain" {
					t.Errorf("data %v", m["data"])
				}
			}
		})
	}
}

func TestGoogleOIDCWithoutFormat(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	}))
	defer jwks.Close()
	now := func() time.Time { return pubsubNow }
	var got []Event
	h := NewWebhook(WebhookOptions{Name: "p", Signature: "google-oidc", Audience: psAud, ServiceAccount: psSA, Google: newGoogleVerifier(jwks.Client(), jwks.URL, now), Now: now},
		func(_ context.Context, ev Event, _ string) (bool, error) { got = append(got, ev); return false, nil })
	r := httptest.NewRequest(http.MethodPost, "/hook/p", strings.NewReader(`{"x":1}`))
	r.Header.Set("Authorization", "Bearer "+psToken(t, key, psClaims{"https://accounts.google.com", []string{psAud}, psSA, true, pubsubNow.Add(time.Hour).Unix()}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 202 || len(got) != 1 || got[0].Data.(map[string]any)["x"] == nil {
		t.Fatalf("status %d, events %v", w.Code, got)
	}
}

func ptr[T any](v T) *T { return &v }

func TestPubSubDuplicateAndRateLimit(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	}))
	defer jwks.Close()
	now := func() time.Time { return pubsubNow }
	seen, n := map[string]bool{}, 0
	h := NewWebhook(WebhookOptions{Name: "p", Signature: "google-oidc", Format: "pubsub", Audience: psAud, ServiceAccount: psSA,
		Google: newGoogleVerifier(jwks.Client(), jwks.URL, now), Now: now, Limit: NewLimiter(2, 0)},
		func(_ context.Context, _ Event, id string) (bool, error) {
			dup := seen[id]
			seen[id] = true
			if !dup {
				n++
			}
			return dup, nil
		})
	tok := psToken(t, key, psClaims{"https://accounts.google.com", []string{psAud}, psSA, true, pubsubNow.Add(time.Hour).Unix()})
	post := func() int {
		r := httptest.NewRequest(http.MethodPost, "/hook/p", strings.NewReader(`{"message":{"data":"e30=","messageId":"m1"}}`))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if a, b, c := post(), post(), post(); a != 204 || b != 204 || c != 429 || n != 1 {
		t.Fatalf("statuses %d %d %d, delivered %d", a, b, c, n)
	}
}

func psServe(t *testing.T, key *rsa.PrivateKey, hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	}))
}

func TestGoogleVerifierRejectsOtherAlgs(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var hits atomic.Int32
	jwks := psServe(t, key, &hits)
	defer jwks.Close()
	gv := newGoogleVerifier(jwks.Client(), jwks.URL, func() time.Time { return pubsubNow })
	claims := `{"iss":"https://accounts.google.com","aud":["` + psAud + `"],"email":"` + psSA + `","email_verified":true,"exp":` + strconv.FormatInt(pubsubNow.Add(time.Hour).Unix(), 10) + `}`
	enc := base64.RawURLEncoding.EncodeToString
	none := enc([]byte(`{"alg":"none","kid":"k1"}`)) + "." + enc([]byte(claims)) + "."
	sg, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte("0123456789abcdef0123456789abcdef")}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	hs, _ := sg.Sign([]byte(claims))
	hsTok, _ := hs.CompactSerialize()
	for name, tok := range map[string]string{"alg none": none, "HS256": hsTok} {
		if _, err := gv.Verify(context.Background(), tok, psAud); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestGoogleVerifierLimitsKeyRefetch(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var hits atomic.Int32
	jwks := psServe(t, key, &hits)
	defer jwks.Close()
	clock := pubsubNow
	gv := newGoogleVerifier(jwks.Client(), jwks.URL, func() time.Time { return clock })
	c := psClaims{"https://accounts.google.com", []string{psAud}, psSA, true, pubsubNow.Add(time.Hour).Unix()}
	verify := func(kid string) error {
		sg, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
		tok, _ := jwt.Signed(sg).Claims(c).Serialize()
		_, err := gv.Verify(context.Background(), tok, psAud)
		return err
	}
	if err := verify("k1"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	before := hits.Load()
	for i := range 2 {
		if verify("unknown-"+strconv.Itoa(i)) == nil {
			t.Fatal("unknown kid accepted")
		}
	}
	if n := hits.Load() - before; n > 1 {
		t.Errorf("%d JWKS fetches for unknown key ids in a minute, want at most 1", n)
	}
	if err := verify("k1"); err != nil {
		t.Errorf("known kid: %v", err)
	}
}

package source

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const googleCertsURL = "https://www.googleapis.com/oauth2/v3/certs"

// GoogleVerifier checks the OIDC bearer tokens Google signs for Pub/Sub push
// subscriptions. One serves every source; its key set caches Google's keys.
type GoogleVerifier struct {
	v *oidc.IDTokenVerifier
}

// NewGoogleVerifier fetches Google's certs through hc (the guarded client).
func NewGoogleVerifier(hc *http.Client, now func() time.Time) *GoogleVerifier {
	return newGoogleVerifier(hc, googleCertsURL, now)
}

func newGoogleVerifier(hc *http.Client, certsURL string, now func() time.Time) *GoogleVerifier {
	// The key set keeps this context for later key fetches, so it must not be a request's.
	ks := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), hc), certsURL)
	// go-oidc compares iss and aud against one value each; Siphon checks them below.
	return &GoogleVerifier{oidc.NewVerifier("", ks, &oidc.Config{SkipIssuerCheck: true, SkipClientIDCheck: true, Now: now})}
}

// Verify checks the signature, expiry, issuer, audience and email_verified of
// a token, and returns its email. It never echoes the token.
func (g *GoogleVerifier) Verify(ctx context.Context, token, audience string) (string, error) {
	t, err := g.v.Verify(ctx, token)
	if err != nil {
		return "", err
	}
	if t.Issuer != "https://accounts.google.com" && t.Issuer != "accounts.google.com" {
		return "", errors.New("issuer is not Google")
	}
	if audience == "" || !slices.Contains(t.Audience, audience) {
		return "", errors.New("audience mismatch")
	}
	var c struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err := t.Claims(&c); err != nil || !c.Verified || c.Email == "" {
		return "", errors.New("email not verified")
	}
	return c.Email, nil
}

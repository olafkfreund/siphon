package source

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTeamsWebhook(t *testing.T) {
	raw := []byte(strings.Repeat("k", 32))
	secret := base64.StdEncoding.EncodeToString(raw)
	sign := func(body string) string {
		m := hmac.New(sha256.New, raw)
		m.Write([]byte(body))
		return base64.StdEncoding.EncodeToString(m.Sum(nil))
	}
	const body = `{"type":"message","id":"1700000000000","text":"hi"}`
	for _, tc := range []struct {
		name, secret, reply, body, auth string
		code                            int
		want                            string // the exact response body, or "" to skip
		events                          int
	}{
		{"signed", secret, "", body, "HMAC " + sign(body), 200, `{"type":"message","text":"Received."}`, 1},
		{"reply", secret, "ok", body, "HMAC " + sign(body), 200, `{"type":"message","text":"ok"}`, 1},
		{"body hash key without id", secret, "", `{"text":"x"}`, "HMAC " + sign(`{"text":"x"}`), 200, `{"type":"message","text":"Received."}`, 1},
		{"non-string id uses the body hash", secret, "", `{"id":7,"text":"x"}`, "HMAC " + sign(`{"id":7,"text":"x"}`), 200, `{"type":"message","text":"Received."}`, 1},
		{"wrong signature", secret, "", body, "HMAC " + sign("other"), 401, "", 0},
		{"missing header", secret, "", body, "", 401, "", 0},
		{"wrong scheme", secret, "", body, "Bearer " + sign(body), 401, "", 0},
		{"non-base64 signature", secret, "", body, "HMAC !!!", 401, "", 0},
		{"bad key", "not base64!", "", body, "HMAC " + sign(body), 500, "secret misconfigured", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]bool{}
			var got []Event
			var ids []string
			var reason string
			h := NewWebhook(WebhookOptions{Name: "t", Secret: tc.secret, Signature: "teams-hmac", ReplyText: tc.reply, OnReject: func(_ int, why string) { reason = why }},
				func(_ context.Context, ev Event, id string) (bool, error) {
					ids = append(ids, id)
					if seen[id] {
						return true, nil
					}
					seen[id] = true
					got = append(got, ev)
					return false, nil
				})
			post := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/hook/t", strings.NewReader(tc.body))
				r.Header.Set("Content-Type", "application/json")
				if tc.auth != "" {
					r.Header.Set("Authorization", tc.auth)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			w := post()
			if w.Code != tc.code || (tc.code == 200 && w.Body.String() != tc.want) || (tc.code == 500 && reason != tc.want) {
				t.Fatalf("%d %q (%q)", w.Code, w.Body, reason)
			}
			if w.Body.Len() > 0 && tc.code != 200 {
				t.Errorf("body on an error: %q", w.Body)
			}
			if tc.code == 200 {
				if ct := w.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("content type %q", ct)
				}
				if d := post(); d.Code != 200 || d.Body.String() != w.Body.String() { // a duplicate gets the same reply
					t.Errorf("duplicate: %d %q", d.Code, d.Body)
				}
			}
			if len(got) != tc.events {
				t.Fatalf("events %d, want %d", len(got), tc.events)
			}
			if tc.events == 1 {
				if _, ok := got[0].Headers["authorization"]; ok {
					t.Errorf("authorization kept: %v", got[0].Headers)
				}
				if tc.name == "signed" && ids[0] != "1700000000000" {
					t.Errorf("key %q, want the activity id", ids[0])
				}
			}
		})
	}
}

func TestTeamsWebhookDeliverError(t *testing.T) {
	raw := []byte(strings.Repeat("k", 32))
	m := hmac.New(sha256.New, raw)
	const body = `{"id":"1"}`
	m.Write([]byte(body))
	h := NewWebhook(WebhookOptions{Name: "t", Secret: base64.StdEncoding.EncodeToString(raw), Signature: "teams-hmac"},
		func(context.Context, Event, string) (bool, error) { return false, errors.New("db down") })
	r := httptest.NewRequest(http.MethodPost, "/hook/t", strings.NewReader(body))
	r.Header.Set("Authorization", "HMAC "+base64.StdEncoding.EncodeToString(m.Sum(nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 500 || w.Body.Len() != 0 {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
}

package source

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhook(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, preset := range []string{"github", "sha256"} {
		t.Run(preset, func(t *testing.T) {
			calls := 0
			keyInput := `{"n":1}`
			if preset == "sha256" {
				keyInput = "1700000000." + keyInput
			}
			h := NewWebhook(WebhookOptions{Name: "hook", Secret: "key", Signature: preset, SigHeader: "X-Signature", TimestampHeader: "X-Time", IDHeader: "X-Delivery", MaxBody: 16, Now: func() time.Time { return now }}, func(_ context.Context, ev Event, id string) (bool, error) {
				calls++
				if ev.Source != "hook" || !ev.ReceivedAt.Equal(now) || id != bodyKey(keyInput) || ev.Headers["x-delivery"] != "one" || ev.Headers["x-signature"] != "" || ev.Headers["x-hub-signature-256"] != "" {
					t.Errorf("bad event: %+v id=%q", ev, id)
				}
				if ev.Data.(map[string]any)["n"] != int64(1) {
					t.Errorf("bad data: %+v", ev.Data)
				}
				return calls > 1, nil
			})
			request := func(body, timestamp string) *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/hook/hook", strings.NewReader(body))
				mac := hmac.New(sha256.New, []byte("key"))
				if preset == "sha256" {
					mac.Write([]byte(timestamp + "."))
				}
				mac.Write([]byte(body))
				header := "X-Signature"
				value := hex.EncodeToString(mac.Sum(nil))
				if preset == "github" {
					header = "X-Hub-Signature-256"
					value = "sha256=" + value
				}
				r.Header.Set(header, value)
				r.Header.Set("X-Time", timestamp)
				r.Header.Set("X-Delivery", "one")
				return r
			}
			check := func(r *http.Request, want int) {
				t.Helper()
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != want {
					t.Errorf("status %d, want %d", w.Code, want)
				}
			}
			check(request(`{"n":1}`, "1700000000"), 202)
			check(request(`{"n":1}`, "1700000000"), 409)
			bad := request(`{"n":1}`, "1700000000")
			bad.Header.Set(map[string]string{"github": "X-Hub-Signature-256", "sha256": "X-Signature"}[preset], "bad")
			check(bad, 401)
			if preset == "sha256" {
				check(request(`{"n":1}`, "1699999000"), 401)
				tampered := request(`{"n":1}`, "1700000000")
				tampered.Header.Set("X-Time", "1700000001")
				check(tampered, 401)
			} else {
				// GitHub does not sign X-Time, so the configured header is ignored.
				check(request(`{"n":1}`, "1699999000"), 409)
			}
			check(request(strings.Repeat("x", 17), "1700000000"), 413)
			get := request(`{"n":1}`, "1700000000")
			get.Method = http.MethodGet
			check(get, 405)
			for i := calls; i < 20; i++ {
				check(request(`{"n":1}`, "1700000000"), 409)
			}
			check(request(`{"n":1}`, "1700000000"), 429)
		})
	}
}

func TestWebhookRawAndFailure(t *testing.T) {
	var got Event
	var gotID string
	h := NewWebhook(WebhookOptions{Secret: "key", Signature: "sha256", SigHeader: "X-Signature"}, func(_ context.Context, ev Event, id string) (bool, error) {
		got, gotID = ev, id
		return false, errors.New("private failure")
	})
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("plain"))
	mac := hmac.New(sha256.New, []byte("key"))
	mac.Write([]byte("plain"))
	r.Header.Set("X-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	for _, header := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		r.Header.Set(header, "secret")
	}
	r.Header.Set("Connection", "Keep-Alive, X-Secret")
	r.Header.Set("X-Secret", "v")
	r.Header.Set("X-Useful", "kept")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	sum := sha256.Sum256([]byte("plain"))
	if w.Code != 500 || strings.Contains(w.Body.String(), "private failure") || got.Data.(map[string]any)["raw"] != "plain" || gotID != hex.EncodeToString(sum[:]) {
		t.Fatalf("status=%d body=%q event=%+v id=%q", w.Code, w.Body.String(), got, gotID)
	}
	if got.Headers["x-useful"] != "kept" {
		t.Fatalf("useful header missing: %+v", got.Headers)
	}
	for _, header := range []string{"x-signature", "authorization", "cookie", "proxy-authorization", "proxy-authenticate", "connection", "keep-alive", "x-secret", "te", "trailer", "transfer-encoding", "upgrade"} {
		if _, ok := got.Headers[header]; ok {
			t.Errorf("leaked %s", header)
		}
	}
}

func TestWebhookBadSignaturesDoNotExhaustBucket(t *testing.T) {
	h := NewWebhook(WebhookOptions{Secret: "key", Signature: "sha256", SigHeader: "X-Signature"}, func(context.Context, Event, string) (bool, error) {
		return false, nil
	})
	for i := 0; i < 31; i++ {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body"))
		r.Header.Set("X-Signature", "bad")
		if i == 30 {
			mac := hmac.New(sha256.New, []byte("key"))
			mac.Write([]byte("body"))
			r.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := http.StatusUnauthorized
		if i == 30 {
			want = http.StatusAccepted
		}
		if w.Code != want {
			t.Fatalf("request %d: status %d, want %d", i, w.Code, want)
		}
	}
}

// bodyKey hashes exactly the signed MAC input.
func bodyKey(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

package source

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

func TestWebhookToken(t *testing.T) {
	calls := 0
	h := NewWebhook(WebhookOptions{Name: "gl", Secret: "s3cret", Signature: "token", TokenHeader: "X-Gitlab-Token"}, func(_ context.Context, ev Event, id string) (bool, error) {
		calls++
		if ev.Headers["x-gitlab-token"] != "" || id != bodyKey(`{"n":1}`) {
			t.Errorf("token leaked or bad key: %+v %q", ev.Headers, id)
		}
		return calls > 1, nil
	})
	do := func(token string, set bool) int {
		r := httptest.NewRequest(http.MethodPost, "/hook/gl", strings.NewReader(`{"n":1}`))
		if set {
			r.Header.Set("X-Gitlab-Token", token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, tc := range []struct {
		token string
		set   bool
		want  int
	}{{"s3cret", true, 202}, {"s3cret", true, 409}, {"wrong", true, 401}, {"", true, 401}, {"", false, 401}, {"s3cret ", true, 401}} {
		if got := do(tc.token, tc.set); got != tc.want {
			t.Errorf("token %q set=%v: %d, want %d", tc.token, tc.set, got, tc.want)
		}
	}
}

func TestWebhookStandard(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Vector shape from standardwebhooks.com: whsec_ + base64 key, msg id.ts.body.
	raw := []byte("0123456789abcdef0123456789abcdef")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(raw)
	sign := func(key []byte, id, ts, body string) string {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(id + "." + ts + "." + body))
		return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	body := `{"n":1}`
	calls := 0
	h := NewWebhook(WebhookOptions{Name: "w", Secret: secret, Signature: "standard-webhooks", Now: func() time.Time { return now }}, func(_ context.Context, ev Event, id string) (bool, error) {
		calls++
		if id != "msg_1" || ev.Headers["webhook-signature"] != "" || ev.Headers["webhook-id"] != "msg_1" {
			t.Errorf("bad delivery: %q %+v", id, ev.Headers)
		}
		return calls > 1, nil
	})
	do := func(id, ts, sig string) int {
		r := httptest.NewRequest(http.MethodPost, "/hook/w", strings.NewReader(body))
		if id != "" {
			r.Header.Set("Webhook-Id", id)
		}
		if ts != "" {
			r.Header.Set("Webhook-Timestamp", ts)
		}
		if sig != "" {
			r.Header.Set("Webhook-Signature", sig)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	ts := "1700000000"
	good := sign(raw, "msg_1", ts, body)
	other := sign([]byte("another-key"), "msg_1", ts, body)
	for _, tc := range []struct {
		name, id, ts, sig string
		want              int
	}{
		{"valid", "msg_1", ts, good, 202},
		{"replay", "msg_1", ts, good, 409},
		{"multiple, one valid", "msg_1", ts, other + " v2,xxx " + good, 409}, // valid, so a replay
		{"wrong key", "msg_1", ts, other, 401},
		{"tampered id", "msg_2", ts, good, 401},
		{"tampered ts", "msg_1", "1700000001", good, 401},
		{"missing sig", "msg_1", ts, "", 401},
		{"missing id", "", ts, good, 401},
		{"missing ts", "msg_1", "", good, 401},
		{"malformed", "msg_1", ts, "v1,!!!", 401},
		{"no version", "msg_1", ts, strings.TrimPrefix(good, "v1,"), 401},
		{"v2 only", "msg_1", ts, "v2," + strings.TrimPrefix(good, "v1,"), 401},
		{"skew past", "msg_1", "1699999699", sign(raw, "msg_1", "1699999699", body), 401},
		{"skew future", "msg_1", "1700000301", sign(raw, "msg_1", "1700000301", body), 401},
		{"edge past", "msg_1", "1699999700", sign(raw, "msg_1", "1699999700", body), 409},
	} {
		if got := do(tc.id, tc.ts, tc.sig); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	// "multiple, one valid" must be verified, not just replayed: use a fresh handler.
	calls = 0
	if got := do("msg_1", ts, other+" v2,xxx "+good); got != 202 {
		t.Errorf("multiple signatures, one valid: %d, want 202", got)
	}
}

// The reference vector published with the Standard Webhooks libraries.
func TestStandardWebhooksReferenceVector(t *testing.T) {
	body := []byte(`{"test": 2432232314}`)
	r := httptest.NewRequest("POST", "/hook/x", nil)
	r.Header.Set("Webhook-Id", "msg_p5jXN8AQM9LWM0D4loKWxJek")
	r.Header.Set("Webhook-Timestamp", "1614265330")
	r.Header.Set("Webhook-Signature", "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=")
	if _, ok := verifyStandard("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", r, body, time.Unix(1614265330, 0)); !ok {
		t.Fatal("reference vector rejected")
	}
	r.Header.Set("Webhook-Signature", "v1,g0hM9SsF+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=")
	if _, ok := verifyStandard("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", r, body, time.Unix(1614265330, 0)); ok {
		t.Fatal("tampered signature accepted")
	}
}

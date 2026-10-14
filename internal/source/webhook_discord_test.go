package source

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var discordNow = time.Unix(1_800_000_000, 0)

type discordReq struct {
	body, ts, sig string // sig "" signs for real; ts "" uses now
	drop          string // a header to leave out
}

// discordCall posts one request to a discord handler and returns the response
// and the delivered events. seen carries ids across calls.
func discordCall(t *testing.T, priv ed25519.PrivateKey, secret, reply string, seen map[string]bool, q discordReq) (*httptest.ResponseRecorder, []Event) {
	t.Helper()
	if q.ts == "" {
		q.ts = strconv.FormatInt(discordNow.Unix(), 10)
	}
	if q.sig == "" {
		q.sig = hex.EncodeToString(ed25519.Sign(priv, []byte(q.ts+q.body)))
	}
	var got []Event
	h := NewWebhook(WebhookOptions{Name: "d", Secret: secret, Signature: "ed25519", Format: "discord", ReplyText: reply, Now: func() time.Time { return discordNow }},
		func(_ context.Context, ev Event, id string) (bool, error) {
			if seen[id] {
				return true, nil
			}
			seen[id] = true
			got = append(got, ev)
			return false, nil
		})
	r := httptest.NewRequest(http.MethodPost, "/hook/d", strings.NewReader(q.body))
	r.Header.Set("X-Signature-Ed25519", q.sig)
	r.Header.Set("X-Signature-Timestamp", q.ts)
	r.Header.Del(q.drop)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w, got
}

func TestDiscordWebhook(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key := hex.EncodeToString(pub)
	stale := strconv.FormatInt(discordNow.Add(-6*time.Minute).Unix(), 10)
	for _, tc := range []struct {
		name  string
		q     discordReq
		reply string
		code  int
		body  string
		n     int // deliveries
	}{
		{"ping", discordReq{body: `{"type":1}`}, "", 200, `{"type":1}`, 0},
		{"command", discordReq{body: `{"type":2,"id":"1","data":{"name":"ask"}}`}, "", 200, `{"type":5}`, 1},
		{"component", discordReq{body: `{"type":3,"id":"1"}`}, "", 200, `{"type":6}`, 1},
		{"modal", discordReq{body: `{"type":5,"id":"1"}`}, "", 200, `{"type":5}`, 1},
		{"reply", discordReq{body: `{"type":2,"id":"1"}`}, "ok", 200, `{"data":{"content":"ok"},"type":4}`, 1},
		{"autocomplete", discordReq{body: `{"type":4,"id":"1"}`}, "", 200, `{"data":{"choices":[]},"type":8}`, 0},
		{"no id", discordReq{body: `{"type":2}`}, "", 400, "", 0},
		{"not an object", discordReq{body: `[1]`}, "", 400, "", 0},
		{"no type", discordReq{body: `{"id":"1"}`}, "", 400, "", 0},
		{"bad signature", discordReq{body: `{"type":1}`, sig: strings.Repeat("00", 64)}, "", 401, "", 0},
		{"short signature", discordReq{body: `{"type":1}`, sig: "00"}, "", 401, "", 0},
		{"stale", discordReq{body: `{"type":1}`, ts: stale}, "", 401, "", 0},
		{"missing signature", discordReq{body: `{"type":1}`, drop: "X-Signature-Ed25519"}, "", 401, "", 0},
		{"missing timestamp", discordReq{body: `{"type":1}`, drop: "X-Signature-Timestamp"}, "", 401, "", 0},
	} {
		w, got := discordCall(t, priv, key, tc.reply, map[string]bool{}, tc.q)
		if tc.q.drop != "" { // discordCall signs before dropping; resend without the header
			w = dropped(t, priv, key, tc.q)
		}
		if w.Code != tc.code || (tc.body != "" && strings.TrimSpace(w.Body.String()) != tc.body) || len(got) != tc.n {
			t.Errorf("%s: %d %q, %d delivered", tc.name, w.Code, w.Body.String(), len(got))
		}
		for _, ev := range got {
			if _, ok := ev.Headers["x-signature-ed25519"]; ok {
				t.Errorf("%s: signature header reached the event", tc.name)
			}
		}
	}
}

// dropped is discordCall for a request missing one header.
func dropped(t *testing.T, priv ed25519.PrivateKey, key string, q discordReq) *httptest.ResponseRecorder {
	t.Helper()
	h := NewWebhook(WebhookOptions{Name: "d", Secret: key, Signature: "ed25519", Format: "discord", Now: func() time.Time { return discordNow }},
		func(context.Context, Event, string) (bool, error) { t.Error("delivered"); return false, nil })
	ts := strconv.FormatInt(discordNow.Unix(), 10)
	r := httptest.NewRequest(http.MethodPost, "/hook/d", strings.NewReader(q.body))
	r.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(priv, []byte(ts+q.body))))
	r.Header.Set("X-Signature-Timestamp", ts)
	r.Header.Del(q.drop)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDiscordDuplicateAndBadKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	key := hex.EncodeToString(pub)
	seen := map[string]bool{}
	q := discordReq{body: `{"type":2,"id":"7"}`}
	w1, g1 := discordCall(t, priv, key, "", seen, q)
	w2, g2 := discordCall(t, priv, key, "", seen, q)
	if w1.Code != 200 || w2.Code != 200 || w1.Body.String() != w2.Body.String() || len(g1) != 1 || len(g2) != 0 {
		t.Errorf("duplicate: %d/%d %q/%q %d/%d", w1.Code, w2.Code, w1.Body, w2.Body, len(g1), len(g2))
	}
	for _, bad := range []string{"nothex-zz", "abcd"} {
		w, _ := discordCall(t, priv, bad, "", map[string]bool{}, q)
		if w.Code != 500 || strings.Contains(w.Body.String(), bad) {
			t.Errorf("bad key %q: %d %q", bad, w.Code, w.Body)
		}
	}
}

func TestEd25519Generic(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	var got []Event
	h := NewWebhook(WebhookOptions{Name: "g", Secret: hex.EncodeToString(pub), Signature: "ed25519", Now: func() time.Time { return discordNow }},
		func(_ context.Context, ev Event, _ string) (bool, error) { got = append(got, ev); return false, nil })
	ts, body := strconv.FormatInt(discordNow.Unix(), 10), `{"a":1}`
	r := httptest.NewRequest(http.MethodPost, "/hook/g", strings.NewReader(body))
	r.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(priv, []byte(ts+body))))
	r.Header.Set("X-Signature-Timestamp", ts)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 202 || len(got) != 1 {
		t.Fatalf("%d, %d delivered", w.Code, len(got))
	}
	if _, ok := got[0].Headers["x-signature-ed25519"]; ok {
		t.Error("signature header reached the event")
	}
}

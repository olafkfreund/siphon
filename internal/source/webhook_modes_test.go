package source

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// post sends body with headers to a webhook built from o and returns the
// status and the delivery id it was given.
func post(t *testing.T, o WebhookOptions, now time.Time, body string, hdr map[string]string) (int, string) {
	t.Helper()
	o.Name = "w"
	o.Now = func() time.Time { return now }
	var got string
	h := NewWebhook(o, func(_ context.Context, _ Event, id string) (bool, error) { got = id; return false, nil })
	r := httptest.NewRequest(http.MethodPost, "/hook/w", strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, got
}

// The vector is the worked example in Slack's docs:
// https://api.slack.com/authentication/verifying-requests-from-slack
func TestWebhookSlack(t *testing.T) {
	const secret = "8f742231b10e8888abcd99yyyzzz85a5"
	const ts = "1531420618"
	const body = "token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J&team_domain=testteamnow&channel_id=G8PSS9T3V&channel_name=foobar&user_id=U2CERLKJA&user_name=roadrunner&command=%2Fwebhook-collect&text=&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2FT1DC2JH3J%2F397700885554%2F96rGlfmibIGlgcZRskXaIFfN&trigger_id=398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c"
	const sig = "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503"
	now := time.Unix(1531420618+60, 0)
	o := WebhookOptions{Secret: secret, Signature: "slack"}
	hdr := map[string]string{"x-slack-signature": sig, "X-Slack-Request-Timestamp": ts}
	if code, id := post(t, o, now, body, hdr); code != 202 || id == "" {
		t.Fatalf("good: %d %q", code, id)
	}
	if code, _ := post(t, o, now, body+"x", hdr); code != 401 {
		t.Errorf("tampered body: %d", code)
	}
	if code, _ := post(t, WebhookOptions{Secret: "other", Signature: "slack"}, now, body, hdr); code != 401 {
		t.Errorf("wrong secret: %d", code)
	}
	if code, _ := post(t, o, now.Add(10*time.Minute), body, hdr); code != 401 {
		t.Errorf("expired: %d", code)
	}
	if code, _ := post(t, o, now, body, map[string]string{"X-Slack-Signature": sig}); code != 401 {
		t.Errorf("no timestamp: %d", code)
	}
}

// Stripe: https://docs.stripe.com/webhooks#verify-manually , HMAC-SHA256 of
// "t.payload"; the vectors are openssl dgst -sha256 -hmac of that string.
func TestWebhookStripe(t *testing.T) {
	const body = `{"id":"evt_1"}`
	const ts = "1700000000"
	const cur = "248a374f50f943a28b0f6ab50faf9a7e7e29b710fa26df9fb1618b9bf8ea9c9a"  // whsec_test_secret
	const prev = "ef1b1b7e4b312671a75ef2a03e10529f7834a6528a49db77ad6568c8c8017843" // whsec_old
	now := time.Unix(1700000000, 0)
	o := WebhookOptions{Secret: "whsec_test_secret", Signature: "stripe"}
	h := func(s string) map[string]string { return map[string]string{"Stripe-Signature": s} }
	if code, id := post(t, o, now, body, h("t="+ts+",v1="+cur+",v0=ignored")); code != 202 || id == "" {
		t.Fatalf("good: %d %q", code, id)
	}
	// A rotated secret: Stripe signs with both, the second v1 matches.
	if code, _ := post(t, WebhookOptions{Secret: "whsec_old", Signature: "stripe"}, now, body, h("t="+ts+",v1="+cur+",v1="+prev)); code != 202 {
		t.Errorf("rotated: %d", code)
	}
	if code, _ := post(t, o, now, body, h("t="+ts+",v1="+prev)); code != 401 {
		t.Errorf("wrong signature: %d", code)
	}
	if code, _ := post(t, o, now.Add(6*time.Minute), body, h("t="+ts+",v1="+cur)); code != 401 {
		t.Errorf("expired: %d", code)
	}
	if code, _ := post(t, o, now, body, h("v1="+cur)); code != 401 {
		t.Errorf("no t: %d", code)
	}
}

func TestWebhookSHA256PrefixListAndSeparator(t *testing.T) {
	now := time.Unix(1700000000, 0)
	// PagerDuty: X-PagerDuty-Signature: v1=hex[,v1=hex...] (HMAC-SHA256 of the body).
	// https://developer.pagerduty.com/docs/verifying-signatures
	const body = `{"event":1}`
	const cur = "8567f9933e14dc3d2fe0305fa1c08da9371772b1703c379a708a1dac25ab999c" // pdkey
	const old = "7243821365e71c131f6fd2bfe8d57433adc394b90f262987b9abebc0f0c3b3f3" // pdold
	pd := WebhookOptions{Secret: "pdkey", Signature: "sha256", SigHeader: "X-PagerDuty-Signature", SigPrefix: "v1="}
	for name, v := range map[string]string{"single": "v1=" + cur, "list": "v1=" + old + ", v1=" + cur} {
		if code, _ := post(t, pd, now, body, map[string]string{"x-pagerduty-signature": v}); code != 202 {
			t.Errorf("%s: %d", name, code)
		}
	}
	for name, v := range map[string]string{"bare hex": cur, "wrong": "v1=" + old, "empty": ""} {
		if code, _ := post(t, pd, now, body, map[string]string{"X-PagerDuty-Signature": v}); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
	// Grafana: HMAC-SHA256 of "timestamp:body".
	const gbody = `{"a":1}`
	const graf = "22fa362e179e150a7005766ee09f7a41a115d22c6499140a58c3eb23b8e70198" // gkey
	g := WebhookOptions{Secret: "gkey", Signature: "sha256", SigHeader: "X-Grafana-Alerting-Signature", TimestampHeader: "X-Grafana-Alerting-Timestamp", TimestampSep: ":"}
	hdr := map[string]string{"X-Grafana-Alerting-Signature": graf, "X-Grafana-Alerting-Timestamp": "1700000000"}
	if code, _ := post(t, g, now, gbody, hdr); code != 202 {
		t.Errorf("grafana: %d", code)
	}
	g.TimestampSep = "" // the default "." must not match a ":" signature
	if code, _ := post(t, g, now, gbody, hdr); code != 401 {
		t.Errorf("default separator accepted a colon signature: %d", code)
	}
}

// Slack's Request URL check: a signed url_verification is answered with its
// challenge and never delivered; an unsigned one is refused.
func TestSlackURLVerification(t *testing.T) {
	now := time.Unix(1700000000, 0)
	body := `{"type":"url_verification","challenge":"chal-123","token":"x"}`
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte("slack-secret"))
	mac.Write([]byte("v0:" + ts + ":" + body))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	delivered := false
	h := NewWebhook(WebhookOptions{Name: "s", Secret: "slack-secret", Signature: "slack", Now: func() time.Time { return now }},
		func(context.Context, Event, string) (bool, error) { delivered = true; return false, nil })
	send := func(sig string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/hook/s", strings.NewReader(body))
		r.Header.Set("X-Slack-Request-Timestamp", ts)
		r.Header.Set("X-Slack-Signature", sig)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := send(sig); w.Code != 200 || w.Body.String() != "chal-123" || delivered {
		t.Fatalf("signed verification: %d %q delivered=%v", w.Code, w.Body.String(), delivered)
	}
	if w := send("v0=00"); w.Code != http.StatusUnauthorized || w.Body.String() == "chal-123" {
		t.Fatalf("unsigned verification answered: %d %q", w.Code, w.Body.String())
	}
}

// A provider retry re-signed with a new timestamp carries the same body: a duplicate.
func TestWebhookSlackStripeRetryIsDuplicate(t *testing.T) {
	const secret, body = "k", `{"id":"evt_1"}`
	now := time.Unix(1700000100, 0)
	sign := func(msg string) string {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte(msg))
		return hex.EncodeToString(m.Sum(nil))
	}
	for _, mode := range []string{"slack", "stripe"} {
		seen := map[string]bool{}
		h := NewWebhook(WebhookOptions{Name: "w", Secret: secret, Signature: mode, Now: func() time.Time { return now }},
			func(_ context.Context, _ Event, id string) (bool, error) {
				d := seen[id]
				seen[id] = true
				return d, nil
			})
		var codes []int
		for _, ts := range []string{"1700000000", "1700000050"} {
			r := httptest.NewRequest(http.MethodPost, "/hook/w", strings.NewReader(body))
			if mode == "slack" {
				r.Header.Set("X-Slack-Request-Timestamp", ts)
				r.Header.Set("X-Slack-Signature", "v0="+sign("v0:"+ts+":"+body))
			} else {
				r.Header.Set("Stripe-Signature", "t="+ts+",v1="+sign(ts+"."+body))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			codes = append(codes, w.Code)
		}
		if codes[0] != 202 || codes[1] != 409 {
			t.Errorf("%s: %v", mode, codes)
		}
	}
}

// ServiceNow's generateMac signs id.ts.body with the base64-decoded hex key;
// this pins that a delivery signed that way passes standard-webhooks.
func TestWebhookServiceNowSigning(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	k := hex.EncodeToString(raw)
	key, err := base64.StdEncoding.DecodeString(k)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(key []byte, id, ts, body string) string {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(id + "." + ts + "." + body))
		return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	id := hex.EncodeToString(raw[:16]) + ":3"
	body := `{"number":"INC0010001"}`
	calls, seen := 0, ""
	h := NewWebhook(WebhookOptions{Name: "w", Secret: "whsec_" + k, Signature: "standard-webhooks", Now: func() time.Time { return now }}, func(_ context.Context, _ Event, got string) (bool, error) {
		calls++
		seen = got
		return calls > 1, nil
	})
	do := func(ts, sig, b string) int {
		r := httptest.NewRequest(http.MethodPost, "/hook/w", strings.NewReader(b))
		r.Header.Set("Webhook-Id", id)
		r.Header.Set("Webhook-Timestamp", ts)
		r.Header.Set("Webhook-Signature", sig)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	good := sign(key, id, ts, body)
	old := strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10)
	if c := do(ts, good, body+" "); c != 401 {
		t.Errorf("changed body: %d, want 401", c)
	}
	if c := do(old, sign(key, id, old, body), body); c != 401 {
		t.Errorf("6 minutes old: %d, want 401", c)
	}
	if c := do(ts, sign([]byte(k), id, ts, body), body); c != 401 {
		t.Errorf("raw ASCII key: %d, want 401", c)
	}
	if calls != 0 {
		t.Fatalf("refused deliveries reached the handler: %d", calls)
	}
	if c := do(ts, good, body); c != 202 || seen != id {
		t.Errorf("valid: %d id %q, want 202 id %q", c, seen, id)
	}
	if c := do(ts, good, body); c != 409 || calls != 2 {
		t.Errorf("replay: %d calls %d, want 409 and 2", c, calls)
	}
}

// bodyTokenPost sends body as ctype to a body-token webhook and returns the
// status, response body, delivered event and delivery id.
func bodyTokenPost(t *testing.T, o WebhookOptions, ctype, body string, seen map[string]bool) (int, string, *Event, string) {
	t.Helper()
	o.Name, o.Signature, o.Secret = "w", "body-token", "s3cret"
	var ev *Event
	var id string
	h := NewWebhook(o, func(_ context.Context, e Event, i string) (bool, error) {
		ev, id = &e, i
		dup := seen[i]
		if seen != nil {
			seen[i] = true
		}
		return dup, nil
	})
	r := httptest.NewRequest(http.MethodPost, "/hook/w", strings.NewReader(body))
	r.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String(), ev, id
}

func TestWebhookBodyToken(t *testing.T) {
	const form = "application/x-www-form-urlencoded"
	// Form and JSON are accepted; the token is gone, the rest is there.
	code, _, ev, _ := bodyTokenPost(t, WebhookOptions{}, form+"; charset=utf-8", "token=s3cret&command=%2Fask&text=hi&tag=a&tag=b", nil)
	m, _ := ev.Data.(map[string]any)
	if code != 202 || ev == nil || m["token"] != nil || m["command"] != "/ask" || m["text"] != "hi" {
		t.Fatalf("form: %d %v", code, ev)
	}
	if l, ok := m["tag"].([]any); !ok || len(l) != 2 || l[0] != "a" || l[1] != "b" {
		t.Errorf("repeated key: %#v", m["tag"])
	}
	code, _, ev, _ = bodyTokenPost(t, WebhookOptions{}, "application/json", `{"token":"s3cret","text":"hi"}`, nil)
	m, _ = ev.Data.(map[string]any)
	if code != 202 || m["token"] != nil || m["text"] != "hi" {
		t.Fatalf("json: %d %v", code, ev)
	}
	// A wrong, missing, numeric or non-object token is refused and never delivered.
	for _, b := range []string{`{"token":"nope"}`, `{"text":"hi"}`, `{"token":123}`, `["s3cret"]`, `not json`} {
		if code, out, ev, _ := bodyTokenPost(t, WebhookOptions{}, "application/json", b, nil); code != 401 || ev != nil || out != "" {
			t.Errorf("%s: %d %q delivered=%v", b, code, out, ev != nil)
		}
	}
	// Two token fields: a repeated form key is a list and is refused; a repeated
	// JSON key keeps the last value, and none is left in the event.
	if code, _, ev, _ := bodyTokenPost(t, WebhookOptions{}, form, "token=s3cret&token=x", nil); code != 401 || ev != nil {
		t.Errorf("repeated form token: %d delivered=%v", code, ev != nil)
	}
	if code, _, ev, _ := bodyTokenPost(t, WebhookOptions{}, "application/json", `{"token":"x","token":"s3cret","text":"hi"}`, nil); code != 202 || ev.Data.(map[string]any)["token"] != nil {
		t.Errorf("repeated JSON token: %d", code)
	}
	// A custom field.
	if code, _, ev, _ := bodyTokenPost(t, WebhookOptions{TokenField: "key"}, "application/json", `{"key":"s3cret","token":"x"}`, nil); code != 202 || ev.Data.(map[string]any)["token"] != "x" {
		t.Errorf("custom field: %d", code)
	}
	// reply_text answers 200 with JSON.
	if code, out, _, _ := bodyTokenPost(t, WebhookOptions{ReplyText: "Working on it"}, "application/json", `{"token":"s3cret"}`, nil); code != 200 || out != `{"text":"Working on it"}` {
		t.Errorf("reply: %d %q", code, out)
	}
	// The same delivery twice is a duplicate, with or without the token changing nothing else.
	seen := map[string]bool{}
	bodyTokenPost(t, WebhookOptions{}, "application/json", `{"token":"s3cret","text":"hi"}`, seen)
	if code, _, _, _ := bodyTokenPost(t, WebhookOptions{}, "application/json", `{"text":"hi","token":"s3cret"}`, seen); code != 409 {
		t.Errorf("duplicate: %d", code)
	}
}

// Form bodies now decode for every mode; JSON deliveries are unchanged.
func TestWebhookFormAndJSONOtherModes(t *testing.T) {
	var got Event
	h := NewWebhook(WebhookOptions{Name: "w", Secret: "k", Signature: "token", TokenHeader: "X-T"}, func(_ context.Context, e Event, _ string) (bool, error) { got = e; return false, nil })
	r := httptest.NewRequest(http.MethodPost, "/hook/w", strings.NewReader("a=1&b=2"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-T", "k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if m, _ := got.Data.(map[string]any); w.Code != 202 || m["a"] != "1" || m["raw"] != nil {
		t.Errorf("form under token mode: %d %v", w.Code, got.Data)
	}
	body := `{"a":1}`
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write([]byte(body))
	code, _ := post(t, WebhookOptions{Secret: "k", Signature: "github"}, time.Now(), body, map[string]string{"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil))})
	if code != 202 {
		t.Errorf("github json: %d", code)
	}
}

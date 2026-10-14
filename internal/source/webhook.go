package source

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type WebhookOptions struct {
	Name            string
	Secret          string
	Signature       string
	SigHeader       string
	TokenHeader     string
	TimestampHeader string
	TokenField      string // body-token: the body field holding the token (default "token")
	ReplyText       string // body-token: answer 200 {"text": …} instead of 202
	SigPrefix       string // sha256: the header may hold a comma-separated list of PREFIX+hex; any match counts
	TimestampSep    string // sha256 with a timestamp: what joins timestamp and body (default ".")
	IDHeader        string
	AllowQueryToken bool            // matrix: also accept ?access_token= as the token
	Format          string          // "eventgrid": answer Azure Event Grid's handshakes and deliver each event of a batch; "discord" (signature ed25519): answer Discord's PING and interactions; "notion": capture Notion's verification token until a secret is set
	Audience        string          // google-oidc: the token's aud
	ServiceAccount  string          // google-oidc: the email the token must carry
	Google          *GoogleVerifier // google-oidc
	// OnVerificationToken receives Notion's verification token (format notion, no secret).
	OnVerificationToken func(string)
	MaxBody             int64
	Now                 func() time.Time
	// PreLimit counts every request before it is read or verified (a flood of
	// unauthenticated posts); Limit counts verified deliveries. Both are per
	// source; nil means a private limiter (fine for tests, not for per-request use).
	PreLimit, Limit *Limiter
	// OnReject is told of a refused delivery: the status and a short fixed
	// reason, never anything from the request. Rate-limit refusals of
	// unverified floods (PreLimit) are not reported.
	OnReject func(status int, reason string)
}

// Limiter is a token bucket. A webhook's limiters must outlive the handler
// (which is rebuilt per request), so callers keep them and pass them in.
type Limiter struct {
	mu          sync.Mutex
	tokens      float64
	burst, rate float64
	last        time.Time
}

func NewLimiter(burst, perSecond float64) *Limiter {
	return &Limiter{tokens: burst, burst: burst, rate: perSecond}
}

func (l *Limiter) Allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() {
		if elapsed := now.Sub(l.last).Seconds(); elapsed > 0 {
			l.tokens = min(l.burst, l.tokens+elapsed*l.rate)
			l.last = now
		}
	} else {
		l.last = now
	}
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

type Deliver func(ctx context.Context, ev Event, deliveryID string) (dup bool, err error)

var notionToken = regexp.MustCompile(`^secret_[A-Za-z0-9]{20,100}$`)

func NewWebhook(o WebhookOptions, deliver Deliver) http.Handler {
	if o.MaxBody <= 0 {
		o.MaxBody = 1 << 20
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Signature != "sha256" {
		o.TimestampHeader = "" // github signs none; token and standard-webhooks bring their own
	}
	if o.PreLimit == nil {
		o.PreLimit = NewLimiter(100, 50)
	}
	if o.Limit == nil {
		o.Limit = NewLimiter(20, 10)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		matrix := o.Format == "matrix"
		if strings.Contains(r.URL.Path, matrixPrefix) != matrix {
			http.NotFound(w, r) // the application-service paths answer only on a matrix source, and a matrix source answers only them
			return
		}
		if r.Method != http.MethodPost && !(r.Method == http.MethodOptions && o.Format == "eventgrid") && !(matrix && (r.Method == http.MethodPut || r.Method == http.MethodGet)) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		status := func(code int) {
			if matrix {
				matrixError(w, code, "")
				return
			}
			w.WriteHeader(code)
		}
		reject := func(code int, reason string) {
			if o.OnReject != nil {
				o.OnReject(code, reason)
			}
			status(code)
		}
		now := o.Now()
		if !o.PreLimit.Allow(now) {
			status(http.StatusTooManyRequests)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, o.MaxBody+1))
		if err != nil {
			reject(http.StatusBadRequest, "unreadable body")
			return
		}
		if int64(len(body)) > o.MaxBody {
			reject(http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		if o.Format == "notion" && o.Secret == "" {
			// Awaiting Notion's one-time token: take exactly {"verification_token":"secret_…"}, deliver nothing.
			var m map[string]any
			ok := json.Unmarshal(body, &m) == nil && len(m) == 1
			tok, _ := m["verification_token"].(string)
			if !ok || !notionToken.MatchString(tok) {
				reject(http.StatusUnauthorized, "awaiting verification token")
				return
			}
			if o.OnVerificationToken != nil {
				o.OnVerificationToken(tok)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		header := o.SigHeader
		var signed []byte
		var key string
		var data any
		switch o.Signature {
		case "body-token":
			header = "" // the token is in the body, so no header is dropped
			field := o.TokenField
			if field == "" {
				field = "token"
			}
			data = decodeBody(r, body)
			m, _ := data.(map[string]any) // a JSON array or scalar has no token
			tok, _ := m[field].(string)
			a, b := sha256.Sum256([]byte(tok)), sha256.Sum256([]byte(o.Secret))
			if o.Secret == "" || subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
				reject(http.StatusUnauthorized, "token missing or wrong")
				return
			}
			delete(m, field)
			enc, _ := json.Marshal(m) // map[string]any from JSON or a form always encodes
			sum := sha256.Sum256(enc)
			key = hex.EncodeToString(sum[:])
		case "token":
			header = o.TokenHeader
			got, want := r.Header.Get(header), o.Secret
			if matrix {
				want = "Bearer " + o.Secret
				if q := r.URL.Query().Get("access_token"); o.AllowQueryToken && q != "" && got == "" {
					got, want = q, o.Secret
				}
			}
			a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
			ok := o.Secret != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1
			if !ok {
				if matrix && got != "" {
					reject(http.StatusForbidden, "token wrong")
				} else {
					reject(http.StatusUnauthorized, "token missing or wrong")
				}
				return
			}
			signed = body
		case "slack":
			// https://api.slack.com/authentication/verifying-requests-from-slack
			header = "X-Slack-Signature"
			ts := r.Header.Get("X-Slack-Request-Timestamp")
			if !freshTS(ts, now) {
				reject(http.StatusUnauthorized, "signature or timestamp invalid")
				return
			}
			signed = body
			if !macMatch(o.Secret, []byte("v0:"+ts+":"+string(body)), []string{r.Header.Get(header)}, "v0=") {
				reject(http.StatusUnauthorized, "signature or timestamp invalid")
				return
			}
			// Slack verifies a new Request URL with a signed url_verification
			// event that must be answered with its challenge, not delivered.
			var v struct{ Type, Challenge string }
			if json.Unmarshal(body, &v) == nil && v.Type == "url_verification" {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, v.Challenge)
				return
			}
		case "stripe":
			// https://docs.stripe.com/webhooks#verify-manually
			header = "Stripe-Signature"
			ts, sigs := parseStripe(r.Header.Get(header))
			signed = body
			if !freshTS(ts, now) || !macMatch(o.Secret, []byte(ts+"."+string(body)), sigs, "") {
				reject(http.StatusUnauthorized, "signature or timestamp invalid")
				return
			}
		case "ed25519":
			// https://discord.com/developers/docs/interactions/overview#setting-up-an-endpoint
			header = "X-Signature-Ed25519"
			pub, err := hex.DecodeString(o.Secret)
			if err != nil || len(pub) != ed25519.PublicKeySize {
				reject(http.StatusInternalServerError, "public key misconfigured") // never echo the key
				return
			}
			ts := r.Header.Get("X-Signature-Timestamp")
			sig, err := hex.DecodeString(r.Header.Get(header))
			if err != nil || len(sig) != ed25519.SignatureSize || !freshTS(ts, now) || !ed25519.Verify(pub, append([]byte(ts), body...), sig) {
				reject(http.StatusUnauthorized, "signature or timestamp invalid")
				return
			}
			signed = body
		case "google-oidc":
			// https://cloud.google.com/pubsub/docs/authenticate-push-subscriptions
			header = "Authorization"
			tok, ok := strings.CutPrefix(r.Header.Get(header), "Bearer ")
			var email string
			if ok && o.Google != nil {
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				email, err = o.Google.Verify(ctx, tok, o.Audience)
				cancel()
			}
			if !ok || o.Google == nil || err != nil || !strings.EqualFold(email, o.ServiceAccount) {
				reject(http.StatusUnauthorized, "token invalid")
				return
			}
			signed = body
		case "teams-hmac":
			// Teams outgoing webhook: Authorization: HMAC <base64 HMAC-SHA256(base64-decoded token, body)>
			header = "Authorization"
			k, err := base64.StdEncoding.DecodeString(o.Secret)
			if err != nil || len(k) == 0 {
				reject(http.StatusInternalServerError, "secret misconfigured") // never echo the key
				return
			}
			sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(r.Header.Get(header), "HMAC "))
			mac := hmac.New(sha256.New, k)
			mac.Write(body)
			if !strings.HasPrefix(r.Header.Get(header), "HMAC ") || err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
				reject(http.StatusUnauthorized, "signature invalid")
				return
			}
			signed = body
			var v struct{ ID string }
			if json.Unmarshal(body, &v) == nil {
				key = v.ID // a non-string id fails the decode, so the body hash is used
			}
		case "standard-webhooks":
			header = "Webhook-Signature"
			var ok bool
			if key, ok = verifyStandard(o.Secret, r, body, now); !ok {
				reject(http.StatusUnauthorized, "signature or timestamp invalid")
				return
			}
			signed = body
		default:
			if o.Signature == "github" {
				header = "X-Hub-Signature-256"
			}
			sig := r.Header.Get(header)
			var cands []string
			prefix := ""
			switch {
			case o.Signature == "github":
				if !strings.HasPrefix(sig, "sha256=") {
					reject(http.StatusUnauthorized, "signature missing")
					return
				}
				prefix, cands = "sha256=", []string{sig}
			case o.Signature == "sha256" && o.SigPrefix != "":
				prefix, cands = o.SigPrefix, strings.Split(sig, ",")
			case o.Signature == "sha256":
				prefix, cands = "sha256=", []string{sig}
			default:
				reject(http.StatusUnauthorized, "no signature scheme configured")
				return
			}
			signed = body
			if o.Signature == "sha256" && o.TimestampHeader != "" {
				sep := o.TimestampSep
				if sep == "" {
					sep = "."
				}
				signed = append(append([]byte(r.Header.Get(o.TimestampHeader)), sep...), body...)
			}
			if !macMatch(o.Secret, signed, cands, prefix) {
				reject(http.StatusUnauthorized, "signature does not match (wrong secret?)")
				return
			}
			if o.TimestampHeader != "" {
				seconds, err := strconv.ParseInt(r.Header.Get(o.TimestampHeader), 10, 64)
				if err != nil || now.Sub(time.Unix(seconds, 0)) > 5*time.Minute || time.Unix(seconds, 0).Sub(now) > 5*time.Minute {
					reject(http.StatusUnauthorized, "timestamp missing or too old")
					return
				}
			}
		}
		id := r.Header.Get(o.IDHeader)
		if o.IDHeader != "" && id == "" {
			reject(http.StatusBadRequest, "delivery id header missing")
			return
		}
		if o.Format != "discord" && !o.Limit.Allow(now) { // discord counts per interaction, after its PING; a batch is one request, so a batch over the burst still gets through
			reject(http.StatusTooManyRequests, "rate limited")
			return
		}
		// Slack and Stripe too: the key is the body alone, so a retry re-signed with
		// a new timestamp is a duplicate. Identical bodies replay only after the
		// seen_event TTL (7 days). Delivery IDs are not signed.
		if key == "" {
			sum := sha256.Sum256(signed)
			key = hex.EncodeToString(sum[:])
		}
		id = key
		headers := make(map[string]string, len(r.Header))
		connectionHeaders := make(map[string]bool)
		for _, value := range r.Header.Values("Connection") {
			for _, name := range strings.Split(value, ",") {
				connectionHeaders[strings.ToLower(strings.TrimSpace(name))] = true
			}
		}
		for k := range r.Header {
			name := strings.ToLower(k)
			if connectionHeaders[name] {
				continue
			}
			switch name {
			case strings.ToLower(header), "x-hub-signature-256", "authorization", "cookie", "proxy-authorization", "proxy-authenticate", "connection", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade":
			default:
				headers[name] = r.Header.Get(k)
			}
		}
		if o.Format == "eventgrid" {
			serveEventGrid(w, r, o, now, body, headers, deliver, reject)
			return
		}
		if matrix {
			serveMatrix(w, r, o, now, body, headers, deliver)
			return
		}
		if o.Format == "pubsub" {
			servePubSub(w, r, o, now, body, headers, deliver, reject)
			return
		}
		if o.Format == "discord" {
			serveDiscord(w, r, o, now, body, headers, deliver, reject)
			return
		}
		if data == nil {
			data = decodeBody(r, body)
		}
		dup, err := deliver(r.Context(), Event{Source: o.Name, ReceivedAt: now, Headers: headers, Data: data}, id)
		if o.Signature == "teams-hmac" && err == nil { // Teams shows whatever we answer, so a duplicate gets the same 200
			text := o.ReplyText
			if text == "" {
				text = "Received." // an empty text may show as an empty bubble or an error in Teams
			}
			reply, _ := json.Marshal(struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{"message", text}) // field order: type first
			w.Header().Set("Content-Type", "application/json")
			w.Write(reply)
		} else if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		} else if dup {
			w.WriteHeader(http.StatusConflict)
		} else if o.ReplyText != "" {
			reply, _ := json.Marshal(map[string]string{"text": o.ReplyText})
			w.Header().Set("Content-Type", "application/json")
			w.Write(reply)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
	})
}

var egOrigin = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// serveEventGrid handles an authenticated Azure Event Grid request: the
// OPTIONS abuse-protection handshake, the validation event, and delivery of
// each event of a batch (or of a single CloudEvent) keyed by its own id.
// validationUrl is never fetched.
func serveEventGrid(w http.ResponseWriter, r *http.Request, o WebhookOptions, now time.Time, body []byte, headers map[string]string, deliver Deliver, reject func(int, string)) {
	if r.Method == http.MethodOptions {
		origin := r.Header.Get("WebHook-Request-Origin")
		if !egOrigin.MatchString(origin) {
			reject(http.StatusBadRequest, "origin missing or invalid")
			return
		}
		w.Header().Set("WebHook-Allowed-Origin", origin)
		w.Header().Set("WebHook-Allowed-Rate", "*")
		return
	}
	data, err := DecodeJSON(body)
	if err != nil {
		reject(http.StatusBadRequest, "body is not JSON")
		return
	}
	list, ok := data.([]any)
	if !ok {
		list = []any{data}
	}
	events := make([]map[string]any, len(list))
	for i, x := range list {
		m, _ := x.(map[string]any)
		if id, _ := m["id"].(string); id == "" {
			reject(http.StatusBadRequest, "event without id")
			return
		}
		events[i] = m
	}
	if len(events) == 0 {
		reject(http.StatusBadRequest, "empty batch")
		return
	}
	if t, _ := events[0]["eventType"].(string); len(events) == 1 && t == "Microsoft.EventGrid.SubscriptionValidationEvent" {
		d, _ := events[0]["data"].(map[string]any)
		code, _ := d["validationCode"].(string)
		if code == "" || len(code) > 128 {
			reject(http.StatusBadRequest, "validation code missing or too long")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		reply, _ := json.Marshal(map[string]string{"validationResponse": code})
		w.Write(reply)
		return
	}
	for _, m := range events {
		if _, err := deliver(r.Context(), Event{Source: o.Name, ReceivedAt: now, Headers: headers, Data: m}, m["id"].(string)); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
}

// servePubSub handles an authenticated Pub/Sub push: it unwraps the envelope
// and delivers the message keyed by its messageId. A duplicate gets 204 too,
// so Pub/Sub stops redelivering. The caller has already counted Limit.
func servePubSub(w http.ResponseWriter, r *http.Request, o WebhookOptions, now time.Time, body []byte, headers map[string]string, deliver Deliver, reject func(int, string)) {
	var env struct {
		Message struct {
			Data        string            `json:"data"`
			Attributes  map[string]string `json:"attributes"`
			MessageID   string            `json:"messageId"`
			PublishTime string            `json:"publishTime"`
		} `json:"message"`
		Subscription string `json:"subscription"`
	}
	if json.Unmarshal(body, &env) != nil || env.Message.MessageID == "" {
		reject(http.StatusBadRequest, "not a Pub/Sub push")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(env.Message.Data)
	if err != nil {
		reject(http.StatusBadRequest, "message data is not base64")
		return
	}
	var data any = string(raw)
	if j, err := DecodeJSON(raw); err == nil {
		data = j
	}
	ev := Event{Source: o.Name, ReceivedAt: now, Headers: headers, Data: map[string]any{
		"data": data, "attributes": env.Message.Attributes, "message_id": env.Message.MessageID,
		"publish_time": env.Message.PublishTime, "subscription": env.Subscription,
	}}
	if _, err := deliver(r.Context(), ev, env.Message.MessageID); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveDiscord handles an authenticated Discord interaction: PING and
// autocomplete are answered and not delivered; the rest are delivered keyed by
// the interaction id and acknowledged (or answered with reply_text) inside
// Discord's 3-second window. A duplicate gets the same response.
func serveDiscord(w http.ResponseWriter, r *http.Request, o WebhookOptions, now time.Time, body []byte, headers map[string]string, deliver Deliver, reject func(int, string)) {
	var v struct {
		Type *int    `json:"type"`
		ID   *string `json:"id"`
	}
	if json.Unmarshal(body, &v) != nil || v.Type == nil {
		reject(http.StatusBadRequest, "not an interaction")
		return
	}
	var resp any
	switch *v.Type {
	case 1: // PING
		resp = map[string]int{"type": 1}
	case 4: // autocomplete: no choices
		resp = map[string]any{"type": 8, "data": map[string]any{"choices": []string{}}}
	}
	if resp == nil {
		if v.ID == nil || *v.ID == "" {
			reject(http.StatusBadRequest, "interaction without id")
			return
		}
		if !o.Limit.Allow(now) {
			reject(http.StatusTooManyRequests, "rate limited")
			return
		}
		data, err := DecodeJSON(body)
		if err == nil {
			_, err = deliver(r.Context(), Event{Source: o.Name, ReceivedAt: now, Headers: headers, Data: data}, *v.ID)
		}
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch {
		case o.ReplyText != "":
			resp = map[string]any{"type": 4, "data": map[string]string{"content": o.ReplyText}}
		case *v.Type == 3:
			resp = map[string]int{"type": 6}
		default:
			resp = map[string]int{"type": 5}
		}
	}
	out, _ := json.Marshal(resp) // fixed shapes always encode
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

const matrixPrefix = "/_matrix/app/v1/"

// matrixError answers with a fixed Matrix error body; nothing from the request
// (least of all the token) is echoed.
func matrixError(w http.ResponseWriter, code int, errcode string) {
	if errcode == "" {
		errcode = map[int]string{400: "M_BAD_JSON", 401: "M_UNAUTHORIZED", 403: "M_FORBIDDEN", 404: "M_NOT_FOUND", 413: "M_TOO_LARGE", 429: "M_LIMIT_EXCEEDED"}[code]
	}
	if errcode == "" {
		errcode = "M_UNKNOWN"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, `{"errcode":"`+errcode+`"}`)
}

// serveMatrix handles an authenticated Matrix application-service request:
// transactions are delivered per event keyed by event_id (a re-sent
// transaction is acknowledged, its events not delivered twice), ping is
// answered, and user and room queries are declined.
func serveMatrix(w http.ResponseWriter, r *http.Request, o WebhookOptions, now time.Time, body []byte, headers map[string]string, deliver Deliver) {
	_, rest, _ := strings.Cut(r.URL.Path, matrixPrefix)
	ok := func() {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{}")
	}
	switch {
	case r.Method == http.MethodPost && rest == "ping":
		ok()
	case r.Method == http.MethodGet && (strings.HasPrefix(rest, "users/") || strings.HasPrefix(rest, "rooms/")):
		matrixError(w, http.StatusNotFound, "M_NOT_FOUND")
	case r.Method == http.MethodPut && strings.HasPrefix(rest, "transactions/") && len(rest) > len("transactions/") && !strings.Contains(rest[len("transactions/"):], "/"):
		var txn struct {
			Events []map[string]any `json:"events"`
		}
		if json.Unmarshal(body, &txn) != nil || txn.Events == nil {
			matrixError(w, http.StatusBadRequest, "M_BAD_JSON")
			return
		}
		for _, m := range txn.Events {
			if id, _ := m["event_id"].(string); id == "" {
				matrixError(w, http.StatusBadRequest, "M_BAD_JSON")
				return
			}
		}
		for _, m := range txn.Events {
			if _, err := deliver(r.Context(), Event{Source: o.Name, ReceivedAt: now, Headers: headers, Data: m}, m["event_id"].(string)); err != nil {
				matrixError(w, http.StatusInternalServerError, "M_UNKNOWN")
				return
			}
		}
		ok()
	default:
		matrixError(w, http.StatusNotFound, "M_UNRECOGNIZED")
	}
}

// decodeBody reads JSON first, so a JSON body sent with a form Content-Type
// (curl -d does that) stays JSON; otherwise a form body becomes fields (a
// string per single value, a []any per repeated key); anything else is raw.
func decodeBody(r *http.Request, body []byte) any {
	if data, err := DecodeJSON(body); err == nil {
		return data
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && mt == "application/x-www-form-urlencoded" {
		if q, err := url.ParseQuery(string(body)); err == nil {
			data := make(map[string]any, len(q))
			for k, v := range q {
				if len(v) == 1 {
					data[k] = v[0]
				} else {
					list := make([]any, len(v))
					for i, x := range v {
						list[i] = x
					}
					data[k] = list
				}
			}
			return data
		}
	}
	return map[string]any{"raw": string(body)}
}

// verifyStandard checks a Standard Webhooks delivery and returns its signed
// webhook-id, which is the replay key.
func verifyStandard(secret string, r *http.Request, body []byte, now time.Time) (string, bool) {
	id, ts := r.Header.Get("Webhook-Id"), r.Header.Get("Webhook-Timestamp")
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if secret == "" || id == "" || err != nil || now.Sub(time.Unix(seconds, 0)) > 5*time.Minute || time.Unix(seconds, 0).Sub(now) > 5*time.Minute {
		return "", false
	}
	key := []byte(secret)
	if b64, ok := strings.CutPrefix(secret, "whsec_"); ok {
		if key, err = base64.StdEncoding.DecodeString(b64); err != nil || len(key) == 0 {
			return "", false
		}
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	valid := false
	for _, part := range strings.Fields(r.Header.Get("Webhook-Signature")) {
		v, ok := strings.CutPrefix(part, "v1,")
		got, err := base64.StdEncoding.DecodeString(v)
		if ok && err == nil && hmac.Equal(got, want) {
			valid = true
		}
	}
	return id, valid
}

// macMatch reports whether any candidate (PREFIX+hex, or bare hex for the
// sha256 preset's optional prefix) is the HMAC-SHA256 of msg. Every candidate
// is compared, in constant time.
func macMatch(secret string, msg []byte, cands []string, prefix string) bool {
	if secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(msg)
	want := mac.Sum(nil)
	ok := false
	for _, c := range cands {
		c = strings.TrimSpace(c)
		if prefix == "sha256=" { // the sha256 preset has always taken the bare form too
			c = strings.TrimPrefix(c, prefix)
		} else if rest, found := strings.CutPrefix(c, prefix); found {
			c = rest
		} else {
			continue
		}
		if got, err := hex.DecodeString(c); err == nil && hmac.Equal(got, want) {
			ok = true
		}
	}
	return ok
}

// freshTS: a Unix-seconds timestamp within 5 minutes of now.
func freshTS(ts string, now time.Time) bool {
	seconds, err := strconv.ParseInt(ts, 10, 64)
	return err == nil && now.Sub(time.Unix(seconds, 0)) <= 5*time.Minute && time.Unix(seconds, 0).Sub(now) <= 5*time.Minute
}

// parseStripe reads "t=…,v1=…,v1=…"; other schemes (v0) are ignored.
func parseStripe(h string) (ts string, v1 []string) {
	for _, part := range strings.Split(h, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = append(v1, v)
		}
	}
	return
}

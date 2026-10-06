package source

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
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
	TimestampHeader string
	IDHeader        string
	MaxBody         int64
	Now             func() time.Time
}

type Deliver func(ctx context.Context, ev Event, deliveryID string) (dup bool, err error)

func NewWebhook(o WebhookOptions, deliver Deliver) http.Handler {
	if o.MaxBody <= 0 {
		o.MaxBody = 1 << 20
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Signature == "github" {
		o.TimestampHeader = ""
	}
	var mu sync.Mutex
	tokens := 20.0
	last := o.Now()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		now := o.Now()
		body, err := io.ReadAll(io.LimitReader(r.Body, o.MaxBody+1))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if int64(len(body)) > o.MaxBody {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		header := o.SigHeader
		if o.Signature == "github" {
			header = "X-Hub-Signature-256"
		}
		sig := r.Header.Get(header)
		if o.Signature == "github" {
			if !strings.HasPrefix(sig, "sha256=") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			sig = strings.TrimPrefix(sig, "sha256=")
		} else if o.Signature == "sha256" {
			sig = strings.TrimPrefix(sig, "sha256=")
		} else {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		provided, err := hex.DecodeString(sig)
		mac := hmac.New(sha256.New, []byte(o.Secret))
		signed := body
		if o.Signature == "sha256" && o.TimestampHeader != "" {
			signed = append(append([]byte(r.Header.Get(o.TimestampHeader)), '.'), body...)
		}
		mac.Write(signed)
		if o.Secret == "" || err != nil || !hmac.Equal(provided, mac.Sum(nil)) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if o.TimestampHeader != "" {
			seconds, err := strconv.ParseInt(r.Header.Get(o.TimestampHeader), 10, 64)
			if err != nil || now.Sub(time.Unix(seconds, 0)) > 5*time.Minute || time.Unix(seconds, 0).Sub(now) > 5*time.Minute {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		id := r.Header.Get(o.IDHeader)
		if o.IDHeader != "" && id == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		if elapsed := now.Sub(last).Seconds(); elapsed > 0 {
			tokens = min(20, tokens+elapsed*10)
			last = now
		}
		allowed := tokens >= 1
		if allowed {
			tokens--
		}
		mu.Unlock()
		if !allowed {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		// Without a signed timestamp, identical bodies replay only after the
		// seen_event TTL (7 days). Delivery IDs are not signed.
		sum := sha256.Sum256(signed)
		id = hex.EncodeToString(sum[:])
		headers := make(map[string]string, len(r.Header))
		for k := range r.Header {
			switch strings.ToLower(k) {
			case strings.ToLower(header), "x-hub-signature-256", "authorization", "cookie", "proxy-authorization", "connection", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade":
			default:
				headers[strings.ToLower(k)] = r.Header.Get(k)
			}
		}
		data, err := DecodeJSON(body)
		if err != nil {
			data = map[string]any{"raw": string(body)}
		}
		dup, err := deliver(r.Context(), Event{Source: o.Name, ReceivedAt: now, Headers: headers, Data: data}, id)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		} else if dup {
			w.WriteHeader(http.StatusConflict)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
	})
}

package source

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	mxBase = "/hook/m/_matrix/app/v1/"
	mxTxn  = `{"events":[{"event_id":"$a","type":"m.room.message"},{"event_id":"$b","type":"m.room.message"}]}`
)

var mxSecret = "hs" + "_tok_example" // not a real token

type mxReq struct {
	method, path, body, auth string
}

func mxCall(t *testing.T, o WebhookOptions, seen map[string]bool, q mxReq) (*httptest.ResponseRecorder, []Event) {
	t.Helper()
	var got []Event
	o.Name, o.Secret, o.Signature, o.TokenHeader = "m", mxSecret, "token", "Authorization"
	if o.Format == "" {
		o.Format = "matrix"
	}
	h := NewWebhook(o, func(_ context.Context, ev Event, id string) (bool, error) {
		if seen[id] {
			return true, nil
		}
		seen[id] = true
		got = append(got, ev)
		return false, nil
	})
	r := httptest.NewRequest(q.method, q.path, strings.NewReader(q.body))
	if q.auth != "" {
		r.Header.Set("Authorization", q.auth)
	}
	r.Header.Set("X-Other", "kept")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w, got
}

func TestMatrixWebhook(t *testing.T) {
	good := "Bearer " + mxSecret
	put := func(auth string) mxReq { return mxReq{"PUT", mxBase + "transactions/1", mxTxn, auth} }
	seen := map[string]bool{}
	w, got := mxCall(t, WebhookOptions{}, seen, put(good))
	if w.Code != 200 || w.Body.String() != "{}" || len(got) != 2 || got[0].Data.(map[string]any)["event_id"] != "$a" {
		t.Fatal(w.Code, w.Body.String(), got)
	}
	for _, e := range got {
		if _, has := e.Headers["authorization"]; has || e.Headers["x-other"] != "kept" {
			t.Fatal(e.Headers)
		}
	}
	if w, got = mxCall(t, WebhookOptions{}, seen, put(good)); w.Code != 200 || len(got) != 0 {
		t.Fatal("resend", w.Code, got)
	}

	for _, tc := range []struct {
		name string
		o    WebhookOptions
		q    mxReq
		code int
		body string
	}{
		{"no bearer", WebhookOptions{}, put(""), 401, "M_UNAUTHORIZED"},
		{"wrong bearer", WebhookOptions{}, put("Bearer nope"), 403, "M_FORBIDDEN"},
		{"bare secret", WebhookOptions{}, put(mxSecret), 403, "M_FORBIDDEN"},
		{"query refused", WebhookOptions{}, mxReq{"PUT", mxBase + "transactions/1?access_token=" + mxSecret, mxTxn, ""}, 401, "M_UNAUTHORIZED"},
		{"query allowed", WebhookOptions{AllowQueryToken: true}, mxReq{"PUT", mxBase + "transactions/1?access_token=" + mxSecret, mxTxn, ""}, 200, "{}"},
		{"query wrong", WebhookOptions{AllowQueryToken: true}, mxReq{"PUT", mxBase + "transactions/1?access_token=x", mxTxn, ""}, 403, "M_FORBIDDEN"},
		{"ping", WebhookOptions{}, mxReq{"POST", mxBase + "ping", `{"transaction_id":"x"}`, good}, 200, "{}"},
		{"user query", WebhookOptions{}, mxReq{"GET", mxBase + "users/@a:b", "", good}, 404, "M_NOT_FOUND"},
		{"room query", WebhookOptions{}, mxReq{"GET", mxBase + "rooms/%23a:b", "", good}, 404, "M_NOT_FOUND"},
		{"unknown path", WebhookOptions{}, mxReq{"GET", mxBase + "other", "", good}, 404, "M_UNRECOGNIZED"},
		{"bad json", WebhookOptions{}, mxReq{"PUT", mxBase + "transactions/1", "{", good}, 400, "M_BAD_JSON"},
		{"no events", WebhookOptions{}, mxReq{"PUT", mxBase + "transactions/1", `{}`, good}, 400, "M_BAD_JSON"},
		{"no event_id", WebhookOptions{}, mxReq{"PUT", mxBase + "transactions/1", `{"events":[{}]}`, good}, 400, "M_BAD_JSON"},
		{"empty events", WebhookOptions{}, mxReq{"PUT", mxBase + "transactions/1", `{"events":[]}`, good}, 200, "{}"},
		{"plain path on matrix source", WebhookOptions{}, mxReq{"POST", "/hook/m", `{}`, good}, 404, ""},
		{"non-matrix source", WebhookOptions{Format: "-"}, put(good), 404, ""},
		{"PUT elsewhere is refused", WebhookOptions{Format: "-"}, mxReq{"PUT", "/hook/m", `{}`, good}, 405, ""},
		{"rate limited", WebhookOptions{Limit: NewLimiter(0, 0)}, put(good), 429, "M_LIMIT_EXCEEDED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, got := mxCall(t, tc.o, map[string]bool{}, tc.q)
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatal(w.Code, w.Body.String())
			}
			if tc.code != 200 && len(got) > 1 || strings.Contains(w.Body.String(), mxSecret) {
				t.Fatal(got, w.Body.String())
			}
		})
	}
}

func TestMatrixNoTimeDependence(t *testing.T) {
	// Now is injected so the limiter is not on the real clock.
	now := time.Unix(1_800_000_000, 0)
	w, _ := mxCall(t, WebhookOptions{Now: func() time.Time { return now }}, map[string]bool{}, mxReq{"PUT", mxBase + "transactions/1", mxTxn, "Bearer " + mxSecret})
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
}

func TestMatrixBatchOverBurst(t *testing.T) {
	var evs []string
	for i := 0; i < 50; i++ {
		evs = append(evs, fmt.Sprintf(`{"event_id":"$e%d"}`, i))
	}
	txn := `{"events":[` + strings.Join(evs, ",") + `]}`
	seen := map[string]bool{}
	o := WebhookOptions{Limit: NewLimiter(20, 0)}
	q := mxReq{"PUT", mxBase + "transactions/1", txn, "Bearer " + mxSecret}
	if w, got := mxCall(t, o, seen, q); w.Code != 200 || len(got) != 50 {
		t.Fatal(w.Code, len(got))
	}
	o.Limit = NewLimiter(20, 0)
	if w, got := mxCall(t, o, seen, q); w.Code != 200 || len(got) != 0 {
		t.Fatal("resend", w.Code, len(got))
	}
}

package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// egCall sends one request to an eventgrid handler and returns the status, the
// recorder and the delivered events (ids in order).
func egCall(t *testing.T, o WebhookOptions, method, file string, hdr map[string]string, seen map[string]bool, fail error) (*httptest.ResponseRecorder, []Event) {
	t.Helper()
	body := ""
	if file != "" {
		b, err := os.ReadFile("testdata/eventgrid/" + file)
		if err != nil {
			t.Fatal(err)
		}
		body = string(b)
	}
	var got []Event
	o.Name, o.Secret, o.Signature, o.TokenHeader, o.Format = "eg", "k", "token", "X-Siphon-Key", "eventgrid"
	h := NewWebhook(o, func(_ context.Context, ev Event, id string) (bool, error) {
		if fail != nil {
			return false, fail
		}
		if seen[id] {
			return true, nil
		}
		if seen != nil {
			seen[id] = true
		}
		got = append(got, ev)
		return false, nil
	})
	r := httptest.NewRequest(method, "/hook/eg", strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w, got
}

var egAuth = map[string]string{"X-Siphon-Key": "k"}

func TestEventGridValidation(t *testing.T) {
	w, got := egCall(t, WebhookOptions{}, "POST", "validation.json", egAuth, nil, nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"validationResponse":"512d38b6-c7b8-40c8-89fe-f46f9e9622b6"}` || len(got) != 0 {
		t.Errorf("validation: %d %q delivered %d", w.Code, w.Body.String(), len(got))
	}
	if w, _ := egCall(t, WebhookOptions{}, "POST", "validation.json", nil, nil, nil); w.Code != 401 || w.Body.Len() != 0 {
		t.Errorf("no token: %d %q", w.Code, w.Body.String())
	}
}

func TestEventGridOptions(t *testing.T) {
	hdr := map[string]string{"X-Siphon-Key": "k", "WebHook-Request-Origin": "eventgrid.azure.net"}
	w, got := egCall(t, WebhookOptions{}, "OPTIONS", "", hdr, nil, nil)
	if w.Code != 200 || w.Header().Get("WebHook-Allowed-Origin") != "eventgrid.azure.net" || w.Header().Get("WebHook-Allowed-Rate") != "*" || len(got) != 0 {
		t.Errorf("options: %d %v", w.Code, w.Header())
	}
	if w, _ := egCall(t, WebhookOptions{}, "OPTIONS", "", map[string]string{"WebHook-Request-Origin": "eventgrid.azure.net"}, nil, nil); w.Code != 401 {
		t.Errorf("options without token: %d", w.Code)
	}
	for _, origin := range []string{"", "https://evil.example", "a.com/x", "a b"} {
		if w, _ := egCall(t, WebhookOptions{}, "OPTIONS", "", map[string]string{"X-Siphon-Key": "k", "WebHook-Request-Origin": origin}, nil, nil); w.Code != 400 || w.Header().Get("WebHook-Allowed-Origin") != "" {
			t.Errorf("origin %q: %d", origin, w.Code)
		}
	}
	// Other sources refuse OPTIONS.
	h := NewWebhook(WebhookOptions{Secret: "k", Signature: "token", TokenHeader: "X-Siphon-Key"}, nil)
	r := httptest.NewRequest("OPTIONS", "/hook/x", nil)
	r.Header.Set("X-Siphon-Key", "k")
	r.Header.Set("WebHook-Request-Origin", "eventgrid.azure.net")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r)
	if w2.Code != http.StatusMethodNotAllowed {
		t.Errorf("non-eventgrid OPTIONS: %d", w2.Code)
	}
}

func TestEventGridDelivery(t *testing.T) {
	seen := map[string]bool{}
	w, got := egCall(t, WebhookOptions{}, "POST", "batch.json", egAuth, seen, nil)
	if w.Code != 200 || len(got) != 2 {
		t.Fatalf("batch: %d delivered %d", w.Code, len(got))
	}
	if m := got[1].Data.(map[string]any); m["id"] != "831e1650-001e-001b-66ab-eeb76e069632" || got[0].Headers["x-siphon-key"] != "" {
		t.Errorf("second event %v, headers %v", m, got[0].Headers)
	}
	w, got = egCall(t, WebhookOptions{}, "POST", "batch.json", egAuth, seen, nil)
	if w.Code != 200 || len(got) != 0 {
		t.Errorf("re-sent: %d delivered %d", w.Code, len(got))
	}
	if w, got := egCall(t, WebhookOptions{}, "POST", "cloudevent.json", egAuth, nil, nil); w.Code != 200 || len(got) != 1 {
		t.Errorf("single CloudEvent: %d delivered %d", w.Code, len(got))
	}
	if w, got := egCall(t, WebhookOptions{}, "POST", "cloudevent-batch.json", egAuth, nil, nil); w.Code != 200 || len(got) != 2 {
		t.Errorf("CloudEvents batch: %d delivered %d", w.Code, len(got))
	}
}

func TestEventGridRefusals(t *testing.T) {
	// An element without an id refuses the whole request before any delivery.
	w, got := egCall(t, WebhookOptions{}, "POST", "noid.json", egAuth, nil, nil)
	if w.Code != 400 || len(got) != 0 {
		t.Errorf("no id: %d delivered %d", w.Code, len(got))
	}
	// A non-object element refuses the whole request too.
	if w, got := egCall(t, WebhookOptions{}, "POST", "nonobject.json", egAuth, nil, nil); w.Code != 400 || len(got) != 0 {
		t.Errorf("non-object element: %d delivered %d", w.Code, len(got))
	}
	if w, _ := egCall(t, WebhookOptions{}, "POST", "batch.json", egAuth, nil, errors.New("db down")); w.Code != 500 {
		t.Errorf("deliver error: %d", w.Code)
	}
	w, got = egCall(t, WebhookOptions{Limit: NewLimiter(0, 0)}, "POST", "batch.json", egAuth, nil, nil)
	if w.Code != 429 || len(got) != 0 {
		t.Errorf("limiter: %d delivered %d", w.Code, len(got))
	}
}

func TestEventGridBatchOverBurst(t *testing.T) {
	var evs []string
	for i := 0; i < 50; i++ {
		evs = append(evs, fmt.Sprintf(`{"id":"e%d","eventType":"x"}`, i))
	}
	n := 0
	h := NewWebhook(WebhookOptions{Name: "eg", Secret: "k", Signature: "token", TokenHeader: "X-Siphon-Key", Format: "eventgrid", Limit: NewLimiter(20, 0)},
		func(context.Context, Event, string) (bool, error) { n++; return false, nil })
	r := httptest.NewRequest("POST", "/hook/eg", strings.NewReader("["+strings.Join(evs, ",")+"]"))
	r.Header.Set("X-Siphon-Key", "k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || n != 50 {
		t.Fatal(w.Code, n)
	}
}

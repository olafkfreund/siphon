package source

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPPoll(t *testing.T) {
	s := HTTP{Options: HTTPOptions{Name: "local", URL: "http://example.test"}, transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"id":1700000000}`))}, nil
	})}
	ev, err := s.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ev.Source != "local" || ev.Data.(map[string]any)["ok"] != true || ev.Data.(map[string]any)["id"] != int64(1700000000) {
		t.Fatalf("%+v", ev)
	}
	s.Options.MaxBody = 2
	if _, err := s.Poll(context.Background()); err == nil {
		t.Fatal("oversized body accepted")
	}
}

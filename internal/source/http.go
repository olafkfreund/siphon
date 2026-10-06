package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Event struct {
	Source     string
	ReceivedAt time.Time
	Headers    map[string]string
	Data       any
}

type HTTPOptions struct {
	Name         string
	URL          string
	Method       string
	Headers      map[string]string
	Body         []byte
	AllowPrivate bool
	MaxBody      int64
	Timeout      time.Duration
}

type HTTP struct {
	Options   HTTPOptions
	transport http.RoundTripper
}

func (s HTTP) Poll(ctx context.Context) (Event, error) {
	o := s.Options
	method := o.Method
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost {
		return Event{}, fmt.Errorf("unsupported HTTP method %q", method)
	}
	limit := o.MaxBody
	if limit <= 0 {
		limit = 1 << 20
	}
	req, err := http.NewRequestWithContext(ctx, method, o.URL, bytes.NewReader(o.Body))
	if err != nil {
		return Event{}, err
	}
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	client := guardedClient(o.AllowPrivate, o.Timeout, limit)
	if s.transport != nil {
		client.Transport = s.transport
	}
	resp, err := client.Do(req)
	if err != nil {
		return Event{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Event{}, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Event{}, err
	}
	if int64(len(body)) > limit {
		return Event{}, fmt.Errorf("HTTP body exceeds %d bytes", limit)
	}
	var data any
	if json.Unmarshal(body, &data) != nil {
		data = map[string]any{"text": string(body)}
	}
	headers := make(map[string]string, len(resp.Header))
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}
	return Event{Source: o.Name, ReceivedAt: time.Now(), Headers: headers, Data: data}, nil
}

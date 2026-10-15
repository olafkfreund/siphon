package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type MCPOptions struct {
	Name         string
	Command      []string
	Env          map[string]string // added to the stdio child's minimal env, never argv
	URL          string
	Bearer       string
	OAuth        auth.OAuthHandler // steady-state OAuth login (excludes Bearer)
	Resource     string
	Tool         string
	ToolArgs     map[string]any
	AllowPrivate bool
	MaxBody      int64
	Timeout      time.Duration
}

type MCP struct {
	Options   MCPOptions
	Transport mcp.Transport // optional in-process transport for tests
}

var ErrListenUnsupported = errors.New("MCP resource subscriptions unsupported")

func (s MCP) Poll(ctx context.Context) (Event, error) {
	o := s.Options
	if (o.Resource == "") == (o.Tool == "") {
		return Event{}, errors.New("declare exactly one resource or tool")
	}
	if s.Transport == nil && (len(o.Command) == 0) == (o.URL == "") {
		return Event{}, errors.New("declare exactly one command or URL")
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	transport := s.transport(ctx, false)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "siphon", Version: "1"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		return Event{}, err
	}
	defer session.Close()
	var data any
	if o.Resource != "" {
		result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: o.Resource})
		if err != nil {
			return Event{}, err
		}
		if len(result.Contents) == 0 {
			return Event{}, errors.New("empty MCP resource")
		}
		values := make([]any, 0, len(result.Contents))
		for _, c := range result.Contents {
			values = append(values, decodeContent(c.Text, c.Blob))
		}
		data = oneOrMany(values)
	} else {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: o.Tool, Arguments: o.ToolArgs})
		if err != nil {
			return Event{}, err
		}
		if result.IsError {
			return Event{}, fmt.Errorf("MCP tool %s returned an error: %s", o.Tool, toolErrText(result.Content))
		}
		// Prefer the TextContent copy of structured output (the MCP spec says a
		// tool SHOULD send both, and go-sdk does): it decodes exactly, whereas
		// the SDK has already turned StructuredContent numbers into float64.
		if text, ok := jsonText(result.Content); ok {
			data = text
		} else if result.StructuredContent != nil {
			// ponytail: lossy above 2^53; only hit when a tool omits the text copy.
			encoded, err := json.Marshal(result.StructuredContent)
			if err != nil {
				return Event{}, err
			}
			if data, err = DecodeJSON(encoded); err != nil {
				return Event{}, err
			}
		} else {
			values := make([]any, 0, len(result.Content))
			for _, c := range result.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					values = append(values, decodeContent(t.Text, nil))
				}
			}
			if len(values) == 0 {
				return Event{}, errors.New("MCP tool returned no text content")
			}
			data = oneOrMany(values)
		}
	}
	return Event{Source: o.Name, ReceivedAt: time.Now(), Headers: map[string]string{}, Data: data}, nil
}

func (s MCP) Listen(ctx context.Context, onChange func()) error {
	if s.Options.Tool != "" || s.Options.Resource == "" {
		return ErrListenUnsupported
	}
	if s.Transport == nil && (len(s.Options.Command) == 0) == (s.Options.URL == "") {
		return errors.New("declare exactly one command or URL")
	}
	timeout := s.Options.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "siphon", Version: "1"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			if req.Params != nil && req.Params.URI == s.Options.Resource {
				onChange()
			}
		},
	})
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	transport := &deadlineTransport{Transport: s.transport(ctx, true)}
	session, err := client.Connect(connectCtx, transport, nil)
	if transport.stop != nil {
		transport.stop()
	}
	cancel()
	if err != nil {
		return err
	}
	defer session.Close()
	caps := session.InitializeResult().Capabilities
	if caps == nil || caps.Resources == nil || !caps.Resources.Subscribe {
		return ErrListenUnsupported
	}
	subscribeCtx, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(subscribeCtx, func() { transport.conn.Close() })
	err = session.Subscribe(subscribeCtx, &mcp.SubscribeParams{URI: s.Options.Resource})
	stop()
	cancel()
	if err != nil {
		return err
	}
	onChange()
	// Wait ignores ctx: close the session on cancellation, or serve hangs on SIGTERM.
	defer context.AfterFunc(ctx, func() { session.Close() })()
	err = session.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

type deadlineTransport struct {
	mcp.Transport
	conn mcp.Connection
	stop func() bool
}

func (t *deadlineTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.Transport.Connect(ctx)
	if err == nil {
		t.conn = conn
		t.stop = context.AfterFunc(ctx, func() { conn.Close() })
	}
	return conn, err
}

func (s MCP) transport(ctx context.Context, stream bool) mcp.Transport {
	o := s.Options
	transport := s.Transport
	if transport == nil && len(o.Command) > 0 {
		cmd := exec.CommandContext(ctx, o.Command[0], o.Command[1:]...)
		cmd.Env = []string{}
		for _, name := range []string{"PATH", "HOME", "LANG"} {
			if value, ok := os.LookupEnv(name); ok {
				cmd.Env = append(cmd.Env, name+"="+value)
			}
		}
		for _, k := range slices.Sorted(maps.Keys(o.Env)) {
			cmd.Env = append(cmd.Env, k+"="+o.Env[k])
		}
		transport = &mcp.CommandTransport{Command: cmd}
	}
	if transport == nil {
		var client *http.Client
		client = guardedClient(o.AllowPrivate, o.Timeout, o.MaxBody, stream)
		if o.Bearer != "" && o.OAuth == nil {
			client.Transport = HeaderTransport{client.Transport, map[string]string{"Authorization": "Bearer " + o.Bearer}}
		}
		if o.OAuth != nil {
			client.Transport = RedirectChallenge(client.Transport, o.URL)
		}
		transport = &mcp.StreamableClientTransport{Endpoint: o.URL, HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: !stream, OAuthHandler: o.OAuth}
	}
	return transport
}

func decodeContent(text string, blob []byte) any {
	if blob != nil {
		text = string(blob)
	}
	value, err := DecodeJSON([]byte(text))
	if err == nil {
		return value
	}
	return map[string]any{"text": text}
}

func oneOrMany(values []any) any {
	if len(values) == 1 {
		return values[0]
	}
	return values
}

// HeaderTransport sets Header on every request (a nil Base is the default transport).
type HeaderTransport struct {
	Base   http.RoundTripper
	Header map[string]string
}

func (t HeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.Header {
		r.Header.Set(k, v)
	}
	if t.Base == nil {
		return http.DefaultTransport.RoundTrip(r)
	}
	return t.Base.RoundTrip(r)
}

type redirectChallenge struct {
	base     http.RoundTripper
	endpoint *url.URL
}

// RedirectChallenge turns a 3xx from the endpoint itself (a browser login
// page, as ServiceNow's MCP server sends) into a 401 challenge, so the SDK's
// OAuth handler runs. Other requests and responses pass through.
func RedirectChallenge(base http.RoundTripper, endpoint string) http.RoundTripper {
	u, err := url.Parse(endpoint)
	if err != nil {
		return base
	}
	u.RawQuery, u.Fragment = "", ""
	return redirectChallenge{base: base, endpoint: u}
}

func (t redirectChallenge) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode < 300 || resp.StatusCode > 399 {
		return resp, err
	}
	u := *req.URL
	u.RawQuery, u.Fragment = "", ""
	if u != *t.endpoint {
		return resp, nil
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	prm := *t.endpoint
	prm.Path = "/.well-known/oauth-protected-resource" + prm.Path
	prm.RawPath = ""
	return &http.Response{
		Status:     "401 Unauthorized",
		StatusCode: http.StatusUnauthorized,
		Proto:      resp.Proto, ProtoMajor: resp.ProtoMajor, ProtoMinor: resp.ProtoMinor,
		Header: http.Header{
			"Www-Authenticate":  {`Bearer resource_metadata="` + prm.String() + `"`},
			"Siphon-Redirected": {"1"},
		},
		Body:    http.NoBody,
		Request: req,
	}, nil
}

// jsonText returns the single TextContent of a tool result decoded as JSON
// (integers kept exact), if there is exactly one and it parses.
func jsonText(content []mcp.Content) (any, bool) {
	if len(content) != 1 {
		return nil, false
	}
	t, ok := content[0].(*mcp.TextContent)
	if !ok {
		return nil, false
	}
	v, err := DecodeJSON([]byte(t.Text))
	return v, err == nil
}

// toolErrText joins the TextContent of an error result with "; ", capped at
// 4 KiB on a rune boundary. The caller masks it before clipping it for display,
// so a secret is never cut in half before Mask sees it.
func toolErrText(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "; "))
	if text == "" {
		return "(no message)"
	}
	return ClipRunes(text, 4096)
}

// ClipRunes cuts s to at most n bytes on a rune boundary, adding "…" if it cut.
func ClipRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// OneLine collapses whitespace and clips to n bytes: for a masked error shown
// in one line (a source status or a log line).
func OneLine(s string, n int) string { return ClipRunes(strings.Join(strings.Fields(s), " "), n) }

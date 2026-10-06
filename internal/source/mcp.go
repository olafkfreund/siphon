package source

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type MCPOptions struct {
	Name         string
	Command      []string
	URL          string
	Bearer       string
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
	session, err := mcp.NewClient(&mcp.Implementation{Name: "agentgw", Version: "1"}, nil).Connect(ctx, transport, nil)
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
			return Event{}, fmt.Errorf("MCP tool %q returned an error", o.Tool)
		}
		if result.StructuredContent != nil {
			data = result.StructuredContent
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

// Listen reports changes to the declared resource until the context ends or the session fails.
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
	client := mcp.NewClient(&mcp.Implementation{Name: "agentgw", Version: "1"}, &mcp.ClientOptions{
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
	return session.Wait()
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
		transport = &mcp.CommandTransport{Command: cmd}
	}
	if transport == nil {
		var client *http.Client
		client = guardedClient(o.AllowPrivate, o.Timeout, o.MaxBody, stream)
		if o.Bearer != "" {
			client.Transport = bearerTransport{base: client.Transport, token: o.Bearer}
		}
		transport = &mcp.StreamableClientTransport{Endpoint: o.URL, HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: !stream}
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

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(copy)
}

package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	transport := s.Transport
	if transport == nil && len(o.Command) > 0 {
		transport = &mcp.CommandTransport{Command: exec.CommandContext(ctx, o.Command[0], o.Command[1:]...)}
	}
	if transport == nil {
		client := guardedClient(o.AllowPrivate, o.Timeout, o.MaxBody)
		if o.Bearer != "" {
			client.Transport = bearerTransport{base: client.Transport, token: o.Bearer}
		}
		transport = &mcp.StreamableClientTransport{Endpoint: o.URL, HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true}
	}
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

func decodeContent(text string, blob []byte) any {
	if blob != nil {
		text = string(blob)
	}
	var value any
	if json.Unmarshal([]byte(text), &value) == nil {
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

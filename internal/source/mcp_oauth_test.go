package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

var errLogin = errors.New("OAuth login required: siphon connect oauth s")

// fakeOAuth is a steady-state handler: no token means Authorize fails at once.
type fakeOAuth struct{ tok string }

func (f fakeOAuth) TokenSource(context.Context) (oauth2.TokenSource, error) {
	if f.tok == "" {
		return nil, nil
	}
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: f.tok}), nil
}

func (fakeOAuth) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	resp.Body.Close()
	return errLogin
}

func TestMCPOAuthPoll(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "r", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"ok":true}`}}}, nil
	})
	mcpH := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://x/.well-known/oauth-protected-resource"`)
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		mcpH.ServeHTTP(w, r)
	}))
	defer ts.Close()
	poll := func(h fakeOAuth) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := MCP{Options: MCPOptions{URL: ts.URL, Tool: "r", AllowPrivate: true, OAuth: h}}.Poll(ctx)
		return err
	}
	start := time.Now()
	if err := poll(fakeOAuth{}); !errors.Is(err, errLogin) || time.Since(start) > 5*time.Second {
		t.Fatalf("no login: want a fast login error, got %v after %v", err, time.Since(start))
	}
	if err := poll(fakeOAuth{tok: "good"}); err != nil {
		t.Fatalf("with a login: %v", err)
	}
}

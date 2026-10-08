package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/olafkfreund/siphon/internal/config"
	"github.com/olafkfreund/siphon/internal/source"
)

// modelPreset is a tile on the Connections page. Presets only pre-fill the
// base URL; every one but Ollama speaks the OpenAI-compatible API.
type modelPreset struct {
	ID, Label, Sub, URL, Mono string
	Key                       bool // usually needs an API key
}

var modelPresets = []modelPreset{
	{"ollama", "Ollama", "Local or LAN models", "http://127.0.0.1:11434", "O", false},
	{"lmstudio", "LM Studio", "Local server", "http://127.0.0.1:1234/v1", "LM", false},
	{"openrouter", "OpenRouter", "Many hosted models", "https://openrouter.ai/api/v1", "OR", true},
	{"groq", "Groq", "Fast hosted inference", "https://api.groq.com/openai/v1", "G", true},
	{"mistral", "Mistral", "Hosted Mistral models", "https://api.mistral.ai/v1", "M", true},
	{"openai", "OpenAI-compatible", "Any other endpoint", "", "{ }", true},
}

func presetByID(id string) (modelPreset, bool) {
	for _, p := range modelPresets {
		if p.ID == id {
			return p, true
		}
	}
	return modelPreset{}, false
}

// addModelConn saves an ollama/openai connection through the normal edit path,
// so validation (URL, private endpoint allowlist) and the write-only key
// handling are the same as for every other portal edit.
func (s *server) addModelConn(actor, name, presetID, rawURL, key string) error {
	p, ok := presetByID(presetID)
	if !ok || !itemName.MatchString(name) {
		return errInvalid{"choose a provider and a name (letters, digits, . _ -)"}
	}
	if rawURL = strings.TrimSpace(rawURL); rawURL == "" {
		rawURL = p.URL
	}
	if _, _, err := config.ModelURL(rawURL); err != nil {
		return errInvalid{"URL " + err.Error()}
	}
	provider := "openai"
	if p.ID == "ollama" {
		provider = "ollama"
	}
	y := fmt.Sprintf("provider: %s\nurl: %q\n", provider, rawURL)
	if provider == "openai" && p.ID != "openai" {
		y += "preset: " + p.ID + "\n"
	}
	var pending []pendingSecret
	if key = strings.TrimSpace(key); key != "" {
		if provider == "ollama" {
			return errInvalid{"Ollama needs no API key"}
		}
		ps := pendingSecret{Kind: "credentials", Name: name, Key: "api_key", Value: key}
		y += "api_key: file:" + ps.path(secretsDir(s.Config().Server.DB)) + "\n"
		pending = append(pending, ps)
	}
	_, _, _, err := s.commit(actor, "credentials/"+name+" model connection saved", nil, putItem("credentials", name, y), pending)
	if err == nil {
		modelCache.forget(name)
	}
	return err
}

// connTest is the result of listing a connection's models.
type connTest struct {
	Name    string    `json:"name"`
	Models  []string  `json:"models"`
	Latency string    `json:"latency,omitempty"`
	Err     string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

// modelCache keeps each connection's model list for a minute.
var modelCache = &mcache{m: map[string]connTest{}}

type mcache struct {
	mu sync.Mutex
	m  map[string]connTest
}

func (c *mcache) get(name string, now time.Time) (connTest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[name]
	return t, ok && now.Sub(t.At) < time.Minute
}

func (c *mcache) put(t connTest) { c.mu.Lock(); c.m[t.Name] = t; c.mu.Unlock() }

func (c *mcache) clear() { c.mu.Lock(); c.m = map[string]connTest{}; c.mu.Unlock() }

func (c *mcache) forget(name string) { c.mu.Lock(); delete(c.m, name); c.mu.Unlock() }

// listModels asks a connection for its models. It runs in the daemon, so it
// goes through the same guard as sources: public addresses only, unless the
// exact host:port is a listed private endpoint (and even then never
// link-local/metadata). It dials the checked address, never re-resolving.
// Only model ids leave this function: no bodies, no keys.
func (s *server) listModels(ctx context.Context, name string, fresh bool) connTest {
	now := s.Now()
	if !fresh {
		if t, ok := modelCache.get(name, now); ok {
			return t
		}
	}
	t := connTest{Name: name, At: now}
	cfg := s.Config()
	c := cfg.Credentials[name]
	if c == nil || (c.Provider != "ollama" && c.Provider != "openai") {
		t.Err = "not a model connection" // not cached: names are caller-chosen
		return t
	}
	defer func() { modelCache.put(t) }()
	host, port, err := config.ModelURL(c.URL)
	if err != nil {
		t.Err = "bad URL"
		return t
	}
	private := cfg.PrivateEndpoint(host, port)
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	mode := source.Public
	if private {
		mode = source.PrivateNoLinkLocal // listed endpoint: private yes, link-local/metadata never
	}
	addrs, err := source.ResolveAllowedMode(ctx, host, mode)
	if err != nil {
		t.Err = "this endpoint isn't reachable from Siphon: public addresses only, unless listed in server.models.private_endpoints"
		return t
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var last error
		for _, a := range addrs { // the checked addresses only
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), strconv.Itoa(port)))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}}
	defer tr.CloseIdleConnections()
	list := strings.TrimRight(c.URL, "/") + "/models"
	if c.Provider == "ollama" {
		list = strings.TrimRight(c.URL, "/") + "/api/tags"
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", list, nil)
	if c.APIKey.Value != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey.Value)
	}
	start := time.Now()
	resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Err = "no response (" + errClass(err) + ")"
		return t
	}
	defer resp.Body.Close()
	t.Latency = time.Since(start).Round(time.Millisecond).String()
	if resp.StatusCode != 200 {
		t.Err = "the endpoint answered " + resp.Status
		return t
	}
	var body struct {
		Data   []struct{ ID string }   `json:"data"`
		Models []struct{ Name string } `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		t.Err = "the answer isn't a model list"
		return t
	}
	for _, d := range body.Data {
		t.Models = append(t.Models, d.ID)
	}
	for _, m := range body.Models {
		t.Models = append(t.Models, m.Name)
	}
	sort.Strings(t.Models)
	if len(t.Models) > 200 {
		t.Models = t.Models[:200]
	}
	return t
}

// errClass names a network error without echoing addresses or bodies.
func errClass(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timed out"
	case strings.Contains(err.Error(), "refused"):
		return "connection refused"
	}
	return "network error"
}

// toolsHint marks model families known to handle tool calls well.
func toolsHint(model string) bool {
	m := strings.ToLower(model)
	for _, f := range []string{"qwen", "llama3", "mistral", "gpt-oss", "command-r"} {
		if strings.Contains(m, f) {
			return true
		}
	}
	return false
}

// modelConnView is a model connection card.
type modelConnView struct {
	Name, Provider, Preset, Host string
	Test                         *connTest
}

type modelForm struct{ Name, Preset, URL, Err string }

func (s *server) modelConns() []modelConnView {
	out := []modelConnView{}
	for name, c := range s.Config().Credentials {
		if c.Provider != "ollama" && c.Provider != "openai" {
			continue
		}
		v := modelConnView{Name: name, Provider: c.Provider, Preset: c.Preset}
		if v.Preset == "" {
			v.Preset = c.Provider
		}
		if h, p, err := config.ModelURL(c.URL); err == nil {
			v.Host = net.JoinHostPort(h, strconv.Itoa(p))
		}
		if t, ok := modelCache.get(name, s.Now()); ok {
			v.Test = &t
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *server) modelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /logins", func(w http.ResponseWriter, r *http.Request) { // renamed
		u := "/connections"
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, u, http.StatusMovedPermanently)
	})
	mux.HandleFunc("POST /connections/models", s.portal(func(w http.ResponseWriter, r *http.Request, csrf string) {
		name, preset, raw := r.PostFormValue("name"), r.PostFormValue("preset"), r.PostFormValue("url")
		err := s.addModelConn("portal", name, preset, raw, r.PostFormValue("api_key"))
		var inv errInvalid
		if errors.As(err, &inv) {
			v := s.connectionsView(r, csrf)
			v.ModelForm = &modelForm{Name: name, Preset: preset, URL: raw, Err: inv.msg}
			s.pageStatus(w, r, "logins", v, http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, "/connections?added="+name+"#models", http.StatusSeeOther)
	}))
	mux.HandleFunc("POST /connections/{name}/test", s.portal(func(w http.ResponseWriter, r *http.Request, _ string) {
		t := s.listModels(r.Context(), r.PathValue("name"), true)
		s.render(w, "conntest", t)
	}))
	mux.HandleFunc("GET /connections/models-for", s.portal(func(w http.ResponseWriter, r *http.Request, _ string) {
		name := r.URL.Query().Get("f.credential")
		if c := s.Config().Credentials[name]; c == nil || (c.Provider != "ollama" && c.Provider != "openai") {
			return // not a model connection: no suggestions
		}
		s.render(w, "modeloptions", s.listModels(r.Context(), name, false))
	}))
	mux.HandleFunc("GET /connections/{name}/models", s.portal(func(w http.ResponseWriter, r *http.Request, _ string) {
		s.render(w, "modeloptions", s.listModels(r.Context(), r.PathValue("name"), false))
	}))
}

// connectionsView fills the Connections page.
func (s *server) connectionsView(r *http.Request, csrf string) view {
	v := view{CSRF: csrf, Logins: s.logins(), Models: s.modelConns(), Presets: modelPresets,
		LoginForm: &loginForm{Kind: "token", Provider: "claude"}, ModelForm: &modelForm{Preset: "ollama"}}
	if n := r.URL.Query().Get("connect"); n != "" {
		if c := s.Config().Credentials[n]; c != nil && c.Provider != "ollama" && c.Provider != "openai" {
			v.LoginForm.Name, v.LoginForm.Provider = n, c.Provider
			if c.Provider != "claude" {
				v.LoginForm.Kind = "login"
			}
		}
	}
	if n := r.URL.Query().Get("added"); n != "" {
		v.LoginForm.Notice = n + " is connected."
	}
	return v
}
